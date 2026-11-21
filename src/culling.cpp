#include "preview/culling.hpp"

#include <algorithm>
#include <array>
#include <cmath>
#include <limits>

namespace preview {

namespace {

double boundsTransformError(const Bounds3& bounds, const Camera& camera) {
    const double scale = std::max(
        1.0, std::max(bounds.maxExtent(),
                      std::max(length(camera.position), length(bounds.center()))));
    return 32.0 * static_cast<double>(std::numeric_limits<float>::epsilon()) * scale;
}

}  // namespace

FrustumResult conservativeFrustumCull(const Bounds3& worldBounds,
                                      const Camera& camera) {
    FrustumResult result;
    result.boundaryError = boundsTransformError(worldBounds, camera);
    if (!worldBounds.valid() || camera.width <= 0 || camera.height <= 0) {
        result.classification = FrustumClassification::Outside;
        return result;
    }

    const Mat4 viewProjection = camera.projectionMatrix() * camera.viewMatrix();
    std::array<double, 6> maxPlaneValues;
    maxPlaneValues.fill(-std::numeric_limits<double>::infinity());
    std::array<double, 6> minPlaneValues;
    minPlaneValues.fill(std::numeric_limits<double>::infinity());
    bool anyPositiveW = false;

    for (const Vec3& corner : worldBounds.corners()) {
        const Vec4 clip = viewProjection * Vec4{corner.x, corner.y, corner.z, 1.0};
        if (clip.w > 1e-15) {
            anyPositiveW = true;
        }
        const std::array<double, 6> values = {
            clip.x + clip.w,
            clip.w - clip.x,
            clip.y + clip.w,
            clip.w - clip.y,
            clip.z + clip.w,
            clip.w - clip.z
        };
        for (std::size_t plane = 0; plane < values.size(); ++plane) {
            maxPlaneValues[plane] = std::max(maxPlaneValues[plane], values[plane]);
            minPlaneValues[plane] = std::min(minPlaneValues[plane], values[plane]);
        }
    }

    const double epsilon =
        std::max(1e-12, result.boundaryError * 8.0 /
                            std::max(camera.nearPlane, 1e-6));
    for (double maxValue : maxPlaneValues) {
        if (maxValue < -epsilon) {
            result.classification = FrustumClassification::Outside;
            result.minimumPlaneMargin = maxValue;
            return result;
        }
    }
    if (!anyPositiveW) {
        result.classification = FrustumClassification::Outside;
        result.minimumPlaneMargin = 0.0;
        return result;
    }

    double minimumMargin = std::numeric_limits<double>::infinity();
    bool fullyInside = true;
    for (std::size_t plane = 0; plane < minPlaneValues.size(); ++plane) {
        minimumMargin = std::min(minimumMargin, maxPlaneValues[plane]);
        if (minPlaneValues[plane] < -epsilon) {
            fullyInside = false;
        }
    }
    result.minimumPlaneMargin = minimumMargin;
    result.classification = fullyInside ? FrustumClassification::Inside
                                        : FrustumClassification::Intersecting;
    return result;
}

}  // namespace preview
