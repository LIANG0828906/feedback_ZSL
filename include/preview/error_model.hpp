#pragma once

#include "preview/math.hpp"

#include <cstdint>
#include <string>

namespace preview {

struct ErrorTerms {
    double bboxTransformErrorWorld = 0.0;
    double pyramidQuantizationErrorWorld = 0.0;
    double layerSwitchJumpWorld = 0.0;
    double frustumBoundaryErrorWorld = 0.0;
    double occlusionErrorUpperBoundWorld = 0.0;
};

double estimateBoundsTransformError(const Bounds3& bounds,
                                    const Camera& camera) noexcept;

struct FrameErrorSummary {
    std::string summaryVersion = "error-budget-v1";
    std::uint64_t snapshotVersion = 0;
    std::uint64_t pyramidVersion = 0;
    std::uint64_t loadContentHash = 0;
    std::uint64_t maskHash = 0;

    std::uint64_t totalBlocks = 0;
    std::uint64_t visibleBlocks = 0;
    std::uint64_t occludedBlocks = 0;
    std::uint64_t uncertainBlocks = 0;
    std::uint64_t culledBlocks = 0;
    std::uint64_t layerSwitches = 0;
    std::uint64_t criticalBlocks = 0;

    double bboxTransformErrorContribution = 0.0;
    double pyramidQuantizationErrorContribution = 0.0;
    double layerSwitchJumpContribution = 0.0;
    double frustumBoundaryErrorContribution = 0.0;
    double occlusionErrorUpperBoundMax = 0.0;
    std::uint64_t maskDeviationUpperBoundPixels = 0;

    std::string canonicalJson() const;
    std::uint64_t summaryHash() const;
};

}  // namespace preview
