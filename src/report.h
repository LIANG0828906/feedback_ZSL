#pragma once

#include <array>
#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "renderer.h"
#include "scene.h"
#include "shadow.h"
#include "texture.h"

namespace caisson {

struct AggregateStats {
    BlockStats totals;

    void resize(std::size_t layerCount);
    void add(const BlockStats& block);
};

std::vector<std::uint8_t> makeDisplayImage(
    const Scene& scene,
    const std::vector<std::uint8_t>& premultiplied);

std::string buildRenderReport(
    const Scene& scene,
    const AggregateStats& stats,
    const ShadowReportData& shadow,
    const std::filesystem::path& outputDirectory,
    const std::string& textureManifestRelative,
    const std::vector<std::uint8_t>& display,
    const std::vector<std::uint8_t>& premultiplied,
    const std::vector<std::uint8_t>& ownership,
    const std::string& shadowLandingsSha256);

bool verifyAgainstReference(
    const Scene& scene,
    const std::filesystem::path& outputDirectory,
    const std::filesystem::path& referenceDirectory,
    std::string& structuredErrorJson);

}  // namespace caisson

