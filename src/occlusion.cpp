#include "preview/occlusion.hpp"

#include "preview/culling.hpp"

#include <algorithm>
#include <cmath>

namespace preview {

const char* visibilityDecisionName(VisibilityDecision decision) noexcept {
    switch (decision) {
        case VisibilityDecision::Visible: return "visible";
        case VisibilityDecision::Occluded: return "occluded";
        case VisibilityDecision::Uncertain: return "uncertain";
        case VisibilityDecision::FrustumCulled: return "frustum_culled";
    }
    return "unknown";
}

BlockVisibility evaluateBlockVisibility(const SceneBlock& block,
                                        const Camera& camera,
                                        const DepthPyramid& pyramid,
                                        const OcclusionConfig& config) {
    BlockVisibility result;
    result.id = block.id;
    result.frustum = conservativeFrustumCull(block.bounds, camera);
    result.quantizedDepthMax = pyramid.key.quantizedDepthMax;
    result.errors.bboxTransformErrorWorld =
        estimateBoundsTransformError(block.bounds, camera);
    result.errors.frustumBoundaryErrorWorld = result.frustum.boundaryError;

    if (result.frustum.classification == FrustumClassification::Outside) {
        result.decision = VisibilityDecision::FrustumCulled;
        result.code = StatusCode::E_OK;
        result.errors.occlusionErrorUpperBoundWorld =
            result.errors.bboxTransformErrorWorld +
            result.errors.frustumBoundaryErrorWorld;
        return result;
    }

    const Mat4 view = camera.viewMatrix();
    const Mat4 projection = camera.projectionMatrix();
    result.screenBounds =
        projectBounds(block.bounds, view, projection, camera.width, camera.height);
    if (!result.screenBounds.intersects) {
        result.decision = VisibilityDecision::FrustumCulled;
        result.code = StatusCode::E_OK;
        result.errors.occlusionErrorUpperBoundWorld =
            result.errors.bboxTransformErrorWorld +
            result.errors.frustumBoundaryErrorWorld;
        return result;
    }
    if (pyramid.layers.empty()) {
        result.decision = VisibilityDecision::Visible;
        result.code = StatusCode::E_OK;
        result.errors.occlusionErrorUpperBoundWorld =
            result.errors.bboxTransformErrorWorld +
            result.errors.frustumBoundaryErrorWorld;
        return result;
    }

    const int levelCount = static_cast<int>(pyramid.layers.size());
    int level = 0;
    const int targetTiles = std::max(1, config.initialCoverageTiles);
    while (level + 1 < levelCount) {
        const int divisor = 1 << (level + 1);
        const int coarseW =
            std::max(1, (result.screenBounds.width() + divisor - 1) / divisor);
        const int coarseH =
            std::max(1, (result.screenBounds.height() + divisor - 1) / divisor);
        if (coarseW * coarseH < targetTiles) {
            break;
        }
        ++level;
    }

    double previousMargin = 0.0;
    bool hasPreviousMargin = false;
    double accumulatedJump = 0.0;
    for (;;) {
        const PyramidProbe probe = pyramid.probe(result.screenBounds, level);
        ++result.layersVisited;
        const double rawMargin =
            result.screenBounds.depthMin - probe.occluderDepth;
        if (hasPreviousMargin) {
            const double jump = std::abs(rawMargin - previousMargin);
            accumulatedJump += jump;
            ++result.layerSwitches;
        }
        previousMargin = rawMargin;
        hasPreviousMargin = true;

        const double quantizationError = probe.quantStep * 0.5;
        const double uncertainty =
            quantizationError + result.errors.bboxTransformErrorWorld +
            result.errors.frustumBoundaryErrorWorld + accumulatedJump;
        const double lower = rawMargin - uncertainty;
        const double upper = rawMargin + uncertainty;

        result.selectedLevel = level;
        result.rawMargin = rawMargin;
        result.marginLower = lower;
        result.marginUpper = upper;
        result.occluderDepth = probe.occluderDepth;
        result.errors.pyramidQuantizationErrorWorld = quantizationError;
        result.errors.layerSwitchJumpWorld = accumulatedJump;
        result.errors.occlusionErrorUpperBoundWorld = uncertainty;

        if (lower > 0.0) {
            result.decision = VisibilityDecision::Occluded;
            result.code = StatusCode::E_OK;
            return result;
        }
        if (upper < 0.0) {
            result.decision = VisibilityDecision::Visible;
            result.code = StatusCode::E_OK;
            return result;
        }
        if (level == 0) {
            result.decision = VisibilityDecision::Uncertain;
            result.code = StatusCode::E_OCCLUSION_UNCERTAIN;
            return result;
        }
        --level;
    }
}

}  // namespace preview
