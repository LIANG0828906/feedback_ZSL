#pragma once

#include "preview/error.hpp"
#include "preview/math.hpp"
#include "preview/streaming.hpp"

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <vector>

namespace preview {

struct DepthPyramidConfig {
    int width = 320;
    int height = 180;
    int quantizationBits = 20;
    int maxLevels = 12;
    std::size_t cancellationCheckpointBlocks = 32;
};

struct DepthPyramidLayer {
    int level = 0;
    int width = 0;
    int height = 0;
    double quantStep = 0.0;
    std::vector<std::uint32_t> codes;
};

struct PyramidKey {
    std::uint64_t snapshotVersion = 0;
    std::uint64_t snapshotContentHash = 0;
    double matrix[16]{};
    double nearPlane = 0.0;
    double farPlane = 0.0;
    double quantizedDepthMax = 0.0;
    double baseStep = 0.0;
    int width = 0;
    int height = 0;
    int levels = 0;
    std::uint32_t quantizationBits = 0;

    bool operator==(const PyramidKey& other) const noexcept;
    bool operator!=(const PyramidKey& other) const noexcept {
        return !(*this == other);
    }
    std::uint64_t hash() const noexcept;
};

struct PyramidProbe {
    int level = 0;
    int x0 = 0;
    int y0 = 0;
    int x1 = -1;
    int y1 = -1;
    double occluderDepth = 0.0;
    double quantStep = 0.0;
};

class DepthPyramid {
public:
    std::uint64_t version = 0;
    PyramidKey key;
    std::vector<DepthPyramidLayer> layers;

    PyramidProbe probe(const ScreenBounds& screenBounds, int level) const noexcept;
    double maxQuantizationError() const noexcept;
    std::uint64_t contentHash() const noexcept;
};

struct DepthPyramidBuildResult {
    std::shared_ptr<const DepthPyramid> pyramid;
    Diagnostic diagnostic;

    bool ok() const noexcept { return pyramid != nullptr && diagnostic.ok(); }
    explicit operator bool() const noexcept { return ok(); }
};

PyramidKey makeDepthPyramidKey(const LoadSnapshot& snapshot,
                               const Camera& camera,
                               const DepthPyramidConfig& config) noexcept;

DepthPyramidBuildResult buildDepthPyramid(
    const LoadSnapshot& snapshot,
    const Camera& camera,
    const DepthPyramidConfig& config,
    std::uint64_t pyramidVersion,
    const std::atomic<bool>* cancelFlag = nullptr);

struct PyramidRebuildReport {
    bool rebuilt = false;
    std::uint64_t oldVersion = 0;
    std::uint64_t newVersion = 0;
    int oldLevels = 0;
    int newLevels = 0;
    double oldDepthMin = 0.0;
    double oldDepthMax = 0.0;
    double newDepthMin = 0.0;
    double newDepthMax = 0.0;
    double depthRangeDifference = 0.0;
    double maxQuantizationError = 0.0;
    std::uint64_t oldConfigHash = 0;
    std::uint64_t newConfigHash = 0;
};

PyramidRebuildReport makeRebuildReport(
    const std::shared_ptr<const DepthPyramid>& previous,
    const DepthPyramid& current);

}  // namespace preview
