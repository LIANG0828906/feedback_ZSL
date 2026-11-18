#include "report.h"

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <filesystem>
#include <map>
#include <set>
#include <sstream>
#include <tuple>
#include <vector>

#include "png_io.h"
#include "sha256.h"
#include "util.h"

namespace caisson {
namespace {

std::string hashFile(
    const std::filesystem::path& path,
    std::string& error) {
    std::vector<std::uint8_t> bytes;
    if (!readBinaryFile(path, bytes, error)) return "";
    return sha256Hex(bytes);
}

int ppm(std::uint64_t numerator, std::uint64_t denominator) {
    if (denominator == 0) return 0;
    return static_cast<int>(std::lround(
        static_cast<double>(numerator) * 1000000.0 /
        static_cast<double>(denominator)));
}

int signedMm(double meters) {
    return static_cast<int>(std::llround(meters * 1000.0));
}

std::string channelName(int channel) {
    switch (channel) {
        case 0: return "R";
        case 1: return "G";
        case 2: return "B";
        default: return "A";
    }
}

int blockIndexForPixel(const Scene& scene, int x, int y) {
    const int blockX = x / scene.image.tileSize;
    const int blockY = y / scene.image.tileSize;
    return blockY * scene.blockColumns() + blockX;
}

std::string pixelDifferenceJson(
    const Scene& scene,
    const std::string& fileName,
    int x,
    int y,
    int channel,
    int reference,
    int actual) {
    std::ostringstream out;
    out << "{\"file\":" << quote(fileName)
        << ",\"block_index\":" << blockIndexForPixel(scene, x, y)
        << ",\"x\":" << x << ",\"y\":" << y
        << ",\"channel\":" << quote(channelName(channel))
        << ",\"reference\":" << reference
        << ",\"actual\":" << actual << '}';
    return out.str();
}

bool comparePng(
    const Scene& scene,
    const std::filesystem::path& actualPath,
    const std::filesystem::path& referencePath,
    const std::string& fileName,
    std::vector<std::string>& differences,
    std::string& failureCode,
    std::string& failureMessage) {
    int actualWidth = 0;
    int actualHeight = 0;
    int referenceWidth = 0;
    int referenceHeight = 0;
    std::vector<std::uint8_t> actual;
    std::vector<std::uint8_t> reference;
    std::string error;
    if (!readPngRgba(
            actualPath, actualWidth, actualHeight, actual, error)) {
        failureCode = "E_REFERENCE_IO";
        failureMessage = "cannot read actual PNG (" + fileName + "): " + error;
        return false;
    }
    if (!readPngRgba(
            referencePath, referenceWidth, referenceHeight, reference, error)) {
        failureCode = "E_REFERENCE_IO";
        failureMessage = "cannot read reference PNG (" + fileName + "): " + error;
        return false;
    }
    if (actualWidth != referenceWidth || actualHeight != referenceHeight ||
        actualWidth != scene.image.width || actualHeight != scene.image.height) {
        failureCode = "E_RECOVERY_DIMENSION_MISMATCH";
        failureMessage = "image dimensions differ for " + fileName;
        return false;
    }
    for (int y = 0; y < actualHeight; ++y) {
        for (int x = 0; x < actualWidth; ++x) {
            const std::size_t offset =
                (static_cast<std::size_t>(y) * static_cast<std::size_t>(actualWidth) +
                 static_cast<std::size_t>(x)) *
                4;
            for (int channel = 0; channel < 4; ++channel) {
                const int a = actual[offset + static_cast<std::size_t>(channel)];
                const int r = reference[offset + static_cast<std::size_t>(channel)];
                if (a == r) continue;
                if (differences.size() < 64) {
                    differences.push_back(pixelDifferenceJson(
                        scene, fileName, x, y, channel, r, a));
                }
                failureCode = "E_RECOVERY_PIXEL_MISMATCH";
                failureMessage =
                    "resumed output differs from reference pixels";
            }
        }
    }
    return differences.empty();
}

}  // namespace

void AggregateStats::resize(std::size_t layerCount) {
    totals.resize(layerCount);
}

void AggregateStats::add(const BlockStats& block) {
    if (totals.transmitEnergy.size() != block.transmitEnergy.size()) {
        totals.resize(block.transmitEnergy.size());
    }
    for (std::size_t i = 0; i < totals.transmitEnergy.size(); ++i) {
        totals.transmitEnergy[i] += block.transmitEnergy[i];
        totals.reflectEnergy[i] += block.reflectEnergy[i];
        totals.diffuseEnergy[i] += block.diffuseEnergy[i];
        totals.layerSamples[i] += block.layerSamples[i];
    }
    totals.coveredSamples += block.coveredSamples;
    totals.woodSamples += block.woodSamples;
    totals.gauzeSamples += block.gauzeSamples;
    totals.occluderSamples += block.occluderSamples;
}

std::vector<std::uint8_t> makeDisplayImage(
    const Scene& scene,
    const std::vector<std::uint8_t>& premultiplied) {
    std::vector<std::uint8_t> display(premultiplied.size(), 0U);
    const std::size_t pixelCount = premultiplied.size() / 4;
    for (std::size_t pixel = 0; pixel < pixelCount; ++pixel) {
        const std::size_t offset = pixel * 4;
        const double alpha = static_cast<double>(premultiplied[offset + 3]) / 255.0;
        const double remaining = 1.0 - alpha;
        display[offset] = static_cast<std::uint8_t>(std::lround(
            std::clamp(
                static_cast<double>(premultiplied[offset]) +
                    scene.image.background.x * 255.0 * remaining,
                0.0,
                255.0)));
        display[offset + 1] = static_cast<std::uint8_t>(std::lround(
            std::clamp(
                static_cast<double>(premultiplied[offset + 1]) +
                    scene.image.background.y * 255.0 * remaining,
                0.0,
                255.0)));
        display[offset + 2] = static_cast<std::uint8_t>(std::lround(
            std::clamp(
                static_cast<double>(premultiplied[offset + 2]) +
                    scene.image.background.z * 255.0 * remaining,
                0.0,
                255.0)));
        display[offset + 3] = 255U;
    }
    return display;
}

std::string buildRenderReport(
    const Scene& scene,
    const AggregateStats& stats,
    const ShadowReportData& shadow,
    const std::filesystem::path& outputDirectory,
    const std::string& textureManifestRelative,
    const std::vector<std::uint8_t>& display,
    const std::vector<std::uint8_t>& premultiplied,
    const std::vector<std::uint8_t>& ownership,
    const std::string& shadowLandingsSha256) {
    const std::size_t pixelCount = display.size() / 4;
    std::uint64_t channelSums[3]{};
    int maxChannel = 0;
    std::uint64_t saturatedPixels = 0;
    std::uint64_t saturatedChannels = 0;
    std::uint64_t nonGrayPixels = 0;
    std::uint64_t coveragePixels = 0;
    std::uint64_t occluderPixels = 0;
    std::uint64_t nearestCounts[256]{};
    std::uint64_t dominantCounts[256]{};
    std::set<std::uint32_t> uniqueColors;
    for (std::size_t pixel = 0; pixel < pixelCount; ++pixel) {
        const std::size_t offset = pixel * 4;
        const int r = display[offset];
        const int g = display[offset + 1];
        const int b = display[offset + 2];
        channelSums[0] += static_cast<std::uint64_t>(r);
        channelSums[1] += static_cast<std::uint64_t>(g);
        channelSums[2] += static_cast<std::uint64_t>(b);
        maxChannel = std::max({maxChannel, r, g, b});
        if (r == 255 || g == 255 || b == 255) ++saturatedPixels;
        saturatedChannels += static_cast<std::uint64_t>(r == 255) +
                             static_cast<std::uint64_t>(g == 255) +
                             static_cast<std::uint64_t>(b == 255);
        if (std::max({r, g, b}) - std::min({r, g, b}) >= 8) ++nonGrayPixels;
        uniqueColors.insert(
            (static_cast<std::uint32_t>(r) << 16U) |
            (static_cast<std::uint32_t>(g) << 8U) |
            static_cast<std::uint32_t>(b));
        const int nearest = ownership[offset];
        const int dominant = ownership[offset + 2];
        ++nearestCounts[static_cast<std::size_t>(nearest)];
        ++dominantCounts[static_cast<std::size_t>(dominant)];
        if (ownership[offset + 3] != 0) ++coveragePixels;
        if (nearest == 255) ++occluderPixels;
    }
    int premultipliedMaxChannel = 0;
    int premultipliedMaxAlpha = 0;
    for (std::size_t pixel = 0; pixel < pixelCount; ++pixel) {
        const std::size_t offset = pixel * 4;
        premultipliedMaxChannel = std::max({
            premultipliedMaxChannel,
            static_cast<int>(premultiplied[offset]),
            static_cast<int>(premultiplied[offset + 1]),
            static_cast<int>(premultiplied[offset + 2]),
        });
        premultipliedMaxAlpha = std::max(
            premultipliedMaxAlpha,
            static_cast<int>(premultiplied[offset + 3]));
    }

    std::string error;
    const std::string imageSha = hashFile(outputDirectory / "image.png", error);
    if (!error.empty()) return "";
    const std::string premulSha =
        hashFile(outputDirectory / "premultiplied.png", error);
    if (!error.empty()) return "";
    const std::string ownerSha =
        hashFile(outputDirectory / "ownership.png", error);
    if (!error.empty()) return "";
    const std::string textureSha =
        hashFile(outputDirectory / textureManifestRelative, error);
    if (!error.empty()) return "";

    std::vector<std::uint64_t> transmit = stats.totals.transmitEnergy;
    std::vector<std::uint64_t> reflect = stats.totals.reflectEnergy;
    std::vector<std::uint64_t> diffuse = stats.totals.diffuseEnergy;
    std::vector<std::uint64_t> layerSamples = stats.totals.layerSamples;
    const std::size_t layerCount = scene.layers.size();
    transmit.resize(layerCount, 0U);
    reflect.resize(layerCount, 0U);
    diffuse.resize(layerCount, 0U);
    layerSamples.resize(layerCount, 0U);

    std::ostringstream out;
    out << "{\n";
    out << "  \"report_version\": 1,\n";
    out << "  \"renderer\": \"caisson_render/1\",\n";
    out << "  \"scene_digest\": " << quote(scene.digest) << ",\n";
    out << "  \"seed\": " << scene.seed << ",\n";
    out << "  \"image\": {\"width\":" << scene.image.width
        << ",\"height\":" << scene.image.height
        << ",\"tile_size\":" << scene.image.tileSize
        << ",\"aa_samples\":" << scene.image.aaSamples
        << ",\"block_columns\":" << scene.blockColumns()
        << ",\"block_rows\":" << scene.blockRows()
        << ",\"block_count\":" << scene.blockCount() << "},\n";
    out << "  \"outputs\": {";
    out << "\"image\":{\"file\":\"image.png\",\"encoding\":\"RGBA8_straight_over_background\","
           "\"sha256\":"
        << quote(imageSha) << "},";
    out << "\"premultiplied\":{\"file\":\"premultiplied.png\","
           "\"encoding\":\"RGBA8_premultiplied\",\"sha256\":"
        << quote(premulSha) << "},";
    out << "\"ownership\":{\"file\":\"ownership.png\","
           "\"encoding\":\"R=nearest_layer,G=layer_count,B=dominant_layer,A=coverage\","
           "\"sha256\":"
        << quote(ownerSha) << "},";
    out << "\"shadow_landings\":{\"file\":\"shadow_landings.csv\",\"sha256\":"
        << quote(shadowLandingsSha256) << "},";
    out << "\"texture_manifest\":{\"file\":" << quote(textureManifestRelative)
        << ",\"sha256\":" << quote(textureSha) << "}},\n";

    out << "  \"color_stats\": {";
    out << "\"mean_rgb_milli\":["
        << (pixelCount == 0 ? 0 : static_cast<int>(std::lround(
                static_cast<double>(channelSums[0]) * 1000.0 /
                (static_cast<double>(pixelCount) * 255.0))))
        << ','
        << (pixelCount == 0 ? 0 : static_cast<int>(std::lround(
                static_cast<double>(channelSums[1]) * 1000.0 /
                (static_cast<double>(pixelCount) * 255.0))))
        << ','
        << (pixelCount == 0 ? 0 : static_cast<int>(std::lround(
                static_cast<double>(channelSums[2]) * 1000.0 /
                (static_cast<double>(pixelCount) * 255.0))))
        << "],\"max_channel\":" << maxChannel
        << ",\"saturated_pixels\":" << saturatedPixels
        << ",\"saturated_channel_values\":" << saturatedChannels
        << ",\"premultiplied_max_channel\":" << premultipliedMaxChannel
        << ",\"premultiplied_max_alpha\":" << premultipliedMaxAlpha
        << ",\"non_gray_pixels\":" << nonGrayPixels
        << ",\"coverage_pixels\":" << coveragePixels
        << ",\"occluder_visible_pixels\":" << occluderPixels
        << ",\"unique_rgb_colors\":" << uniqueColors.size() << "},\n";

    out << "  \"sampling_stats\": {\"covered_samples\":"
        << stats.totals.coveredSamples
        << ",\"wood_samples\":" << stats.totals.woodSamples
        << ",\"gauze_samples\":" << stats.totals.gauzeSamples
        << ",\"occluder_samples\":" << stats.totals.occluderSamples << "},\n";

    out << "  \"layers\": [\n";
    for (std::size_t i = 0; i < layerCount; ++i) {
        if (i != 0) out << ",\n";
        const std::uint64_t trans = transmit[i];
        const std::uint64_t refl = reflect[i];
        const std::uint64_t diff = diffuse[i];
        const int layerId = scene.layers[i].id;
        out << "    {\"layer_id\":" << layerId
            << ",\"transmitted_energy_u64\":" << trans
            << ",\"reflected_energy_u64\":" << refl
            << ",\"diffuse_energy_u64\":" << diff
            << ",\"transmit_share_ppm\":" << ppm(trans, trans + refl)
            << ",\"reflect_share_ppm\":" << ppm(refl, trans + refl)
            << ",\"layer_samples\":" << layerSamples[i]
            << ",\"nearest_pixels\":" << nearestCounts[static_cast<std::size_t>(layerId)]
            << ",\"dominant_pixels\":" << dominantCounts[static_cast<std::size_t>(layerId)]
            << '}';
    }
    out << "\n  ],\n";

    out << "  \"soft_shadow\": {\n";
    out << "    \"samples_per_axis\":" << shadow.samplesPerAxis << ",\n";
    out << "    \"samples_per_lamp\":" << shadow.samplesPerLamp << ",\n";
    out << "    \"probe_samples\":" << shadow.probeSamples << ",\n";
    out << "    \"probe_half_span_mm\":" << shadow.probeHalfSpanMm << ",\n";
    out << "    \"distribution_fingerprint\":" << quote(shadow.fingerprint) << ",\n";
    out << "    \"depth_trend\": {";
    std::map<std::string, std::pair<long long, long long>> occluderWidths;
    for (const ShadowProfile& profile : shadow.profiles) {
        if (!profile.crossed) continue;
        auto& entry = occluderWidths[profile.occluderId];
        entry.first += profile.transitionWidthMm;
        entry.second += profile.occluderToReceiverMm;
    }
    std::vector<std::tuple<int, int, std::string>> trend;
    for (const auto& [id, sums] : occluderWidths) {
        const long long count = static_cast<long long>(
            std::count_if(
                shadow.profiles.begin(),
                shadow.profiles.end(),
                [&](const ShadowProfile& profile) {
                    return profile.occluderId == id && profile.crossed;
                }));
        if (count <= 0) continue;
        trend.emplace_back(
            static_cast<int>(sums.second / count),
            static_cast<int>(sums.first / count),
            id);
    }
    std::sort(trend.begin(), trend.end());
    out << "\"metric\":\"mean_occluder_to_receiver_mm\",\"points\":[";
    bool monotonic = true;
    int previousWidth = -1;
    for (std::size_t i = 0; i < trend.size(); ++i) {
        if (i != 0) out << ',';
        const int receiverMm = std::get<0>(trend[i]);
        const int widthMm = std::get<1>(trend[i]);
        const std::string& id = std::get<2>(trend[i]);
        if (previousWidth >= 0 && widthMm < previousWidth) monotonic = false;
        previousWidth = widthMm;
        out << "{\"occluder_id\":" << quote(id)
            << ",\"mean_occluder_to_receiver_mm\":" << receiverMm
            << ",\"mean_transition_width_mm\":" << widthMm << '}';
    }
    out << "],\"monotonic_non_decreasing\":" << (monotonic ? "true" : "false")
        << "},\n";
    out << "    \"profiles\": [\n";
    for (std::size_t i = 0; i < shadow.profiles.size(); ++i) {
        const ShadowProfile& profile = shadow.profiles[i];
        if (i != 0) out << ",\n";
        out << "      {\"lamp_id\":" << quote(profile.lampId)
            << ",\"dougong_id\":" << quote(profile.occluderId)
            << ",\"layer_id\":" << profile.layerId
            << ",\"occluder_depth_mm\":" << profile.occluderDepthMm
            << ",\"occluder_to_receiver_mm\":" << profile.occluderToReceiverMm
            << ",\"landing_mm\":[" << signedMm(profile.landing.x) << ','
            << signedMm(profile.landing.y) << ',' << signedMm(profile.landing.z) << ']'
            << ",\"axis_milli\":[" << signedMm(profile.axis.x) << ','
            << signedMm(profile.axis.y) << ',' << signedMm(profile.axis.z) << ']'
            << ",\"has_shadow\":" << (profile.crossed ? "true" : "false")
            << ",\"left_edge_width_mm\":" << profile.leftEdgeWidthMm
            << ",\"right_edge_width_mm\":" << profile.rightEdgeWidthMm
            << ",\"transition_width_mm\":" << profile.transitionWidthMm
            << ",\"samples\":[";
        for (std::size_t pointIndex = 0;
             pointIndex < profile.points.size();
             ++pointIndex) {
            if (pointIndex != 0) out << ',';
            out << "{\"offset_mm\":" << profile.points[pointIndex].offsetMm
                << ",\"gray_0_255\":" << profile.points[pointIndex].gray << '}';
        }
        out << "]}";
    }
    out << "\n    ]\n  },\n";

    out << "  \"projection_landings\": [\n";
    for (std::size_t i = 0; i < shadow.landings.size(); ++i) {
        const ShadowLanding& landing = shadow.landings[i];
        if (i != 0) out << ",\n";
        out << "    {\"lamp_id\":" << quote(landing.lampId)
            << ",\"dougong_id\":" << quote(landing.occluderId)
            << ",\"vertex_index\":" << landing.vertexIndex
            << ",\"layer_id\":" << landing.layerId
            << ",\"x_mm\":" << signedMm(landing.point.x)
            << ",\"y_mm\":" << signedMm(landing.point.y)
            << ",\"z_mm\":" << signedMm(landing.point.z) << '}';
    }
    out << "\n  ]\n";
    out << "}\n";
    return out.str();
}

bool verifyAgainstReference(
    const Scene& scene,
    const std::filesystem::path& outputDirectory,
    const std::filesystem::path& referenceDirectory,
    std::string& structuredErrorJson) {
    std::vector<std::string> differences;
    std::string failureCode;
    std::string failureMessage;
    const std::array<const char*, 3> pngFiles = {
        "image.png", "premultiplied.png", "ownership.png"};
    for (const char* file : pngFiles) {
        comparePng(
            scene,
            outputDirectory / file,
            referenceDirectory / file,
            file,
            differences,
            failureCode,
            failureMessage);
        if (!failureCode.empty() &&
            failureCode != "E_RECOVERY_PIXEL_MISMATCH") {
            break;
        }
    }
    if (failureCode.empty()) {
        std::vector<std::uint8_t> actualLandings;
        std::vector<std::uint8_t> referenceLandings;
        std::string error;
        if (!readBinaryFile(
                outputDirectory / "shadow_landings.csv", actualLandings, error) ||
            !readBinaryFile(
                referenceDirectory / "shadow_landings.csv", referenceLandings, error)) {
            failureCode = "E_REFERENCE_IO";
            failureMessage = "cannot read shadow landing CSV: " + error;
        } else if (actualLandings != referenceLandings) {
            failureCode = "E_RECOVERY_LANDING_MISMATCH";
            failureMessage = "projection landing coordinates differ";
        }
    }
    if (failureCode.empty()) return true;

    std::ostringstream out;
    out << "{\"error\":{\"code\":" << quote(failureCode)
        << ",\"message\":" << quote(failureMessage);
    if (!differences.empty()) {
        out << ",\"differences\":[";
        for (std::size_t i = 0; i < differences.size(); ++i) {
            if (i != 0) out << ',';
            out << differences[i];
        }
        out << ']';
    }
    out << "}}";
    structuredErrorJson = out.str();
    return false;
}

}  // namespace caisson

