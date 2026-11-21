#pragma once

#include <array>
#include <cstdint>
#include <optional>
#include <string>
#include <vector>

#include "terrain/status.h"

namespace terrain {

struct Rect {
    std::uint32_t x = 0;
    std::uint32_t y = 0;
    std::uint32_t width = 0;
    std::uint32_t height = 0;

    std::uint32_t right() const { return x + width; }
    std::uint32_t bottom() const { return y + height; }
    bool empty() const { return width == 0 || height == 0; }
    bool contains(std::uint32_t px, std::uint32_t py) const {
        return px >= x && py >= y && px < right() && py < bottom();
    }
    bool intersects(const Rect& other) const {
        return x < other.right() && other.x < right() &&
               y < other.bottom() && other.y < bottom();
    }
    bool touches(const Rect& other) const {
        const bool overlap_y = y <= other.bottom() && other.y <= bottom();
        const bool overlap_x = x <= other.right() && other.x <= right();
        return ((right() == other.x || other.right() == x) && overlap_y) ||
               ((bottom() == other.y || other.bottom() == y) && overlap_x);
    }
    Rect expanded(std::uint32_t amount, std::uint32_t max_w,
                  std::uint32_t max_h) const {
        Rect r;
        r.x = x > amount ? x - amount : 0;
        r.y = y > amount ? y - amount : 0;
        const std::uint32_t r_expanded =
            (right() + amount < max_w) ? right() + amount : max_w;
        const std::uint32_t b_expanded =
            (bottom() + amount < max_h) ? bottom() + amount : max_h;
        r.width = r_expanded - r.x;
        r.height = b_expanded - r.y;
        return r;
    }
};

struct Vec3 {
    float x = 0.0f;
    float y = 0.0f;
    float z = 0.0f;
};

inline Vec3 operator+(const Vec3& a, const Vec3& b) {
    return {a.x + b.x, a.y + b.y, a.z + b.z};
}
inline Vec3 operator-(const Vec3& a, const Vec3& b) {
    return {a.x - b.x, a.y - b.y, a.z - b.z};
}
inline Vec3 operator*(const Vec3& a, float s) {
    return {a.x * s, a.y * s, a.z * s};
}
inline float dot(const Vec3& a, const Vec3& b) {
    return a.x * b.x + a.y * b.y + a.z * b.z;
}
inline Vec3 cross(const Vec3& a, const Vec3& b) {
    return {a.y * b.z - a.z * b.y,
            a.z * b.x - a.x * b.z,
            a.x * b.y - a.y * b.x};
}
Vec3 normalize(const Vec3& v);

struct HeightField {
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    float cell_spacing = 1.0f;
    std::uint32_t height_type = 0;  // 0 = IEEE-754 float32
    float valid_min = 0.0f;
    float valid_max = 0.0f;
    std::uint64_t baseline_version = 0;
    std::vector<float> heights;
    std::string payload_hash;

    float at(std::uint32_t x, std::uint32_t y) const {
        return heights[static_cast<std::size_t>(y) * width + x];
    }
    float& at(std::uint32_t x, std::uint32_t y) {
        return heights[static_cast<std::size_t>(y) * width + x];
    }
};

struct MaterialImage {
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    float cell_spacing = 1.0f;
    std::uint32_t channels = 1;
    std::uint32_t palette_size = 8;
    std::uint64_t baseline_version = 0;
    std::vector<std::uint32_t> indices;
    std::string payload_hash;

    std::uint32_t at(std::uint32_t x, std::uint32_t y) const {
        return indices[static_cast<std::size_t>(y) * width + x];
    }
    std::uint32_t& at(std::uint32_t x, std::uint32_t y) {
        return indices[static_cast<std::size_t>(y) * width + x];
    }
};

struct HeightPatch {
    Rect target;
    float cell_spacing = 1.0f;
    std::uint32_t height_type = 0;
    std::uint64_t baseline_version = 0;
    std::uint64_t sequence = 0;
    std::vector<float> values;
    std::string payload_hash;
    std::string source_path;

    HeightPatch() = default;
    HeightPatch(Rect r, float s, std::uint64_t baseline, std::uint64_t seq,
                std::vector<float> v, std::string hash = {})
        : target(r),
          cell_spacing(s),
          baseline_version(baseline),
          sequence(seq),
          values(std::move(v)),
          payload_hash(std::move(hash)) {}
};

struct MaterialPatch {
    Rect target;
    float cell_spacing = 1.0f;
    std::uint32_t channels = 1;
    std::uint64_t baseline_version = 0;
    std::uint64_t sequence = 0;
    std::vector<std::uint32_t> indices;
    std::string payload_hash;
    std::string source_path;

    MaterialPatch() = default;
    MaterialPatch(Rect r, float s, std::uint64_t baseline, std::uint64_t seq,
                  std::vector<std::uint32_t> v, std::string hash = {})
        : target(r),
          cell_spacing(s),
          baseline_version(baseline),
          sequence(seq),
          indices(std::move(v)),
          payload_hash(std::move(hash)) {}
};

struct PatchBatch {
    std::uint64_t sequence = 0;
    std::optional<HeightPatch> height;
    std::optional<MaterialPatch> material;
    std::string patch_hash;
    std::string source_path;
};

struct CommitMetadata {
    std::uint64_t patch_sequence = 0;
    std::string patch_hash;
    std::vector<std::uint64_t> invalid_blocks;
    std::vector<std::string> invalidation_reasons;
    bool fallback_full = false;
};

struct Camera {
    std::uint32_t pixels_per_cell = 4;
    Vec3 light_direction{0.45f, 0.78f, 0.44f};
};

}  // namespace terrain

