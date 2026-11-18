#pragma once

#include "json.hpp"
#include "renderer.hpp"
#include "scene.hpp"

#include <array>
#include <map>
#include <string>
#include <vector>

namespace sr {

struct ImageDigests {
    std::array<uint8_t, 32> finalRgba{};
    std::array<uint8_t, 32> smokeRgba{};
    std::array<uint8_t, 32> ownership{};
    std::array<uint8_t, 32> finalPng{};
    std::array<uint8_t, 32> smokePng{};
    std::array<uint8_t, 32> ownershipPng{};
};

Json buildShadowLandings(const Renderer& renderer);
Json buildStatsReport(const Scene& scene, const Renderer& renderer,
                      const std::map<int, TileResult>& tiles,
                      const ImageDigests& digests);
Json buildRecoveryReport(const Scene& scene, const Renderer& renderer,
                         const std::vector<int>& completedTiles,
                         const std::string& interruption);

}  // namespace sr

