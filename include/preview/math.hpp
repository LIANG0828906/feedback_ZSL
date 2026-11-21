#pragma once

#include <array>
#include <cmath>
#include <cstddef>
#include <cstdint>
#include <limits>

namespace preview {

struct Vec2 {
    double x = 0.0;
    double y = 0.0;
};

struct Vec3 {
    double x = 0.0;
    double y = 0.0;
    double z = 0.0;

    double operator[](int index) const noexcept {
        return index == 0 ? x : (index == 1 ? y : z);
    }
    double& operator[](int index) noexcept {
        return index == 0 ? x : (index == 1 ? y : z);
    }
};

struct Vec4 {
    double x = 0.0;
    double y = 0.0;
    double z = 0.0;
    double w = 0.0;

    double operator[](int index) const noexcept {
        switch (index) {
            case 0: return x;
            case 1: return y;
            case 2: return z;
            default: return w;
        }
    }
};

inline Vec3 operator+(const Vec3& a, const Vec3& b) noexcept {
    return {a.x + b.x, a.y + b.y, a.z + b.z};
}
inline Vec3 operator-(const Vec3& a, const Vec3& b) noexcept {
    return {a.x - b.x, a.y - b.y, a.z - b.z};
}
inline Vec3 operator*(const Vec3& a, double s) noexcept {
    return {a.x * s, a.y * s, a.z * s};
}
inline Vec3 operator/(const Vec3& a, double s) noexcept {
    return {a.x / s, a.y / s, a.z / s};
}
inline double dot(const Vec3& a, const Vec3& b) noexcept {
    return a.x * b.x + a.y * b.y + a.z * b.z;
}
inline Vec3 cross(const Vec3& a, const Vec3& b) noexcept {
    return {a.y * b.z - a.z * b.y,
            a.z * b.x - a.x * b.z,
            a.x * b.y - a.y * b.x};
}
inline double lengthSquared(const Vec3& v) noexcept { return dot(v, v); }
inline double length(const Vec3& v) noexcept { return std::sqrt(lengthSquared(v)); }
Vec3 normalize(const Vec3& value) noexcept;

struct Mat4 {
    std::array<double, 16> m{};

    static Mat4 identity() noexcept;
    double& at(int row, int column) noexcept { return m[static_cast<std::size_t>(column * 4 + row)]; }
    double at(int row, int column) const noexcept { return m[static_cast<std::size_t>(column * 4 + row)]; }
};

Mat4 operator*(const Mat4& a, const Mat4& b) noexcept;
Vec4 operator*(const Mat4& matrix, const Vec4& value) noexcept;

Mat4 perspectiveMatrix(double fovYDegrees, double aspect,
                       double nearPlane, double farPlane) noexcept;
Mat4 lookAtMatrix(const Vec3& eye, const Vec3& target, const Vec3& up) noexcept;

struct Bounds3 {
    Vec3 min{std::numeric_limits<double>::infinity(),
             std::numeric_limits<double>::infinity(),
             std::numeric_limits<double>::infinity()};
    Vec3 max{-std::numeric_limits<double>::infinity(),
             -std::numeric_limits<double>::infinity(),
             -std::numeric_limits<double>::infinity()};

    bool valid() const noexcept;
    Vec3 center() const noexcept;
    Vec3 extent() const noexcept;
    double maxExtent() const noexcept;
    std::array<Vec3, 8> corners() const noexcept;
    bool contains(const Bounds3& other, double tolerance = 0.0) const noexcept;
    Bounds3 transformed(const Mat4& matrix) const noexcept;
    void expand(const Bounds3& other) noexcept;
};

struct Camera {
    Vec3 position{0.0, 0.0, 5.0};
    Vec3 target{0.0, 0.0, 0.0};
    Vec3 up{0.0, 1.0, 0.0};
    double fovYDegrees = 60.0;
    double nearPlane = 0.1;
    double farPlane = 100.0;
    int width = 320;
    int height = 180;

    Mat4 viewMatrix() const noexcept { return lookAtMatrix(position, target, up); }
    Mat4 projectionMatrix() const noexcept {
        return perspectiveMatrix(fovYDegrees,
                                 static_cast<double>(width) / static_cast<double>(height),
                                 nearPlane, farPlane);
    }
};

struct ScreenBounds {
    int x0 = 0;
    int y0 = 0;
    int x1 = -1;
    int y1 = -1;
    double depthMin = 0.0;
    double depthMax = 0.0;
    bool intersects = false;

    int width() const noexcept { return intersects ? x1 - x0 + 1 : 0; }
    int height() const noexcept { return intersects ? y1 - y0 + 1 : 0; }
    std::uint64_t area() const noexcept;
};

ScreenBounds projectBounds(const Bounds3& worldBounds,
                           const Mat4& view,
                           const Mat4& projection,
                           int width,
                           int height) noexcept;

}  // namespace preview
