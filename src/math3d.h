#pragma once

#include <algorithm>
#include <cmath>
#include <limits>

namespace caisson {

constexpr double kPi = 3.141592653589793238462643383279502884;
constexpr double kEpsilon = 1.0e-12;

struct Vec2 {
    double x = 0.0;
    double y = 0.0;

    Vec2() = default;
    Vec2(double x_value, double y_value) : x(x_value), y(y_value) {}
};

inline Vec2 operator+(const Vec2& a, const Vec2& b) { return {a.x + b.x, a.y + b.y}; }
inline Vec2 operator-(const Vec2& a, const Vec2& b) { return {a.x - b.x, a.y - b.y}; }
inline Vec2 operator*(const Vec2& a, double s) { return {a.x * s, a.y * s}; }

struct Vec3 {
    double x = 0.0;
    double y = 0.0;
    double z = 0.0;

    Vec3() = default;
    Vec3(double x_value, double y_value, double z_value)
        : x(x_value), y(y_value), z(z_value) {}

    double operator[](int index) const {
        if (index == 0) return x;
        if (index == 1) return y;
        return z;
    }
};

inline Vec3 operator+(const Vec3& a, const Vec3& b) {
    return {a.x + b.x, a.y + b.y, a.z + b.z};
}
inline Vec3 operator-(const Vec3& a, const Vec3& b) {
    return {a.x - b.x, a.y - b.y, a.z - b.z};
}
inline Vec3 operator-(const Vec3& a) { return {-a.x, -a.y, -a.z}; }
inline Vec3 operator*(const Vec3& a, double s) { return {a.x * s, a.y * s, a.z * s}; }
inline Vec3 operator*(double s, const Vec3& a) { return a * s; }
inline Vec3 operator*(const Vec3& a, const Vec3& b) {
    return {a.x * b.x, a.y * b.y, a.z * b.z};
}
inline Vec3 operator/(const Vec3& a, double s) { return {a.x / s, a.y / s, a.z / s}; }
inline Vec3& operator+=(Vec3& a, const Vec3& b) {
    a.x += b.x;
    a.y += b.y;
    a.z += b.z;
    return a;
}
inline Vec3& operator*=(Vec3& a, double s) {
    a.x *= s;
    a.y *= s;
    a.z *= s;
    return a;
}

inline double dot(const Vec3& a, const Vec3& b) {
    return a.x * b.x + a.y * b.y + a.z * b.z;
}
inline Vec3 cross(const Vec3& a, const Vec3& b) {
    return {
        a.y * b.z - a.z * b.y,
        a.z * b.x - a.x * b.z,
        a.x * b.y - a.y * b.x,
    };
}
inline double lengthSquared(const Vec3& a) { return dot(a, a); }
inline double length(const Vec3& a) { return std::sqrt(lengthSquared(a)); }
inline Vec3 normalize(const Vec3& a) {
    const double len = length(a);
    if (len <= kEpsilon) return {0.0, 0.0, 0.0};
    return a / len;
}
inline Vec3 clamp01(const Vec3& a) {
    return {
        std::clamp(a.x, 0.0, 1.0),
        std::clamp(a.y, 0.0, 1.0),
        std::clamp(a.z, 0.0, 1.0),
    };
}
inline Vec3 lerp(const Vec3& a, const Vec3& b, double t) {
    return a + (b - a) * t;
}
inline Vec2 lerp(const Vec2& a, const Vec2& b, double t) {
    return a + (b - a) * t;
}
inline double luminance(const Vec3& c) {
    return 0.2126 * c.x + 0.7152 * c.y + 0.0722 * c.z;
}

struct Plane {
    Vec3 point;
    Vec3 normal;
};

inline bool intersectRayPlane(
    const Vec3& origin,
    const Vec3& direction,
    const Plane& plane,
    double& t) {
    const double denom = dot(plane.normal, direction);
    if (std::abs(denom) <= 1.0e-12) return false;
    t = dot(plane.point - origin, plane.normal) / denom;
    return std::isfinite(t);
}

inline bool intersectRayTriangle(
    const Vec3& origin,
    const Vec3& direction,
    const Vec3& a,
    const Vec3& b,
    const Vec3& c,
    double& t) {
    const Vec3 edge1 = b - a;
    const Vec3 edge2 = c - a;
    const Vec3 pvec = cross(direction, edge2);
    const double det = dot(edge1, pvec);
    if (std::abs(det) < 1.0e-12) return false;
    const double inv_det = 1.0 / det;
    const Vec3 tvec = origin - a;
    const double u = dot(tvec, pvec) * inv_det;
    if (u < -1.0e-10 || u > 1.0 + 1.0e-10) return false;
    const Vec3 qvec = cross(tvec, edge1);
    const double v = dot(direction, qvec) * inv_det;
    if (v < -1.0e-10 || u + v > 1.0 + 1.0e-10) return false;
    t = dot(edge2, qvec) * inv_det;
    return t > 1.0e-10;
}

}  // namespace caisson

