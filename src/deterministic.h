#pragma once

#include <cmath>
#include <cstdint>

#include "math3d.h"

namespace caisson {

inline std::uint64_t splitMix64(std::uint64_t value) {
    value += 0x9e3779b97f4a7c15ULL;
    value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9ULL;
    value = (value ^ (value >> 27)) * 0x94d049bb133111ebULL;
    return value ^ (value >> 31);
}

inline std::uint64_t deriveSeed(
    std::int64_t sceneSeed,
    std::uint64_t layerId,
    std::uint64_t quadrant,
    std::uint64_t stream) {
    std::uint64_t state = static_cast<std::uint64_t>(sceneSeed);
    state ^= splitMix64(layerId + 0x100000001b3ULL);
    state ^= splitMix64(quadrant + 0x200000001b3ULL);
    state ^= splitMix64(stream + 0x300000001b3ULL);
    return splitMix64(state);
}

inline double unitFromHash(std::uint64_t hash) {
    return static_cast<double>(hash >> 11) * (1.0 / 9007199254740992.0);
}

inline Vec3 sphereSample(int index, int count) {
    if (count <= 1) return {0.0, 1.0, 0.0};
    const double z = 1.0 - (2.0 * static_cast<double>(index) + 1.0) /
                               static_cast<double>(count);
    const double radius = std::sqrt(std::max(0.0, 1.0 - z * z));
    const double angle = 2.39996322972865332223 *
                         static_cast<double>(index);
    return {radius * std::cos(angle), z, radius * std::sin(angle)};
}

}  // namespace caisson

