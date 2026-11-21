#include "preview/math.hpp"

#include <algorithm>
#include <cmath>

namespace preview {

Vec3 normalize(const Vec3& value) noexcept {
    const double len = length(value);
    if (len <= std::numeric_limits<double>::min()) {
        return {0.0, 0.0, 0.0};
    }
    return value / len;
}

Mat4 Mat4::identity() noexcept {
    Mat4 result;
    result.at(0, 0) = 1.0;
    result.at(1, 1) = 1.0;
    result.at(2, 2) = 1.0;
    result.at(3, 3) = 1.0;
    return result;
}

Mat4 operator*(const Mat4& a, const Mat4& b) noexcept {
    Mat4 result;
    for (int row = 0; row < 4; ++row) {
        for (int column = 0; column < 4; ++column) {
            double sum = 0.0;
            for (int k = 0; k < 4; ++k) {
                sum += a.at(row, k) * b.at(k, column);
            }
            result.at(row, column) = sum;
        }
    }
    return result;
}

Vec4 operator*(const Mat4& matrix, const Vec4& value) noexcept {
    Vec4 result;
    result.x = matrix.at(0, 0) * value.x + matrix.at(0, 1) * value.y +
               matrix.at(0, 2) * value.z + matrix.at(0, 3) * value.w;
    result.y = matrix.at(1, 0) * value.x + matrix.at(1, 1) * value.y +
               matrix.at(1, 2) * value.z + matrix.at(1, 3) * value.w;
    result.z = matrix.at(2, 0) * value.x + matrix.at(2, 1) * value.y +
               matrix.at(2, 2) * value.z + matrix.at(2, 3) * value.w;
    result.w = matrix.at(3, 0) * value.x + matrix.at(3, 1) * value.y +
               matrix.at(3, 2) * value.z + matrix.at(3, 3) * value.w;
    return result;
}

Mat4 perspectiveMatrix(double fovYDegrees, double aspect,
                       double nearPlane, double farPlane) noexcept {
    Mat4 result;
    const double radians = fovYDegrees * (3.14159265358979323846 / 180.0);
    const double f = 1.0 / std::tan(radians * 0.5);
    result.at(0, 0) = f / aspect;
    result.at(1, 1) = f;
    result.at(2, 2) = (farPlane + nearPlane) / (nearPlane - farPlane);
    result.at(2, 3) = (2.0 * farPlane * nearPlane) / (nearPlane - farPlane);
    result.at(3, 2) = -1.0;
    return result;
}

Mat4 lookAtMatrix(const Vec3& eye, const Vec3& target, const Vec3& up) noexcept {
    const Vec3 forward = normalize(target - eye);
    const Vec3 side = normalize(cross(forward, up));
    const Vec3 correctedUp = cross(side, forward);

    Mat4 result = Mat4::identity();
    result.at(0, 0) = side.x;
    result.at(0, 1) = side.y;
    result.at(0, 2) = side.z;
    result.at(0, 3) = -dot(side, eye);
    result.at(1, 0) = correctedUp.x;
    result.at(1, 1) = correctedUp.y;
    result.at(1, 2) = correctedUp.z;
    result.at(1, 3) = -dot(correctedUp, eye);
    result.at(2, 0) = -forward.x;
    result.at(2, 1) = -forward.y;
    result.at(2, 2) = -forward.z;
    result.at(2, 3) = dot(forward, eye);
    return result;
}

bool Bounds3::valid() const noexcept {
    return std::isfinite(min.x) && std::isfinite(min.y) && std::isfinite(min.z) &&
           std::isfinite(max.x) && std::isfinite(max.y) && std::isfinite(max.z) &&
           min.x <= max.x && min.y <= max.y && min.z <= max.z;
}

Vec3 Bounds3::center() const noexcept {
    return (min + max) * 0.5;
}

Vec3 Bounds3::extent() const noexcept {
    return max - min;
}

double Bounds3::maxExtent() const noexcept {
    const Vec3 e = extent();
    return std::max(e.x, std::max(e.y, e.z));
}

std::array<Vec3, 8> Bounds3::corners() const noexcept {
    return {{
        {min.x, min.y, min.z},
        {max.x, min.y, min.z},
        {min.x, max.y, min.z},
        {max.x, max.y, min.z},
        {min.x, min.y, max.z},
        {max.x, min.y, max.z},
        {min.x, max.y, max.z},
        {max.x, max.y, max.z}
    }};
}

bool Bounds3::contains(const Bounds3& other, double tolerance) const noexcept {
    return min.x - tolerance <= other.min.x && min.y - tolerance <= other.min.y &&
           min.z - tolerance <= other.min.z && max.x + tolerance >= other.max.x &&
           max.y + tolerance >= other.max.y && max.z + tolerance >= other.max.z;
}

Bounds3 Bounds3::transformed(const Mat4& matrix) const noexcept {
    Bounds3 result;
    for (const Vec3& corner : corners()) {
        const Vec4 transformed = matrix * Vec4{corner.x, corner.y, corner.z, 1.0};
        if (std::abs(transformed.w) > 1e-15) {
            const Vec3 point{transformed.x / transformed.w,
                             transformed.y / transformed.w,
                             transformed.z / transformed.w};
            result.min.x = std::min(result.min.x, point.x);
            result.min.y = std::min(result.min.y, point.y);
            result.min.z = std::min(result.min.z, point.z);
            result.max.x = std::max(result.max.x, point.x);
            result.max.y = std::max(result.max.y, point.y);
            result.max.z = std::max(result.max.z, point.z);
        }
    }
    return result;
}

void Bounds3::expand(const Bounds3& other) noexcept {
    if (!other.valid()) {
        return;
    }
    min.x = std::min(min.x, other.min.x);
    min.y = std::min(min.y, other.min.y);
    min.z = std::min(min.z, other.min.z);
    max.x = std::max(max.x, other.max.x);
    max.y = std::max(max.y, other.max.y);
    max.z = std::max(max.z, other.max.z);
}

std::uint64_t ScreenBounds::area() const noexcept {
    if (!intersects || width() <= 0 || height() <= 0) {
        return 0;
    }
    return static_cast<std::uint64_t>(width()) * static_cast<std::uint64_t>(height());
}

ScreenBounds projectBounds(const Bounds3& worldBounds,
                           const Mat4& view,
                           const Mat4& projection,
                           int width,
                           int height) noexcept {
    ScreenBounds result;
    if (!worldBounds.valid() || width <= 0 || height <= 0) {
        return result;
    }

    const Mat4 viewProjection = projection * view;
    double minX = std::numeric_limits<double>::infinity();
    double maxX = -std::numeric_limits<double>::infinity();
    double minY = std::numeric_limits<double>::infinity();
    double maxY = -std::numeric_limits<double>::infinity();
    double minDepth = std::numeric_limits<double>::infinity();
    double maxDepth = -std::numeric_limits<double>::infinity();
    bool anySurfacePoint = false;

    for (const Vec3& corner : worldBounds.corners()) {
        const Vec4 cameraPoint = view * Vec4{corner.x, corner.y, corner.z, 1.0};
        const double viewDepth = -cameraPoint.z;
        if (!(viewDepth > 1e-15)) {
            continue;
        }
        anySurfacePoint = true;
        minDepth = std::min(minDepth, viewDepth);
        maxDepth = std::max(maxDepth, viewDepth);

        const Vec4 clip = viewProjection * Vec4{corner.x, corner.y, corner.z, 1.0};
        if (std::abs(clip.w) <= 1e-15) {
            continue;
        }
        const double ndcX = clip.x / clip.w;
        const double ndcY = clip.y / clip.w;
        minX = std::min(minX, ndcX);
        maxX = std::max(maxX, ndcX);
        minY = std::min(minY, ndcY);
        maxY = std::max(maxY, ndcY);
    }

    if (!anySurfacePoint || !std::isfinite(minX) || !std::isfinite(minY)) {
        return result;
    }

    const double pixelX0 = (minX * 0.5 + 0.5) * static_cast<double>(width);
    const double pixelX1 = (maxX * 0.5 + 0.5) * static_cast<double>(width);
    const double pixelY0 = (1.0 - maxY * 0.5 - 0.5) * static_cast<double>(height);
    const double pixelY1 = (1.0 - minY * 0.5 - 0.5) * static_cast<double>(height);

    result.x0 = std::max(0, static_cast<int>(std::floor(pixelX0)));
    result.x1 = std::min(width - 1, static_cast<int>(std::ceil(pixelX1)));
    result.y0 = std::max(0, static_cast<int>(std::floor(pixelY0)));
    result.y1 = std::min(height - 1, static_cast<int>(std::ceil(pixelY1)));
    result.depthMin = std::max(0.0, minDepth);
    result.depthMax = maxDepth;
    result.intersects = result.x0 <= result.x1 && result.y0 <= result.y1 &&
                        maxDepth > 0.0;
    return result;
}

}  // namespace preview
