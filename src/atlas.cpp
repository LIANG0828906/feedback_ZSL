#include "font_atlas/atlas.hpp"

#include "font_atlas/io.hpp"
#include "font_atlas/json.hpp"
#include "font_atlas/sha256.hpp"

#include <algorithm>
#include <cctype>
#include <map>
#include <sstream>

namespace font_atlas {
namespace {

Diagnostic overflow(const std::string& glyph, const std::string& message) {
    Diagnostic diagnostic;
    diagnostic.code = ErrorCode::AtlasOverflow;
    diagnostic.glyph = glyph;
    diagnostic.message = message;
    return diagnostic;
}

std::string safeStem(const std::string& glyph, std::size_t index) {
    std::string stem;
    for (unsigned char c : glyph) {
        if (std::isalnum(c) || c == '_' || c == '-') {
            stem.push_back(static_cast<char>(c));
        } else {
            stem.push_back('_');
        }
    }
    if (stem.empty()) {
        stem = "glyph";
    }
    stem += "_" + std::to_string(index) + "_" +
            Sha256::hex(glyph).substr(0, 8);
    return stem;
}

std::vector<std::uint8_t> pgmBytes(int width, int height,
                                   const std::vector<std::uint8_t>& pixels) {
    const std::string header =
        "P5\n" + std::to_string(width) + " " + std::to_string(height) + "\n255\n";
    std::vector<std::uint8_t> bytes(header.begin(), header.end());
    bytes.insert(bytes.end(), pixels.begin(), pixels.end());
    return bytes;
}

Json boundsJson(const BoundsD& bounds) {
    Json object = Json::object();
    object["x0"] = Json(bounds.x0);
    object["y0"] = Json(bounds.y0);
    object["x1"] = Json(bounds.x1);
    object["y1"] = Json(bounds.y1);
    return object;
}

Json metricsJson(const GlyphMetrics& metrics, const std::string& glyphFile,
                 const std::string& glyphHash) {
    Json object = Json::object();
    object["glyph"] = Json(metrics.glyph);
    object["pixelWidth"] = Json(metrics.pixelWidth);
    object["pixelHeight"] = Json(metrics.pixelHeight);
    object["atlasX"] = Json(metrics.atlasX);
    object["atlasY"] = Json(metrics.atlasY);
    object["offsetX"] = Json(metrics.atlasX);
    object["offsetY"] = Json(metrics.atlasY);
    object["bearingX"] = Json(metrics.bearingX);
    object["bearingY"] = Json(metrics.bearingY);
    object["advance"] = Json(metrics.advance);
    object["step"] = Json(metrics.advance);
    object["bbox"] = boundsJson(metrics.sourceBounds);
    object["file"] = Json(glyphFile);
    object["contentHash"] = Json(glyphHash);
    return object;
}

}  // namespace

Result<AtlasManifest> packAtlas(const std::vector<RasterResult>& glyphs,
                                const AtlasOptions& options,
                                const std::filesystem::path& outputDirectory) {
    if (options.width <= 0 || options.height <= 0 || options.padding < 0) {
        Diagnostic diagnostic;
        diagnostic.code = ErrorCode::InvalidArgument;
        diagnostic.message = "atlas dimensions must be positive and padding non-negative";
        return Result<AtlasManifest>::failure(std::move(diagnostic));
    }
    AtlasManifest manifest;
    manifest.width = options.width;
    manifest.height = options.height;
    manifest.padding = options.padding;
    manifest.imageFile = "atlas.pgm";

    std::vector<std::uint8_t> pixels(
        static_cast<std::size_t>(options.width) * options.height, 0);
    std::vector<GlyphMetrics> metrics;
    std::map<std::string, std::string> glyphFiles;
    std::map<std::string, std::string> glyphHashes;
    int cursorX = options.padding;
    int cursorY = options.padding;
    int rowHeight = 0;

    for (std::size_t i = 0; i < glyphs.size(); ++i) {
        const RasterResult& glyph = glyphs[i];
        if (glyph.width <= 0 || glyph.height <= 0 ||
            glyph.coverage.size() !=
                static_cast<std::size_t>(glyph.width) * glyph.height) {
            return Result<AtlasManifest>::failure(overflow(
                glyph.glyph, "glyph bitmap dimensions/content are invalid"));
        }
        if (glyph.width + 2 * options.padding > options.width ||
            glyph.height + 2 * options.padding > options.height) {
            return Result<AtlasManifest>::failure(overflow(
                glyph.glyph, "glyph does not fit inside atlas bounds"));
        }
        if (cursorX + glyph.width + options.padding > options.width) {
            cursorX = options.padding;
            cursorY += rowHeight + options.padding;
            rowHeight = 0;
        }
        if (cursorY + glyph.height + options.padding > options.height) {
            return Result<AtlasManifest>::failure(overflow(
                glyph.glyph, "atlas shelf layout exceeded atlas height"));
        }
        for (int row = 0; row < glyph.height; ++row) {
            std::copy(glyph.coverage.begin() +
                          static_cast<std::ptrdiff_t>(row * glyph.width),
                      glyph.coverage.begin() +
                          static_cast<std::ptrdiff_t>((row + 1) * glyph.width),
                      pixels.begin() +
                          static_cast<std::ptrdiff_t>((cursorY + row) * options.width +
                                                      cursorX));
        }
        GlyphMetrics glyphMetric = glyph.metrics;
        glyphMetric.glyph = glyph.glyph;
        glyphMetric.pixelWidth = glyph.width;
        glyphMetric.pixelHeight = glyph.height;
        glyphMetric.atlasX = cursorX;
        glyphMetric.atlasY = cursorY;
        metrics.push_back(glyphMetric);
        const std::string file = safeStem(glyph.glyph, i) + ".pgm";
        glyphFiles[glyph.glyph] = file;
        glyphHashes[glyph.glyph] = glyph.coverageHash;

        cursorX += glyph.width + options.padding;
        rowHeight = std::max(rowHeight, glyph.height);
    }

    auto directory = ensureDirectory(outputDirectory);
    if (!directory.ok()) {
        return Result<AtlasManifest>::failure(std::move(directory.error));
    }
    for (std::size_t i = 0; i < glyphs.size(); ++i) {
        const RasterResult& glyph = glyphs[i];
        auto write = writeFileAtomic(outputDirectory / glyphFiles[glyph.glyph],
                                     pgmBytes(glyph.width, glyph.height, glyph.coverage));
        if (!write.ok()) {
            return Result<AtlasManifest>::failure(std::move(write.error));
        }
    }
    const auto atlasBytes = pgmBytes(options.width, options.height, pixels);
    auto atlasWrite = writeFileAtomic(outputDirectory / manifest.imageFile, atlasBytes);
    if (!atlasWrite.ok()) {
        return Result<AtlasManifest>::failure(std::move(atlasWrite.error));
    }
    manifest.imageHash = Sha256::hex(atlasBytes.data(), atlasBytes.size());
    manifest.glyphs = metrics;

    Json root = Json::object();
    root["format"] = Json("font-atlas-manifest");
    root["version"] = Json(1);
    root["width"] = Json(manifest.width);
    root["height"] = Json(manifest.height);
    root["padding"] = Json(manifest.padding);
    root["imageFile"] = Json(manifest.imageFile);
    root["imageHash"] = Json(manifest.imageHash);
    Json glyphArray = Json::array();
    for (std::size_t i = 0; i < glyphs.size(); ++i) {
        const auto& glyph = glyphs[i];
        const auto& metric = metrics[i];
        glyphArray.pushBack(metricsJson(metric, glyphFiles[glyph.glyph],
                                       glyphHashes[glyph.glyph]));
    }
    root["glyphs"] = glyphArray;
    auto manifestWrite =
        writeFileAtomic(outputDirectory / "atlas.json", root.dump(2));
    if (!manifestWrite.ok()) {
        return Result<AtlasManifest>::failure(std::move(manifestWrite.error));
    }
    return Result<AtlasManifest>::success(std::move(manifest));
}

std::string atlasManifestJson(const AtlasManifest& manifest) {
    Json root = Json::object();
    root["width"] = Json(manifest.width);
    root["height"] = Json(manifest.height);
    root["padding"] = Json(manifest.padding);
    root["imageFile"] = Json(manifest.imageFile);
    root["imageHash"] = Json(manifest.imageHash);
    Json glyphArray = Json::array();
    for (const auto& glyph : manifest.glyphs) {
        Json item = Json::object();
        item["glyph"] = Json(glyph.glyph);
        item["pixelWidth"] = Json(glyph.pixelWidth);
        item["pixelHeight"] = Json(glyph.pixelHeight);
        item["atlasX"] = Json(glyph.atlasX);
        item["atlasY"] = Json(glyph.atlasY);
        item["bearingX"] = Json(glyph.bearingX);
        item["bearingY"] = Json(glyph.bearingY);
        item["advance"] = Json(glyph.advance);
        item["bbox"] = boundsJson(glyph.sourceBounds);
        glyphArray.pushBack(item);
    }
    root["glyphs"] = glyphArray;
    return root.dump(2);
}

}  // namespace font_atlas
