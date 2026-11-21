#pragma once

#include <cstdint>
#include <filesystem>
#include <map>
#include <string>
#include <vector>

#include "chunkpipe/error.hpp"
#include "chunkpipe/model.hpp"

namespace chunkpipe {

struct RecoveredChunk {
    std::uint32_t sequence = 0;
    std::uint32_t sceneId = 0;
    std::uint16_t archiveVersion = 0;
    Chunk chunk;
    std::uint64_t contentHash = 0;
    std::uint64_t previousChainHash = 0;
    std::uint64_t chainHash = 0;
    std::uint8_t schedulerState = 0;
};

struct RecoveryState {
    std::vector<RecoveredChunk> chunks;
    std::uint32_t nextSequence = 1;
    std::uint64_t chainHash = 0;
};

struct CommitResult {
    bool wrote = false;
    std::uint32_t sequence = 0;
    std::uint64_t chainHash = 0;
};

class RecoveryStore {
public:
    explicit RecoveryStore(std::filesystem::path directory);

    Result<RecoveryState> recover(const SceneArchive& scene);
    Result<CommitResult> commitChunk(const SceneArchive& scene,
                                     const Chunk& chunk,
                                     std::uint64_t contentHash,
                                     std::uint8_t schedulerState);

    bool hasCommitted(std::uint32_t chunkId) const;
    const std::filesystem::path& directory() const;
    std::size_t writeCount() const;

private:
    std::filesystem::path directory_;
    bool recovered_ = false;
    std::uint32_t sceneId_ = 0;
    std::uint16_t sceneVersion_ = 0;
    std::uint32_t nextSequence_ = 1;
    std::uint64_t chainHash_ = 0;
    std::map<std::uint32_t, RecoveredChunk> committed_;
    std::size_t writeCount_ = 0;
};

} // namespace chunkpipe

