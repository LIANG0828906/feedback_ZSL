#include "texture.h"

#include <algorithm>
#include <cmath>
#include <iomanip>
#include <sstream>

#include "deterministic.h"
#include "sha256.h"
#include "util.h"

namespace caisson {
namespace {

double bilinear(
    const std::uint8_t* table,
    int stride,
    int offset,
    const Vec2& uv,
    int size) {
    double u = uv.x - std::floor(uv.x);
    double v = uv.y - std::floor(uv.y);
    const double x = u * static_cast<double>(size) - 0.5;
    const double y = v * static_cast<double>(size) - 0.5;
    const int x0 = static_cast<int>(std::floor(x));
    const int y0 = static_cast<int>(std::floor(y));
    const double fx = x - static_cast<double>(x0);
    const double fy = y - static_cast<double>(y0);
    const int x1 = x0 + 1;
    const int y1 = y0 + 1;
    const auto at = [&](int px, int py) -> double {
        int wrappedX = px % size;
        int wrappedY = py % size;
        if (wrappedX < 0) wrappedX += size;
        if (wrappedY < 0) wrappedY += size;
        return static_cast<double>(
                   table[(wrappedY * size + wrappedX) * stride + offset]) /
               255.0;
    };
    const double top = at(x0, y0) * (1.0 - fx) + at(x1, y0) * fx;
    const double bottom = at(x0, y1) * (1.0 - fx) + at(x1, y1) * fx;
    return top * (1.0 - fy) + bottom * fy;
}

std::string textureLayerName(int layerId, const char* suffix) {
    std::ostringstream out;
    out << "textures/layer_" << std::setw(3) << std::setfill('0') << layerId
        << '_' << suffix;
    return out.str();
}

}  // namespace

TextureSet generateTextures(const Scene& scene) {
    TextureSet result;
    for (const Layer& layer : scene.layers) {
        LayerTexture texture;
        texture.seed = deriveSeed(scene.seed, static_cast<std::uint64_t>(layer.id), 0, 1);
        if (!layer.quadrants.empty()) {
            texture.columnsHint = layer.quadrants.front().mesh.columns;
            texture.rowsHint = layer.quadrants.front().mesh.rows;
        }

        for (int y = 0; y < kTextureSize; ++y) {
            for (int x = 0; x < kTextureSize; ++x) {
                const int cellX = x / 4;
                const int cellY = y / 4;
                const std::uint64_t cellHash = splitMix64(
                    texture.seed ^
                    (static_cast<std::uint64_t>(cellX) * 0x9e3779b1ULL) ^
                    (static_cast<std::uint64_t>(cellY) * 0x85ebca77ULL));
                const double cellR = 0.74 + 0.34 * unitFromHash(cellHash);
                const double cellG = 0.72 + 0.36 * unitFromHash(splitMix64(cellHash + 1));
                const double cellB = 0.70 + 0.38 * unitFromHash(splitMix64(cellHash + 2));
                const double wave =
                    0.5 + 0.5 * std::sin(
                                    2.0 * kPi * static_cast<double>(x) / 7.0 +
                                    0.8 * static_cast<double>(y) / 3.0 +
                                    static_cast<double>(layer.id));
                const double grain =
                    unitFromHash(splitMix64(
                        texture.seed ^ (static_cast<std::uint64_t>(y) << 32U) ^
                        static_cast<std::uint64_t>(x)));
                const double fine = 0.90 + 0.10 * grain;
                const int index = (y * kTextureSize + x) * 3;
                texture.wood[static_cast<std::size_t>(index)] =
                    static_cast<std::uint8_t>(std::lround(
                        std::clamp(cellR * (0.88 + 0.12 * wave) * fine, 0.0, 1.0) * 255.0));
                texture.wood[static_cast<std::size_t>(index + 1)] =
                    static_cast<std::uint8_t>(std::lround(
                        std::clamp(cellG * (0.88 + 0.12 * wave) * fine, 0.0, 1.0) * 255.0));
                texture.wood[static_cast<std::size_t>(index + 2)] =
                    static_cast<std::uint8_t>(std::lround(
                        std::clamp(cellB * (0.88 + 0.12 * wave) * fine, 0.0, 1.0) * 255.0));

                const double weave =
                    0.5 + 0.25 * std::sin(2.0 * kPi * 6.0 * static_cast<double>(x) /
                                          static_cast<double>(kTextureSize)) +
                    0.25 * std::cos(2.0 * kPi * 7.0 * static_cast<double>(y) /
                                    static_cast<double>(kTextureSize));
                const double gauzeValue = std::clamp(
                    0.82 + 0.13 * weave + 0.05 * unitFromHash(splitMix64(cellHash + 7)),
                    0.0,
                    1.0);
                texture.gauze[static_cast<std::size_t>(y * kTextureSize + x)] =
                    static_cast<std::uint8_t>(std::lround(gauzeValue * 255.0));
            }
        }
        result.layers.emplace(layer.id, texture);
    }
    return result;
}

bool writeTextures(
    const Scene& scene,
    const TextureSet& textures,
    const std::filesystem::path& outputDirectory,
    std::string& error) {
    std::ostringstream manifest;
    manifest << "{\n";
    manifest << "  \"format\": \"caisson-texture-manifest-v1\",\n";
    manifest << "  \"texture_size\": " << kTextureSize << ",\n";
    manifest << "  \"scene_digest\": " << quote(scene.digest) << ",\n";
    manifest << "  \"layers\": [\n";
    bool first = true;
    for (const auto& [layerId, texture] : textures.layers) {
        const std::vector<std::uint8_t> woodBytes(texture.wood.begin(), texture.wood.end());
        const std::vector<std::uint8_t> gauzeBytes(texture.gauze.begin(), texture.gauze.end());
        const std::string woodRelative = textureLayerName(layerId, "wood_rgb8.bin");
        const std::string gauzeRelative = textureLayerName(layerId, "gauze_u8.bin");
        if (!atomicWriteBinary(outputDirectory / woodRelative, woodBytes, error)) return false;
        if (!atomicWriteBinary(outputDirectory / gauzeRelative, gauzeBytes, error)) return false;
        if (!first) manifest << ",\n";
        first = false;
        manifest << "    {\"layer_id\":" << layerId
                 << ",\"seed\":" << texture.seed
                 << ",\"wood\":{\"file\":" << quote(woodRelative)
                 << ",\"bytes\":" << woodBytes.size()
                 << ",\"sha256\":" << quote(sha256Hex(woodBytes)) << "}"
                 << ",\"gauze\":{\"file\":" << quote(gauzeRelative)
                 << ",\"bytes\":" << gauzeBytes.size()
                 << ",\"sha256\":" << quote(sha256Hex(gauzeBytes)) << "}}";
    }
    manifest << "\n  ]\n}\n";
    const std::string manifestText = manifest.str();
    if (!atomicWriteText(
            outputDirectory / textures.manifestRelativePath, manifestText, error)) {
        return false;
    }
    return true;
}

Vec3 sampleWood(const LayerTexture& texture, const Vec2& uv) {
    return {
        bilinear(texture.wood.data(), 3, 0, uv, kTextureSize),
        bilinear(texture.wood.data(), 3, 1, uv, kTextureSize),
        bilinear(texture.wood.data(), 3, 2, uv, kTextureSize),
    };
}

double sampleGauze(const LayerTexture& texture, const Vec2& uv) {
    return bilinear(texture.gauze.data(), 1, 0, uv, kTextureSize);
}

}  // namespace caisson

