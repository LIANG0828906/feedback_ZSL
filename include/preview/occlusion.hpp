#pragma once

#include "preview/culling.hpp"
#include "preview/depth_pyramid.hpp"
#include "preview/error_model.hpp"

namespace preview {

enum class VisibilityDecision {
    Visible,
    Occluded,
    Uncertain,
    FrustumCulled
};

const char* visibilityDecisionName(VisibilityDecision decision) noexcept;

struct OcclusionConfig {
    int initialCoverageTiles = 4;
};

struct BlockVisibility {
    BlockId id = kInvalidBlockId;
    VisibilityDecision decision = VisibilityDecision::FrustumCulled;
    StatusCode code = StatusCode::E_OK;
    FrustumResult frustum;
    ScreenBounds screenBounds;
    double marginLower = 0.0;
    double marginUpper = 0.0;
    double rawMargin = 0.0;
    double occluderDepth = 0.0;
    double quantizedDepthMax = 0.0;
    int selectedLevel = 0;
    int layersVisited = 0;
    int layerSwitches = 0;
    ErrorTerms errors;
};

BlockVisibility evaluateBlockVisibility(const SceneBlock& block,
                                        const Camera& camera,
                                        const DepthPyramid& pyramid,
                                        const OcclusionConfig& config = {});

}  // namespace preview
