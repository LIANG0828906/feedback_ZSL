#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "renderer.h"
#include "scene.h"

namespace caisson {

struct SnapshotBlock {
    BlockRect rect;
    std::string relativeFile;
    std::vector<std::uint8_t> bytes;
    std::string sha256;
    std::array<std::uint64_t, 4> colorSums{};
    std::array<std::uint64_t, 4> ownerSums{};
    BlockStats stats;
};

enum class SnapshotLoadStatus {
    None,
    Resumed,
    DiscardedCorrupt,
};

struct SnapshotState {
    SnapshotLoadStatus status = SnapshotLoadStatus::None;
    std::vector<SnapshotBlock> blocks;
};

std::string blockRelativePath(int blockIndex);
std::uint64_t sumChannels(
    const std::vector<std::uint8_t>& bytes,
    int offset,
    int stride);

bool commitBlockSnapshot(
    const Scene& scene,
    const std::filesystem::path& outputDirectory,
    std::vector<SnapshotBlock>& blocks,
    BlockResult result,
    std::string& error);

SnapshotLoadStatus loadSnapshot(
    const Scene& scene,
    const std::filesystem::path& outputDirectory,
    SnapshotState& state,
    std::string& error);

bool clearSnapshot(
    const std::filesystem::path& outputDirectory,
    std::string& error);

std::string snapshotSignature(
    const Scene& scene,
    const std::vector<SnapshotBlock>& blocks);

}  // namespace caisson

