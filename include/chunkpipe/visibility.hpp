#pragma once

#include <atomic>
#include <cstdint>
#include <string>
#include <vector>

#include "chunkpipe/error.hpp"
#include "chunkpipe/model.hpp"

namespace chunkpipe {

struct Camera {
    Vec3 position{0.0f, 3.0f, -12.0f};
    Vec3 target{0.0f, 1.0f, 8.0f};
    Vec3 up{0.0f, 1.0f, 0.0f};
    float fovYDegrees = 60.0f;
    float aspect = 16.0f / 9.0f;
    float nearPlane = 0.1f;
    float farPlane = 100.0f;
    std::uint32_t width = 256;
    std::uint32_t height = 144;
};

struct CullStats {
    std::uint32_t sceneChunks = 0;
    std::uint32_t loadedChunks = 0;
    std::uint32_t frustumCulled = 0;
    std::uint32_t occlusionCulled = 0;
    std::uint32_t visibleChunks = 0;
    std::uint32_t criticalChunks = 0;
    std::uint32_t boundaryPairs = 0;
};

struct VisibilityRecord {
    std::uint32_t chunkId = 0;
    std::uint8_t flags = 0;
    float boundaryVisibleFraction = 0.0f;
};

struct BoundarySample {
    std::uint32_t chunkA = 0;
    std::uint32_t chunkB = 0;
    std::string segment;
    float fractionA = 0.0f;
    float fractionB = 0.0f;
    bool consistent = true;
};

struct FrameSnapshot {
    std::uint32_t publishVersion = 0;
    std::uint64_t contentHash = 0;
    std::uint16_t archiveVersion = 0;
    std::uint32_t sceneId = 0;
    std::vector<std::uint32_t> chunkIds;
    std::vector<std::uint8_t> visibilityMask;
    std::vector<std::uint32_t> loadedChunkIds;
    std::vector<VisibilityRecord> records;
    std::vector<BoundarySample> boundarySamples;
    CullStats stats;
};

enum VisibilityFlag : std::uint8_t {
    kMaskLoaded = 1u << 0,
    kMaskFrustumInside = 1u << 1,
    kMaskVisible = 1u << 2,
    kMaskCritical = 1u << 3,
    kMaskOccluder = 1u << 4
};

Result<FrameSnapshot> renderVisibility(const SceneArchive& scene,
                                       const std::vector<Chunk>& loadedChunks,
                                       const Camera& camera,
                                       float boundaryTolerance,
                                       const std::atomic<bool>* cancelRequested = nullptr);

Result<void> checkBoundaryConsistency(
    const std::vector<BoundarySample>& samples,
    float boundaryTolerance);

std::vector<Chunk> selectLoadedByPriority(const SceneArchive& scene,
                                          const Camera& camera,
                                          std::uint32_t maxChunks);

std::uint64_t snapshotContentHash(const FrameSnapshot& snapshot);
std::string maskHex(const FrameSnapshot& snapshot);

} // namespace chunkpipe

