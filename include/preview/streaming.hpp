#pragma once

#include "preview/error.hpp"
#include "preview/scene.hpp"

#include <chrono>
#include <condition_variable>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <mutex>
#include <optional>
#include <set>
#include <thread>
#include <unordered_map>
#include <vector>

namespace preview {

struct LoadedBlockRef {
    BlockId id = kInvalidBlockId;
    std::uint64_t contentHash = 0;
    const SceneBlock* block = nullptr;
};

struct LoadSnapshot {
    std::uint64_t version = 0;
    std::uint64_t contentHash = 0;
    std::shared_ptr<const Scene> scene;
    std::vector<LoadedBlockRef> blocks;
    std::unordered_map<BlockId, std::size_t> blockIndex;

    const SceneBlock* findBlock(BlockId id) const noexcept;
    std::vector<BlockId> ids() const;
};

struct LoadSnapshotResult {
    std::shared_ptr<const LoadSnapshot> snapshot;
    Diagnostic diagnostic;

    bool ok() const noexcept { return snapshot != nullptr && diagnostic.ok(); }
    explicit operator bool() const noexcept { return ok(); }
};

LoadSnapshotResult makeLoadSnapshot(std::shared_ptr<const Scene> scene,
                                    const std::vector<BlockId>& requestedIds,
                                    std::uint64_t version);

struct StreamingConfig {
    std::size_t batchSize = 16;
};

class StreamingLoader {
public:
    StreamingLoader(std::shared_ptr<const Scene> scene, StreamingConfig config = {});
    ~StreamingLoader();

    StreamingLoader(const StreamingLoader&) = delete;
    StreamingLoader& operator=(const StreamingLoader&) = delete;

    void start();
    void stop();

    bool enqueueBlock(BlockId id);
    std::size_t scheduleForView(const Camera& camera);
    std::size_t enqueueAll();
    void setViewpoint(const Camera& camera);

    std::shared_ptr<const LoadSnapshot> snapshot() const;
    std::uint64_t latestVersion() const;
    std::size_t pendingCount() const;
    std::size_t loadedCount() const;

    bool waitUntilIdle(std::chrono::milliseconds timeout =
                           std::chrono::milliseconds(30000));

private:
    struct QueueEntry {
        std::uint32_t priority = 0;
        double distanceSquared = 0.0;
        BlockId id = kInvalidBlockId;
    };

    struct QueueOrder {
        bool operator()(const QueueEntry& a, const QueueEntry& b) const noexcept;
    };

    void workerMain();
    bool enqueueBlockLocked(BlockId id, const Camera* camera);
    void publishLocked();
    double distanceSquaredToBlock(const Camera& camera, const SceneBlock& block) const;

    std::shared_ptr<const Scene> scene_;
    StreamingConfig config_;
    mutable std::mutex mutex_;
    std::condition_variable condition_;
    std::condition_variable idleCondition_;
    std::thread worker_;
    bool started_ = false;
    bool stopRequested_ = false;
    Camera viewpoint_{};

    std::vector<QueueEntry> queue_;
    std::set<BlockId> queued_;
    std::optional<BlockId> loading_;
    std::unordered_map<BlockId, std::uint64_t> loadedHashes_;
    std::shared_ptr<const LoadSnapshot> current_;
    std::uint64_t nextVersion_ = 1;
};

}  // namespace preview
