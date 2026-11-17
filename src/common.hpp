#pragma once

#include <algorithm>
#include <array>
#include <bit>
#include <cmath>
#include <cstdint>
#include <limits>
#include <string>

namespace mosaic {

struct Vec3 {
    double x = 0.0;
    double y = 0.0;
    double z = 0.0;
};

inline Vec3 operator+(const Vec3& a, const Vec3& b) {
    return {a.x + b.x, a.y + b.y, a.z + b.z};
}
inline Vec3 operator-(const Vec3& a, const Vec3& b) {
    return {a.x - b.x, a.y - b.y, a.z - b.z};
}
inline Vec3 operator*(const Vec3& a, double s) {
    return {a.x * s, a.y * s, a.z * s};
}
inline Vec3 operator*(double s, const Vec3& a) {
    return a * s;
}
inline Vec3 operator/(const Vec3& a, double s) {
    return {a.x / s, a.y / s, a.z / s};
}
inline Vec3 operator-(const Vec3& a) {
    return {-a.x, -a.y, -a.z};
}
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
inline double lengthSquared(const Vec3& a) {
    return dot(a, a);
}
inline double length(const Vec3& a) {
    return std::sqrt(lengthSquared(a));
}
inline Vec3 normalize(const Vec3& a) {
    const double len = length(a);
    if (len <= 0.0 || !std::isfinite(len)) {
        return {};
    }
    return a / len;
}
inline Vec3 clamp(const Vec3& a, double lo, double hi) {
    return {
        std::clamp(a.x, lo, hi),
        std::clamp(a.y, lo, hi),
        std::clamp(a.z, lo, hi),
    };
}
inline bool finite(const Vec3& a) {
    return std::isfinite(a.x) && std::isfinite(a.y) && std::isfinite(a.z);
}

struct Rgb {
    double r = 0.0;
    double g = 0.0;
    double b = 0.0;
};

inline Rgb operator+(const Rgb& a, const Rgb& b) {
    return {a.r + b.r, a.g + b.g, a.b + b.b};
}
inline Rgb operator-(const Rgb& a, const Rgb& b) {
    return {a.r - b.r, a.g - b.g, a.b - b.b};
}
inline Rgb operator*(const Rgb& a, const Rgb& b) {
    return {a.r * b.r, a.g * b.g, a.b * b.b};
}
inline Rgb operator*(const Rgb& a, double s) {
    return {a.r * s, a.g * s, a.b * s};
}
inline Rgb operator*(double s, const Rgb& a) {
    return a * s;
}
inline Rgb operator/(const Rgb& a, double s) {
    return {a.r / s, a.g / s, a.b / s};
}
inline Rgb& operator+=(Rgb& a, const Rgb& b) {
    a.r += b.r;
    a.g += b.g;
    a.b += b.b;
    return a;
}
inline Rgb& operator*=(Rgb& a, double s) {
    a.r *= s;
    a.g *= s;
    a.b *= s;
    return a;
}
inline double luminance(const Rgb& c) {
    return 0.2126 * c.r + 0.7152 * c.g + 0.0722 * c.b;
}
inline Rgb clamp(const Rgb& c, double lo, double hi) {
    return {
        std::clamp(c.r, lo, hi),
        std::clamp(c.g, lo, hi),
        std::clamp(c.b, lo, hi),
    };
}
inline bool finite(const Rgb& c) {
    return std::isfinite(c.r) && std::isfinite(c.g) && std::isfinite(c.b);
}

inline uint64_t splitmix64(uint64_t value) {
    value += 0x9e3779b97f4a7c15ULL;
    value = (value ^ (value >> 30U)) * 0xbf58476d1ce4e5b9ULL;
    value = (value ^ (value >> 27U)) * 0x94d049bb133111ebULL;
    return value ^ (value >> 31U);
}

inline uint64_t hashCombine(uint64_t seed, uint64_t value) {
    return splitmix64(seed ^ (value + 0x9e3779b97f4a7c15ULL + (seed << 6U) + (seed >> 2U)));
}

inline double unitDouble(uint64_t bits) {
    return static_cast<double>(bits >> 11U) * (1.0 / 9007199254740992.0);
}

inline uint64_t doubleBits(double value) {
    return std::bit_cast<uint64_t>(value);
}

inline std::string hex64(uint64_t value) {
    static constexpr char digits[] = "0123456789abcdef";
    std::string out(16, '0');
    for (int i = 15; i >= 0; --i) {
        out[static_cast<size_t>(i)] = digits[value & 0xFU];
        value >>= 4U;
    }
    return out;
}

inline constexpr double kPi = 3.141592653589793238462643383279502884;
inline constexpr double kEpsilon = 1.0e-9;

}  // namespace mosaic
