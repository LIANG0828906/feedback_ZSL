#pragma once

#include <algorithm>
#include <array>
#include <cmath>
#include <cstddef>
#include <cstdint>
#include <limits>
#include <string>

#include <glm/glm.hpp>
#include <glm/gtc/matrix_inverse.hpp>
#include <glm/gtc/matrix_transform.hpp>

namespace shadowpipe {

using Vec2 = glm::vec2;
using Vec3 = glm::vec3;
using Vec4 = glm::vec4;
using Mat4 = glm::mat4;

constexpr float kInfinity = std::numeric_limits<float>::infinity();
constexpr float kEpsilon = 1.0e-7f;

struct AABB {
    Vec3 min{kInfinity};
    Vec3 max{-kInfinity};

    bool valid() const {
        return std::isfinite(min.x) && std::isfinite(min.y) && std::isfinite(min.z) &&
               std::isfinite(max.x) && std::isfinite(max.y) && std::isfinite(max.z) &&
               min.x <= max.x && min.y <= max.y && min.z <= max.z;
    }

    Vec3 center() const { return (min + max) * 0.5f; }
    Vec3 extent() const { return max - min; }
    float volume() const {
        const Vec3 e = extent();
        return e.x * e.y * e.z;
    }

    void expand(const Vec3& p) {
        min = glm::min(min, p);
        max = glm::max(max, p);
    }

    void expand(const AABB& other) {
        if (!other.valid()) {
            return;
        }
        expand(other.min);
        expand(other.max);
    }

    void pad(float amount) {
        if (!valid()) {
            return;
        }
        min -= Vec3(amount);
        max += Vec3(amount);
    }

    bool contains(const AABB& other, float epsilon = 0.0f) const {
        if (!valid() || !other.valid()) {
            return false;
        }
        return min.x - epsilon <= other.min.x && min.y - epsilon <= other.min.y &&
               min.z - epsilon <= other.min.z && max.x + epsilon >= other.max.x &&
               max.y + epsilon >= other.max.y && max.z + epsilon >= other.max.z;
    }

    bool contains(const Vec3& p, float epsilon = 0.0f) const {
        if (!valid()) {
            return false;
        }
        return p.x >= min.x - epsilon && p.x <= max.x + epsilon &&
               p.y >= min.y - epsilon && p.y <= max.y + epsilon &&
               p.z >= min.z - epsilon && p.z <= max.z + epsilon;
    }

    std::array<Vec3, 8> corners() const {
        return {Vec3{min.x, min.y, min.z}, Vec3{max.x, min.y, min.z},
                Vec3{min.x, max.y, min.z}, Vec3{max.x, max.y, min.z},
                Vec3{min.x, min.y, max.z}, Vec3{max.x, min.y, max.z},
                Vec3{min.x, max.y, max.z}, Vec3{max.x, max.y, max.z}};
    }
};

inline AABB transformAABB(const AABB& box, const Mat4& transform) {
    AABB out;
    if (!box.valid()) {
        return out;
    }
    for (const Vec3& corner : box.corners()) {
        const Vec4 transformed = transform * Vec4(corner, 1.0f);
        if (std::abs(transformed.w) > kEpsilon) {
            out.expand(Vec3(transformed) / transformed.w);
        } else {
            out.expand(Vec3(transformed));
        }
    }
    return out;
}

inline bool finiteVec(const Vec3& v) {
    return std::isfinite(v.x) && std::isfinite(v.y) && std::isfinite(v.z);
}

inline bool finiteMat(const Mat4& m) {
    for (int c = 0; c < 4; ++c) {
        for (int r = 0; r < 4; ++r) {
            if (!std::isfinite(m[c][r])) {
                return false;
            }
        }
    }
    return true;
}

inline float safeFloat(float value) {
    if (!std::isfinite(value)) {
        return std::signbit(value) ? -std::numeric_limits<float>::max()
                                   : std::numeric_limits<float>::max();
    }
    return std::abs(value) < 1.0e-12f ? 0.0f : value;
}

inline float clamp01(float value) {
    return std::max(0.0f, std::min(1.0f, value));
}

std::string formatFloat(float value, int precision = 9);

}  // namespace shadowpipe

