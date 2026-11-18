#pragma once

#include "math.h"
#include "scene.h"

#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace cr {

struct WoodTexture {
    int resolution = 0;
    std::uint32_t seed = 0;
    std::string componentId;
    std::vector<Vec3> color;
    std::vector<float> transmittanceModulation;
    std::string digest;

    Vec3 sampleColor(double u, double v) const;
    double sampleTransmittance(double u, double v) const;
};

struct TextureBank {
    std::map<int, WoodTexture> byOwnerCode;
    std::string bankDigest;
};

std::uint64_t splitmix64(std::uint64_t value);
std::uint64_t stableStringHash(const std::string& text);

WoodTexture generateWoodTexture(std::uint32_t seed, const std::string& componentId,
                                int resolution, Vec3 baseColor);
TextureBank buildTextureBank(const Scene& scene);

std::vector<std::uint8_t> serializeTextureTable(const WoodTexture& texture);

}  // namespace cr

