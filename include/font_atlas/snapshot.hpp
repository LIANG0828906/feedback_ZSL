#pragma once

#include "font_atlas/json.hpp"
#include "font_atlas/types.hpp"

#include <filesystem>
#include <optional>
#include <string>
#include <vector>

namespace font_atlas {

struct SnapshotConfig {
    std::string fontFingerprint;
    std::string glyphFingerprint;
    std::string glyph;
    int targetPixelSize = 0;
    double subpixelX = 0.0;
    double subpixelY = 0.0;
    FillRule fillRule = FillRule::NonZero;
    int quantizationStep = 0;
    int samplesPerAxis = 0;
    int tileWidth = 0;
    int tileHeight = 0;
    int bitmapWidth = 0;
    int bitmapHeight = 0;
    int seamTolerance = 0;
    int parallelism = 0;
    std::uint64_t globalBudgetBytes = 0;
    std::uint64_t perTaskBudgetBytes = 0;
    std::uint64_t sampleBatchSize = 0;
    std::vector<std::string> degradeSequence;
};

struct SnapshotState {
    int version = 1;
    std::string configHash;
    SnapshotConfig config;
    std::vector<ChunkPlan> chunks;
    int committedChunks = 0;
    std::string hashChain;
};

std::string snapshotConfigHash(const SnapshotConfig& config);
std::string snapshotHashChain(const std::string& configHash,
                              const std::vector<ChunkPlan>& chunks);
Json snapshotToJson(const SnapshotState& state);

Result<void> saveSnapshot(const std::filesystem::path& directory, SnapshotState state);
Result<std::optional<SnapshotState>> loadSnapshot(const std::filesystem::path& directory,
                                                  const std::string& expectedConfigHash);
Result<void> saveChunk(const std::filesystem::path& directory, int index,
                       const std::vector<std::uint8_t>& coverage);
Result<std::vector<std::uint8_t>> loadChunk(const std::filesystem::path& directory, int index);

}  // namespace font_atlas

