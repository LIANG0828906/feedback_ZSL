#pragma once

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <limits>
#include <string>

namespace sl {

constexpr double kPi = 3.141592653589793238462643383279502884;
constexpr double kEpsilon = 1.0e-9;

inline double clamp(double value, double low, double high) {
    return std::max(low, std::min(high, value));
}

inline double radians(double degrees) {
    return degrees * kPi / 180.0;
}

inline double degrees(double radiansValue) {
    return radiansValue * 180.0 / kPi;
}

struct Vec2 {
    double x = 0.0;
    double y = 0.0;

    Vec2() = default;
    Vec2(double xValue, double yValue) : x(xValue), y(yValue) {}
};

struct Vec3 {
    double x = 0.0;
    double y = 0.0;
    double z = 0.0;

    Vec3() = default;
    Vec3(double xValue, double yValue, double zValue) : x(xValue), y(yValue), z(zValue) {}

    Vec3 operator+(const Vec3& rhs) const { return {x + rhs.x, y + rhs.y, z + rhs.z}; }
    Vec3 operator-(const Vec3& rhs) const { return {x - rhs.x, y - rhs.y, z - rhs.z}; }
    Vec3 operator-() const { return {-x, -y, -z}; }
    Vec3 operator*(double scalar) const { return {x * scalar, y * scalar, z * scalar}; }
    Vec3 operator/(double scalar) const { return {x / scalar, y / scalar, z / scalar}; }
    Vec3& operator+=(const Vec3& rhs) {
        x += rhs.x;
        y += rhs.y;
        z += rhs.z;
        return *this;
    }
    Vec3& operator*=(double scalar) {
        x *= scalar;
        y *= scalar;
        z *= scalar;
        return *this;
    }
};

inline Vec3 operator*(double scalar, const Vec3& value) {
    return value * scalar;
}

inline Vec3 componentMul(const Vec3& a, const Vec3& b) {
    return {a.x * b.x, a.y * b.y, a.z * b.z};
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

inline double lengthSquared(const Vec3& value) {
    return dot(value, value);
}

inline double length(const Vec3& value) {
    return std::sqrt(lengthSquared(value));
}

inline Vec3 normalize(const Vec3& value) {
    double len = length(value);
    if (len <= kEpsilon) {
        return {0.0, 0.0, 0.0};
    }
    return value / len;
}

inline Vec3 mix(const Vec3& a, const Vec3& b, double t) {
    return a * (1.0 - t) + b * t;
}

inline double luminance(const Vec3& value) {
    return 0.2126 * value.x + 0.7152 * value.y + 0.0722 * value.z;
}

inline Vec3 clampColor(const Vec3& value) {
    return {clamp(value.x, 0.0, 1.0), clamp(value.y, 0.0, 1.0), clamp(value.z, 0.0, 1.0)};
}

inline bool finite(double value) {
    return std::isfinite(value);
}

inline bool finite(const Vec3& value) {
    return finite(value.x) && finite(value.y) && finite(value.z);
}

struct Ray {
    Vec3 origin;
    Vec3 direction;
};

struct Triangle {
    Vec3 a;
    Vec3 b;
    Vec3 c;
    Vec3 normal;
    Vec3 centroid;

    void finish() {
        normal = normalize(cross(b - a, c - a));
        centroid = (a + b + c) / 3.0;
    }
};

struct Plane {
    Vec3 origin;
    Vec3 normal;
    Vec3 u;
    Vec3 v;
    double width = 0.0;
    double height = 0.0;

    void finish() {
        normal = normalize(normal);
        Vec3 helper = std::fabs(normal.z) < 0.9 ? Vec3(0.0, 0.0, 1.0) : Vec3(0.0, 1.0, 0.0);
        u = normalize(cross(helper, normal));
        v = normalize(cross(normal, u));
    }
};

inline bool rayTriangle(const Ray& ray, const Triangle& tri, double& t, double& u, double& v) {
    const Vec3 e1 = tri.b - tri.a;
    const Vec3 e2 = tri.c - tri.a;
    const Vec3 pvec = cross(ray.direction, e2);
    const double det = dot(e1, pvec);
    if (std::fabs(det) < 1.0e-12) {
        return false;
    }
    const double invDet = 1.0 / det;
    const Vec3 tvec = ray.origin - tri.a;
    u = dot(tvec, pvec) * invDet;
    if (u < -1.0e-10 || u > 1.0 + 1.0e-10) {
        return false;
    }
    const Vec3 qvec = cross(tvec, e1);
    v = dot(ray.direction, qvec) * invDet;
    if (v < -1.0e-10 || u + v > 1.0 + 1.0e-10) {
        return false;
    }
    t = dot(e2, qvec) * invDet;
    return t > 1.0e-8;
}

inline bool pointOnTriangle(const Vec3& point, const Triangle& tri, double tolerance) {
    const Vec3 n = tri.normal;
    if (std::fabs(dot(point - tri.a, n)) > tolerance) {
        return false;
    }
    const Vec3 c0 = cross(tri.b - tri.a, point - tri.a);
    const Vec3 c1 = cross(tri.c - tri.b, point - tri.b);
    const Vec3 c2 = cross(tri.a - tri.c, point - tri.c);
    const double d0 = dot(c0, n);
    const double d1 = dot(c1, n);
    const double d2 = dot(c2, n);
    return d0 >= -tolerance && d1 >= -tolerance && d2 >= -tolerance;
}

inline bool pointInPlaneRect(const Plane& plane, const Vec3& point, double margin = 0.0) {
    const Vec3 local = point - plane.origin;
    const double x = dot(local, plane.u);
    const double y = dot(local, plane.v);
    return std::fabs(x) <= plane.width * 0.5 + margin &&
           std::fabs(y) <= plane.height * 0.5 + margin;
}

inline bool intersectPlane(const Ray& ray, const Plane& plane, double& t) {
    const double denom = dot(ray.direction, plane.normal);
    if (std::fabs(denom) < 1.0e-12) {
        return false;
    }
    t = dot(plane.origin - ray.origin, plane.normal) / denom;
    return t > 1.0e-8;
}

inline bool intersectPlaneBothDirections(const Ray& ray, const Plane& plane, double& t) {
    const double denom = dot(ray.direction, plane.normal);
    if (std::fabs(denom) < 1.0e-12) {
        return false;
    }
    t = dot(plane.origin - ray.origin, plane.normal) / denom;
    return std::isfinite(t) && std::fabs(t) > 1.0e-10;
}

inline Vec3 closestPointOnSegment(const Vec3& p, const Vec3& a, const Vec3& b) {
    const Vec3 ab = b - a;
    const double denom = lengthSquared(ab);
    if (denom <= kEpsilon) {
        return a;
    }
    const double t = clamp(dot(p - a, ab) / denom, 0.0, 1.0);
    return a + ab * t;
}

inline std::uint64_t hashMix(std::uint64_t value) {
    value ^= value >> 30u;
    value *= 0xbf58476d1ce4e5b9ULL;
    value ^= value >> 27u;
    value *= 0x94d049bb133111ebULL;
    value ^= value >> 31u;
    return value;
}

inline std::uint64_t hashCombine(std::uint64_t seed, std::uint64_t value) {
    return hashMix(seed ^ (value + 0x9e3779b97f4a7c15ULL + (seed << 6u) + (seed >> 2u)));
}

inline std::uint64_t hashString(const std::string& text) {
    std::uint64_t hash = 1469598103934665603ULL;
    for (unsigned char c : text) {
        hash ^= c;
        hash *= 1099511628211ULL;
    }
    return hashMix(hash);
}

inline std::string hex64(std::uint64_t value) {
    static const char digits[] = "0123456789abcdef";
    std::string result(16, '0');
    for (int i = 15; i >= 0; --i) {
        result[static_cast<std::size_t>(i)] = digits[value & 0xfu];
        value >>= 4u;
    }
    return result;
}

} // namespace sl

