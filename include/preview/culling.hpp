#pragma once

#include "preview/math.hpp"

namespace preview {

enum class FrustumClassification {
    Outside,
    Intersecting,
    Inside
};

struct FrustumResult {
    FrustumClassification classification = FrustumClassification::Outside;
    double minimumPlaneMargin = 0.0;
    double boundaryError = 0.0;
};

FrustumResult conservativeFrustumCull(const Bounds3& worldBounds,
                                      const Camera& camera);

}  // namespace preview
