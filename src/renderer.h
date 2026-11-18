#pragma once

#include <cstdint>
#include <filesystem>
#include <vector>

#include "scene.h"
#include "shadow.h"
#include "texture.h"

namespace caisson {

struct BlockRect {
    int index = 0;
    int x = 0;
    int y = 0;
    int width = 0;
    int height = 0;
};

BlockRect blockRect(const Scene& scene, int blockIndex);

struct BlockStats {
    std::vector<std::uint64_t> transmitEnergy{};
    std::vector<std::uint64_t> reflectEnergy{};
    std::vector<std::uint64_t> diffuseEnergy{};
    std::vector<std::uint64_t> layerSamples{};
    std::uint64_t coveredSamples = 0;
    std::uint64_t woodSamples = 0;
    std::uint64_t gauzeSamples = 0;
    std::uint64_t occluderSamples = 0;

    void resize(std::size_t layerCount);
};

struct BlockResult {
    BlockRect rect;
    std::vector<std::uint8_t> pixels;
    BlockStats stats;
};

class Renderer {
public:
    Renderer(
        const Scene& scene,
        const TextureSet& textures,
        const ShadowSampler& shadowSampler);

    BlockResult renderBlock(int blockIndex) const;

private:
    const Scene& scene_;
    const TextureSet& textures_;
    const ShadowSampler& shadowSampler_;
};

}  // namespace caisson

