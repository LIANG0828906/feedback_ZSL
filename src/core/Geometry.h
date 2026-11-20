#pragma once

#include <array>
#include <cmath>
#include <cstddef>
#include <limits>
#include <string>

namespace meshedit {

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

inline Vec3 operator+(const Vec3& a, const Vec3& b) { return {a.x + b.x, a.y + b.y, a.z + b.z}; }
inline Vec3 operator-(const Vec3& a, const Vec3& b) { return {a.x - b.x, a.y - b.y, a.z - b.z}; }
inline Vec3 operator*(const Vec3& a, double s) { return {a.x * s, a.y * s, a.z * s}; }
inline Vec3 operator*(double s, const Vec3& a) { return a * s; }
inline Vec3 operator/(const Vec3& a, double s) { return {a.x / s, a.y / s, a.z / s}; }
inline Vec3 operator-(const Vec3& a) { return {-a.x, -a.y, -a.z}; }
inline Vec3& operator+=(Vec3& a, const Vec3& b) { a = a + b; return a; }
inline Vec3& operator-=(Vec3& a, const Vec3& b) { a = a - b; return a; }
inline Vec3& operator*=(Vec3& a, double s) { a = a * s; return a; }

inline double dot(const Vec3& a, const Vec3& b) { return a.x * b.x + a.y * b.y + a.z * b.z; }
inline Vec3 cross(const Vec3& a, const Vec3& b) {
    return {a.y * b.z - a.z * b.y, a.z * b.x - a.x * b.z, a.x * b.y - a.y * b.x};
}
inline double lengthSquared(const Vec3& a) { return dot(a, a); }
inline double length(const Vec3& a) { return std::sqrt(lengthSquared(a)); }
inline Vec3 normalized(const Vec3& a) {
    const double n = length(a);
    return n > 0.0 ? a / n : Vec3{};
}
inline bool finite(const Vec3& a) {
    return std::isfinite(a.x) && std::isfinite(a.y) && std::isfinite(a.z);
}
inline bool nearlyEqual(const Vec3& a, const Vec3& b, double tolerance) {
    return lengthSquared(a - b) <= tolerance * tolerance;
}

struct Aabb {
    Vec3 min{std::numeric_limits<double>::infinity(),
             std::numeric_limits<double>::infinity(),
             std::numeric_limits<double>::infinity()};
    Vec3 max{-std::numeric_limits<double>::infinity(),
             -std::numeric_limits<double>::infinity(),
             -std::numeric_limits<double>::infinity()};

    bool valid() const { return min.x <= max.x && min.y <= max.y && min.z <= max.z; }
    void expand(const Vec3& p) {
        if (p.x < min.x) min.x = p.x;
        if (p.y < min.y) min.y = p.y;
        if (p.z < min.z) min.z = p.z;
        if (p.x > max.x) max.x = p.x;
        if (p.y > max.y) max.y = p.y;
        if (p.z > max.z) max.z = p.z;
    }
    Vec3 center() const { return (min + max) * 0.5; }
    Vec3 extent() const { return max - min; }
};

struct RayTriangleHit {
    bool hit = false;
    double t = 0.0;
};

Vec3 closestPointOnTriangle(const Vec3& p, const Vec3& a, const Vec3& b, const Vec3& c);
double pointAabbDistanceSquared(const Vec3& p, const Aabb& box);
RayTriangleHit rayTriangle(const Vec3& origin, const Vec3& direction,
                           const Vec3& a, const Vec3& b, const Vec3& c);

std::string formatDouble(double value, int precision = 17);

} // namespace meshedit
