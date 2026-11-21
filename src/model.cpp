#include "chunkpipe/model.hpp"

#include <algorithm>
#include <cmath>

namespace chunkpipe {

Vec3 operator+(const Vec3& a, const Vec3& b) {
    return {a.x + b.x, a.y + b.y, a.z + b.z};
}

Vec3 operator-(const Vec3& a, const Vec3& b) {
    return {a.x - b.x, a.y - b.y, a.z - b.z};
}

Vec3 operator*(const Vec3& a, float scalar) {
    return {a.x * scalar, a.y * scalar, a.z * scalar};
}

float dot(const Vec3& a, const Vec3& b) {
    return a.x * b.x + a.y * b.y + a.z * b.z;
}

Vec3 cross(const Vec3& a, const Vec3& b) {
    return {a.y * b.z - a.z * b.y,
            a.z * b.x - a.x * b.z,
            a.x * b.y - a.y * b.x};
}

float length(const Vec3& value) {
    return std::sqrt(dot(value, value));
}

Vec3 normalize(const Vec3& value) {
    const float magnitude = length(value);
    if (magnitude <= 1e-20f) {
        return {0.0f, 0.0f, 0.0f};
    }
    return value * (1.0f / magnitude);
}

Vec3 minVec(const Vec3& a, const Vec3& b) {
    return {std::min(a.x, b.x), std::min(a.y, b.y), std::min(a.z, b.z)};
}

Vec3 maxVec(const Vec3& a, const Vec3& b) {
    return {std::max(a.x, b.x), std::max(a.y, b.y), std::max(a.z, b.z)};
}

Vec3 Aabb::center() const {
    return (min + max) * 0.5f;
}

Vec3 Aabb::extent() const {
    return max - min;
}

bool Aabb::finiteAndOrdered() const {
    return std::isfinite(min.x) && std::isfinite(min.y) && std::isfinite(min.z) &&
           std::isfinite(max.x) && std::isfinite(max.y) && std::isfinite(max.z) &&
           min.x <= max.x && min.y <= max.y && min.z <= max.z;
}

bool Aabb::contains(const Vec3& point, float tolerance) const {
    return point.x >= min.x - tolerance && point.x <= max.x + tolerance &&
           point.y >= min.y - tolerance && point.y <= max.y + tolerance &&
           point.z >= min.z - tolerance && point.z <= max.z + tolerance;
}

std::array<Vec3, 8> Aabb::corners() const {
    return {{{min.x, min.y, min.z},
             {max.x, min.y, min.z},
             {min.x, max.y, min.z},
             {max.x, max.y, min.z},
             {min.x, min.y, max.z},
             {max.x, min.y, max.z},
             {min.x, max.y, max.z},
             {max.x, max.y, max.z}}};
}

const char* prioritySourceName(PrioritySource source) {
    switch (source) {
    case PrioritySource::AuthoredV3:
        return "authored-v3";
    case PrioritySource::DerivedFromBounds:
        return "derived-from-bounds";
    }
    return "unknown";
}

} // namespace chunkpipe

