#pragma once

#include <array>
#include <cmath>
#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

namespace sr {

constexpr double kPi = 3.141592653589793238462643383279502884;
constexpr uint32_t kNoParticleId = 0xffffffffU;

struct Vec2 {
    double x = 0.0;
    double y = 0.0;
};

struct Vec3 {
    double x = 0.0;
    double y = 0.0;
    double z = 0.0;
};

inline Vec2 operator+(Vec2 a, Vec2 b) { return {a.x + b.x, a.y + b.y}; }
inline Vec2 operator-(Vec2 a, Vec2 b) { return {a.x - b.x, a.y - b.y}; }
inline Vec2 operator*(Vec2 a, double s) { return {a.x * s, a.y * s}; }
inline Vec2 operator/(Vec2 a, double s) { return {a.x / s, a.y / s}; }
inline double dot(Vec2 a, Vec2 b) { return a.x * b.x + a.y * b.y; }
inline double cross(Vec2 a, Vec2 b) { return a.x * b.y - a.y * b.x; }
inline double length2(Vec2 a) { return dot(a, a); }
inline double length(Vec2 a) { return std::sqrt(length2(a)); }
inline Vec2 normalize(Vec2 a) {
    const double n = length(a);
    return n > 0.0 ? a / n : Vec2{};
}

inline Vec3 operator+(Vec3 a, Vec3 b) { return {a.x + b.x, a.y + b.y, a.z + b.z}; }
inline Vec3 operator-(Vec3 a, Vec3 b) { return {a.x - b.x, a.y - b.y, a.z - b.z}; }
inline Vec3 operator-(Vec3 a) { return {-a.x, -a.y, -a.z}; }
inline Vec3 operator*(Vec3 a, double s) { return {a.x * s, a.y * s, a.z * s}; }
inline Vec3 operator*(double s, Vec3 a) { return a * s; }
inline Vec3 operator/(Vec3 a, double s) { return {a.x / s, a.y / s, a.z / s}; }
inline Vec3 operator*(Vec3 a, Vec3 b) { return {a.x * b.x, a.y * b.y, a.z * b.z}; }
inline Vec3& operator+=(Vec3& a, Vec3 b) {
    a.x += b.x;
    a.y += b.y;
    a.z += b.z;
    return a;
}
inline double dot(Vec3 a, Vec3 b) { return a.x * b.x + a.y * b.y + a.z * b.z; }
inline Vec3 cross(Vec3 a, Vec3 b) {
    return {a.y * b.z - a.z * b.y, a.z * b.x - a.x * b.z, a.x * b.y - a.y * b.x};
}
inline double length2(Vec3 a) { return dot(a, a); }
inline double length(Vec3 a) { return std::sqrt(length2(a)); }
inline Vec3 normalize(Vec3 a) {
    const double n = length(a);
    return n > 0.0 ? a / n : Vec3{};
}

template <typename T>
inline T clampValue(T v, T lo, T hi) {
    return v < lo ? lo : (v > hi ? hi : v);
}

inline double saturate(double v) { return clampValue(v, 0.0, 1.0); }
inline double smoothstep(double edge0, double edge1, double x) {
    if (edge1 <= edge0) {
        return x < edge0 ? 0.0 : 1.0;
    }
    const double t = saturate((x - edge0) / (edge1 - edge0));
    return t * t * (3.0 - 2.0 * t);
}
inline double luminance(Vec3 c) { return 0.2126 * c.x + 0.7152 * c.y + 0.0722 * c.z; }
inline bool isFinite(double v) { return std::isfinite(v); }

std::string hexDigest(const std::array<uint8_t, 32>& bytes);
std::string formatDouble(double v, int precision = 12);
std::string formatInt(std::int64_t v);

bool readFileBytes(const std::filesystem::path& path, std::vector<uint8_t>& out, std::string& error);
bool writeFileBytesAtomic(const std::filesystem::path& path, const std::vector<uint8_t>& bytes,
                          std::string& error);
bool ensureDirectory(const std::filesystem::path& path, std::string& error);

void appendU8(std::vector<uint8_t>& out, uint8_t v);
void appendU32(std::vector<uint8_t>& out, uint32_t v);
void appendU64(std::vector<uint8_t>& out, uint64_t v);
void appendDouble(std::vector<uint8_t>& out, double v);
bool readU8(const std::vector<uint8_t>& in, size_t& offset, uint8_t& v);
bool readU32(const std::vector<uint8_t>& in, size_t& offset, uint32_t& v);
bool readU64(const std::vector<uint8_t>& in, size_t& offset, uint64_t& v);
bool readDouble(const std::vector<uint8_t>& in, size_t& offset, double& v);

}  // namespace sr

