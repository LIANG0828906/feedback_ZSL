#pragma once

#include <algorithm>
#include <array>
#include <cmath>
#include <cstddef>

namespace cr {

constexpr double kPi = 3.141592653589793238462643383279502884;

struct Vec2 {
    double x = 0.0;
    double y = 0.0;
};

struct Vec3 {
    double x = 0.0;
    double y = 0.0;
    double z = 0.0;

    double& operator[](std::size_t i) { return i == 0 ? x : (i == 1 ? y : z); }
    double operator[](std::size_t i) const { return i == 0 ? x : (i == 1 ? y : z); }
};

inline Vec2 operator+(Vec2 a, Vec2 b) { return {a.x + b.x, a.y + b.y}; }
inline Vec2 operator-(Vec2 a, Vec2 b) { return {a.x - b.x, a.y - b.y}; }
inline Vec2 operator*(Vec2 a, double s) { return {a.x * s, a.y * s}; }
inline Vec2 operator*(double s, Vec2 a) { return a * s; }
inline Vec2 operator/(Vec2 a, double s) { return {a.x / s, a.y / s}; }
inline double dot(Vec2 a, Vec2 b) { return a.x * b.x + a.y * b.y; }
inline double length(Vec2 a) { return std::sqrt(dot(a, a)); }
inline Vec2 normalize(Vec2 a) {
    const double len = length(a);
    return len > 1e-30 ? a / len : Vec2{};
}

inline Vec3 operator+(Vec3 a, Vec3 b) { return {a.x + b.x, a.y + b.y, a.z + b.z}; }
inline Vec3 operator-(Vec3 a, Vec3 b) { return {a.x - b.x, a.y - b.y, a.z - b.z}; }
inline Vec3 operator-(Vec3 a) { return {-a.x, -a.y, -a.z}; }
inline Vec3 operator*(Vec3 a, double s) { return {a.x * s, a.y * s, a.z * s}; }
inline Vec3 operator*(double s, Vec3 a) { return a * s; }
inline Vec3 operator/(Vec3 a, double s) { return {a.x / s, a.y / s, a.z / s}; }
inline Vec3 operator*(Vec3 a, Vec3 b) { return {a.x * b.x, a.y * b.y, a.z * b.z}; }
inline Vec3& operator+=(Vec3& a, Vec3 b) { a = a + b; return a; }
inline double dot(Vec3 a, Vec3 b) { return a.x * b.x + a.y * b.y + a.z * b.z; }
inline Vec3 cross(Vec3 a, Vec3 b) {
    return {a.y * b.z - a.z * b.y, a.z * b.x - a.x * b.z, a.x * b.y - a.y * b.x};
}
inline double length(Vec3 a) { return std::sqrt(dot(a, a)); }
inline Vec3 normalize(Vec3 a) {
    const double len = length(a);
    return len > 1e-30 ? a / len : Vec3{};
}
inline Vec3 absVec(Vec3 a) {
    return {std::abs(a.x), std::abs(a.y), std::abs(a.z)};
}
inline Vec3 maxVec(Vec3 a, Vec3 b) {
    return {std::max(a.x, b.x), std::max(a.y, b.y), std::max(a.z, b.z)};
}
inline Vec3 minVec(Vec3 a, Vec3 b) {
    return {std::min(a.x, b.x), std::min(a.y, b.y), std::min(a.z, b.z)};
}
inline double maxComponent(Vec3 a) { return std::max(a.x, std::max(a.y, a.z)); }
inline double clamp01(double value) { return std::min(1.0, std::max(0.0, value)); }
inline Vec3 clamp01(Vec3 value) {
    return {clamp01(value.x), clamp01(value.y), clamp01(value.z)};
}

inline double radians(double degrees) { return degrees * kPi / 180.0; }

struct Barycentric {
    double u = 0.0;
    double v = 0.0;
    double w = 0.0;
};

inline bool barycentric2D(Vec2 p, Vec2 a, Vec2 b, Vec2 c, Barycentric& out) {
    const Vec2 v0 = b - a;
    const Vec2 v1 = c - a;
    const Vec2 v2 = p - a;
    const double denom = v0.x * v1.y - v1.x * v0.y;
    if (std::abs(denom) <= 1e-18) return false;
    const double v = (v2.x * v1.y - v1.x * v2.y) / denom;
    const double w = (v0.x * v2.y - v2.x * v0.y) / denom;
    out = {1.0 - v - w, v, w};
    return out.u >= -1e-9 && out.v >= -1e-9 && out.w >= -1e-9;
}

inline Vec3 interpolate(Vec3 a, Vec3 b, Vec3 c, Barycentric bary) {
    return a * bary.u + b * bary.v + c * bary.w;
}

inline Vec2 interpolate(Vec2 a, Vec2 b, Vec2 c, Barycentric bary) {
    return a * bary.u + b * bary.v + c * bary.w;
}

struct Ray {
    Vec3 origin;
    Vec3 direction;
};

struct Mat4 {
    std::array<double, 16> m{};
};

inline double mix(double a, double b, double t) {
    return a + (b - a) * t;
}

inline Vec3 mix(Vec3 a, Vec3 b, double t) {
    return a + (b - a) * t;
}

inline double luminance(Vec3 c) {
    return 0.2126 * c.x + 0.7152 * c.y + 0.0722 * c.z;
}

}  // namespace cr

