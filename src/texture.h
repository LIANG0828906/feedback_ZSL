#pragma once

#include <array>
#include <cstdint>
#include <filesystem>
#include <map>
#include <string>
#include <vector>

#include "math3d.h"
#include "scene.h"

namespace caisson {

constexpr int kTextureSize = 32;
constexpr int kWoodTextureBytes = kTextureSize * kTextureSize * 3;
constexpr int kGauzeTextureBytes = kTextureSize * kTextureSize;

struct LayerTexture {
    std::array<std::uint8_t, kWoodTextureBytes> wood{};
    std::array<std::uint8_t, kGauzeTextureBytes> gauze{};
    std::uint64_t seed = 0;
    int columnsHint = 1;
    int rowsHint = 1;
};

struct TextureSet {
    std::map<int, LayerTexture> layers;
    std::string manifestSha256;
    std::string manifestRelativePath = "textures/manifest.json";
};

TextureSet generateTextures(const Scene& scene);

bool writeTextures(
    const Scene& scene,
    const TextureSet& textures,
    const std::filesystem::path& outputDirectory,
    std::string& error);

Vec3 sampleWood(const LayerTexture& texture, const Vec2& uv);
double sampleGauze(const LayerTexture& texture, const Vec2& uv);

}  // namespace caisson

