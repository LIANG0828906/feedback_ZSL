#include "texture.h"

#include "hash.h"

#include <algorithm>
#include <array>
#include <cmath>
#include <cstring>
#include <sstream>

namespace cr {
namespace {

double unitFromBits(std::uint64_t bits) {
    return static_cast<double>(bits >> 11u) * (1.0 / 9007199254740992.0);
}

std::uint64_t mixBits(std::uint64_t x) {
    x ^= x >> 30u;
    x *= 0xbf58476d1ce4e5b9ull;
    x ^= x >> 27u;
    x *= 0x94d049bb133111ebull;
    x ^= x >> 31u;
    return x;
}

std::uint64_t latticeHash(std::int64_t x, std::int64_t y, std::uint64_t seed) {
    const std::uint64_t ux = static_cast<std::uint64_t>(x);
    const std::uint64_t uy = static_cast<std::uint64_t>(y);
    return mixBits(seed ^ (ux * 0x9e3779b185ebca87ull) ^
                   (uy * 0xc2b2ae3d27d4eb4full));
}

double smoothstep(double t) {
    return t * t * (3.0 - 2.0 * t);
}

double valueNoise(double x, double y, std::uint64_t seed) {
    const double fx = std::floor(x);
    const double fy = std::floor(y);
    const std::int64_t ix = static_cast<std::int64_t>(fx);
    const std::int64_t iy = static_cast<std::int64_t>(fy);
    const double tx = smoothstep(x - fx);
    const double ty = smoothstep(y - fy);
    const double a = unitFromBits(latticeHash(ix, iy, seed));
    const double b = unitFromBits(latticeHash(ix + 1, iy, seed));
    const double c = unitFromBits(latticeHash(ix, iy + 1, seed));
    const double d = unitFromBits(latticeHash(ix + 1, iy + 1, seed));
    return mix(mix(a, b, tx), mix(c, d, tx), ty);
}

double fbm(double x, double y, std::uint64_t seed) {
    double sum = 0.0;
    double amplitude = 0.55;
    double frequency = 1.0;
    double norm = 0.0;
    for (int octave = 0; octave < 4; ++octave) {
        sum += amplitude * valueNoise(x * frequency, y * frequency,
                                      seed + static_cast<std::uint64_t>(octave) *
                                                 0x9e3779b97f4a7c15ull);
        norm += amplitude;
        frequency *= 2.03;
        amplitude *= 0.5;
    }
    return norm > 0.0 ? sum / norm : 0.0;
}

Vec3 adjustColorFamily(Vec3 base, std::uint64_t variant) {
    const double hue = unitFromBits(mixBits(variant ^ 0x51ed270b4d21f2c9ull));
    const double odd = unitFromBits(mixBits(variant ^ 0xa3c4e5f60718293bull));
    const double warm = 0.94 + hue * 0.12;
    const double green = 0.92 + odd * 0.16;
    const double blue = 0.90 + (1.0 - hue) * 0.18;
    return clamp01({base.x * warm, base.y * green, base.z * blue});
}

float floatFromUnit(double value) {
    return static_cast<float>(value);
}

}  // namespace

std::uint64_t splitmix64(std::uint64_t value) {
    value += 0x9e3779b97f4a7c15ull;
    value = (value ^ (value >> 30u)) * 0xbf58476d1ce4e5b9ull;
    value = (value ^ (value >> 27u)) * 0x94d049bb133111ebull;
    return value ^ (value >> 31u);
}

std::uint64_t stableStringHash(const std::string& text) {
    std::uint64_t hash = 1469598103934665603ull;
    for (const unsigned char c : text) {
        hash ^= c;
        hash *= 1099511628211ull;
    }
    return hash;
}

Vec3 WoodTexture::sampleColor(double u, double v) const {
    if (resolution <= 0 || color.empty()) return {0.0, 0.0, 0.0};
    const double x = std::min(static_cast<double>(resolution - 1),
                              std::max(0.0, u * static_cast<double>(resolution - 1)));
    const double y = std::min(static_cast<double>(resolution - 1),
                              std::max(0.0, v * static_cast<double>(resolution - 1)));
    const int x0 = static_cast<int>(std::floor(x));
    const int y0 = static_cast<int>(std::floor(y));
    const int x1 = std::min(resolution - 1, x0 + 1);
    const int y1 = std::min(resolution - 1, y0 + 1);
    const double tx = x - static_cast<double>(x0);
    const double ty = y - static_cast<double>(y0);
    auto at = [&](int px, int py) -> Vec3 {
        return color[static_cast<std::size_t>(py * resolution + px)];
    };
    const Vec3 a = mix(at(x0, y0), at(x1, y0), tx);
    const Vec3 b = mix(at(x0, y1), at(x1, y1), tx);
    return mix(a, b, ty);
}

double WoodTexture::sampleTransmittance(double u, double v) const {
    if (resolution <= 0 || transmittanceModulation.empty()) return 1.0;
    const double x = std::min(static_cast<double>(resolution - 1),
                              std::max(0.0, u * static_cast<double>(resolution - 1)));
    const double y = std::min(static_cast<double>(resolution - 1),
                              std::max(0.0, v * static_cast<double>(resolution - 1)));
    const int x0 = static_cast<int>(std::floor(x));
    const int y0 = static_cast<int>(std::floor(y));
    const int x1 = std::min(resolution - 1, x0 + 1);
    const int y1 = std::min(resolution - 1, y0 + 1);
    const double tx = x - static_cast<double>(x0);
    const double ty = y - static_cast<double>(y0);
    auto at = [&](int px, int py) -> double {
        return transmittanceModulation[static_cast<std::size_t>(py * resolution + px)];
    };
    const double a = mix(at(x0, y0), at(x1, y0), tx);
    const double b = mix(at(x0, y1), at(x1, y1), tx);
    return mix(a, b, ty);
}

WoodTexture generateWoodTexture(std::uint32_t seed, const std::string& componentId,
                                int resolution, Vec3 baseColor) {
    WoodTexture texture;
    texture.resolution = resolution;
    texture.seed = seed;
    texture.componentId = componentId;
    const std::size_t count = static_cast<std::size_t>(resolution) *
                              static_cast<std::size_t>(resolution);
    texture.color.resize(count);
    texture.transmittanceModulation.resize(count);
    const std::uint64_t idHash = stableStringHash(componentId);
    const std::uint64_t textureSeed =
        mixBits(static_cast<std::uint64_t>(seed) ^ (idHash * 0xd6e8feb86659fd93ull));
    const Vec3 family = adjustColorFamily(baseColor, textureSeed);
    const double phase =
        unitFromBits(mixBits(textureSeed ^ 0x6a09e667f3bcc909ull)) * 2.0 * kPi;
    const double ringScale =
        8.0 + unitFromBits(mixBits(textureSeed ^ 0xbb67ae8584caa73bull)) * 7.0;
    const double ringFrequency =
        1.0 + std::floor(
                  unitFromBits(mixBits(textureSeed ^ 0x3c6ef372fe94f82bull)) * 4.0);

    for (int y = 0; y < resolution; ++y) {
        for (int x = 0; x < resolution; ++x) {
            const double u = static_cast<double>(x) / static_cast<double>(resolution);
            const double v = static_cast<double>(y) / static_cast<double>(resolution);
            const double warp = fbm(u * 3.1 + 1.7, v * 4.3 + 0.2, textureSeed);
            const double ringCoordinate =
                v * ringScale + 1.35 * warp + 0.16 * std::sin((u * ringFrequency + v) * kPi);
            const double ring = 0.5 + 0.5 * std::sin(ringCoordinate * 2.0 * kPi + phase);
            const double fine = valueNoise(u * 36.0 + 3.0, v * 11.0 + 9.0,
                                           textureSeed ^ 0x9b05688c2b3e6c1full);
            const double pores = std::pow(std::max(0.0, ring), 3.2);
            const double darkLine =
                std::exp(-std::pow((ringCoordinate - std::floor(ringCoordinate) - 0.47) / 0.045, 2.0));
            const double brightness =
                clamp01(0.34 + 0.39 * ring + 0.21 * fine - 0.27 * darkLine +
                        0.08 * pores);
            Vec3 rgb{family.x * brightness, family.y * brightness, family.z * brightness};
            rgb.x *= 0.96 + 0.08 * fine;
            rgb.y *= 0.97 + 0.05 * pores;
            rgb = clamp01(rgb);
            texture.color[static_cast<std::size_t>(y * resolution + x)] = rgb;
            const double transmit =
                clamp01(0.55 + 0.30 * ring + 0.14 * fine - 0.16 * darkLine);
            texture.transmittanceModulation[static_cast<std::size_t>(y * resolution + x)] =
                floatFromUnit(transmit);
        }
    }

    const std::vector<std::uint8_t> table = serializeTextureTable(texture);
    texture.digest = sha256Hex(table.data(), table.size());
    return texture;
}

TextureBank buildTextureBank(const Scene& scene) {
    TextureBank bank;
    std::ostringstream bankMaterial;
    for (const Layer& layer : scene.layers) {
        for (const Board& board : layer.boards) {
            WoodTexture texture = generateWoodTexture(board.seed, board.id,
                                                      scene.render.textureResolution, board.color);
            bankMaterial << layer.index << '|' << board.id << '|' << board.seed << '|'
                         << board.transmittance << '|' << board.color.x << ',' << board.color.y << ','
                         << board.color.z << '|' << texture.digest << '\n';
            bank.byOwnerCode[board.ownerCode] = std::move(texture);
        }
    }
    bank.bankDigest = sha256Hex(bankMaterial.str());
    return bank;
}

std::vector<std::uint8_t> serializeTextureTable(const WoodTexture& texture) {
    std::vector<std::uint8_t> bytes;
    const std::string header = "COFFWOOD1\n" + std::to_string(texture.resolution) + "\n" +
                               std::to_string(texture.seed) + "\n" + texture.componentId + "\n";
    bytes.insert(bytes.end(), header.begin(), header.end());
    const std::size_t count = texture.color.size();
    bytes.reserve(header.size() + count * 16);
    auto appendFloat = [&](float value) {
        std::uint32_t bits = 0;
        std::memcpy(&bits, &value, sizeof(bits));
        bytes.push_back(static_cast<std::uint8_t>(bits & 0xffu));
        bytes.push_back(static_cast<std::uint8_t>((bits >> 8u) & 0xffu));
        bytes.push_back(static_cast<std::uint8_t>((bits >> 16u) & 0xffu));
        bytes.push_back(static_cast<std::uint8_t>((bits >> 24u) & 0xffu));
    };
    for (std::size_t i = 0; i < count; ++i) {
        appendFloat(static_cast<float>(texture.color[i].x));
        appendFloat(static_cast<float>(texture.color[i].y));
        appendFloat(static_cast<float>(texture.color[i].z));
        appendFloat(texture.transmittanceModulation[i]);
    }
    return bytes;
}

}  // namespace cr

