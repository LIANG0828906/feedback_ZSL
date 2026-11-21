#pragma once

#include "preview/depth_pyramid.hpp"
#include "preview/error_model.hpp"
#include "preview/streaming.hpp"

#include <cstdint>
#include <string>
#include <vector>

namespace preview {

constexpr std::uint32_t kSnapshotFormatVersion = 1;

struct SnapshotLayerRecord {
    int level = 0;
    int width = 0;
    int height = 0;
    double quantStep = 0.0;
    double depthMin = 0.0;
    double depthMax = 0.0;
};

struct SnapshotBlockRecord {
    BlockId id = kInvalidBlockId;
    std::uint64_t contentHash = 0;
};

struct SnapshotDocument {
    std::uint32_t formatVersion = kSnapshotFormatVersion;
    std::uint64_t snapshotVersion = 0;
    std::uint64_t pyramidVersion = 0;
    Camera camera;
    Mat4 viewMatrix = Mat4::identity();
    std::uint64_t sceneContentHash = 0;
    std::uint64_t loadContentHash = 0;
    std::vector<SnapshotBlockRecord> blocks;
    std::vector<SnapshotLayerRecord> layers;
    std::uint64_t pyramidContentHash = 0;
    std::uint64_t maskHash = 0;
    int maskWidth = 0;
    int maskHeight = 0;
    FrameErrorSummary summary;
};

struct SnapshotCurrentState {
    Camera camera;
    std::vector<BlockId> loadedBlockIds;
    std::uint64_t loadContentHash = 0;
};

SnapshotCurrentState currentStateFromSnapshot(const LoadSnapshot& snapshot,
                                              const Camera& camera);

struct SnapshotSaveResult {
    StatusCode code = StatusCode::E_OK;
    Diagnostic diagnostic;

    bool ok() const noexcept { return code == StatusCode::E_OK; }
    explicit operator bool() const noexcept { return ok(); }
};

struct SnapshotLoadResult {
    StatusCode code = StatusCode::E_OK;
    Diagnostic diagnostic;
    SnapshotDocument document;
    bool requiresRerender = false;
    std::vector<std::string> changes;

    bool ok() const noexcept { return code == StatusCode::E_OK; }
    explicit operator bool() const noexcept { return ok(); }
};

SnapshotDocument makeSnapshotDocument(const LoadSnapshot& loadSnapshot,
                                      const Camera& camera,
                                      const DepthPyramid& pyramid,
                                      std::uint64_t maskHash,
                                      int maskWidth,
                                      int maskHeight,
                                      const FrameErrorSummary& summary);

SnapshotSaveResult saveSnapshotAtomic(const std::string& path,
                                      const SnapshotDocument& document);
SnapshotLoadResult loadSnapshot(const std::string& path,
                                const SnapshotCurrentState& currentState);

std::vector<std::uint8_t> encodeSnapshotDocument(
    const SnapshotDocument& document);
SnapshotLoadResult decodeSnapshotDocument(
    const std::vector<std::uint8_t>& bytes);

}  // namespace preview
