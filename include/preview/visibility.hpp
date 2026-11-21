#pragma once

#include "preview/occlusion.hpp"
#include "preview/streaming.hpp"

#include <cstdint>
#include <vector>

namespace preview {

struct VisibilityMask {
    int width = 0;
    int height = 0;
    std::vector<std::uint32_t> pixels;
    std::vector<BlockId> visibleBlocks;
    std::vector<BlockId> uncertainBlocks;
    std::uint64_t snapshotVersion = 0;
    std::uint64_t pyramidVersion = 0;
    std::uint64_t loadContentHash = 0;
    std::uint64_t contentHash = 0;

    std::uint64_t nonEmptyPixels() const noexcept;
};

VisibilityMask composeVisibilityMask(
    const LoadSnapshot& snapshot,
    const Camera& camera,
    const DepthPyramid& pyramid,
    const std::vector<BlockVisibility>& decisions);

StatusCode validateVisibilityMaskBinding(const VisibilityMask& mask,
                                         const LoadSnapshot& snapshot,
                                         const DepthPyramid& pyramid,
                                         Diagnostic* diagnostic = nullptr);

}  // namespace preview
