#include "preview/streaming.hpp"

#include <algorithm>
#include <cmath>

namespace preview {

const SceneBlock* LoadSnapshot::findBlock(BlockId id) const noexcept {
    const auto found = blockIndex.find(id);
    if (found == blockIndex.end()) {
        return nullptr;
    }
    return blocks[found->second].block;
}

std::vector<BlockId> LoadSnapshot::ids() const {
    std::vector<BlockId> result;
    result.reserve(blocks.size());
    for (const LoadedBlockRef& block : blocks) {
        result.push_back(block.id);
    }
    return result;
}

LoadSnapshotResult makeLoadSnapshot(std::shared_ptr<const Scene> scene,
                                    const std::vector<BlockId>& requestedIds,
                                    std::uint64_t version) {
    LoadSnapshotResult result;
    if (!scene) {
        result.diagnostic = makeDiagnostic(StatusCode::E_INVALID_ARGUMENT,
                                           "snapshot.scene", "scene is null");
        return result;
    }
    std::vector<BlockId> ids = requestedIds;
    std::sort(ids.begin(), ids.end());
    ids.erase(std::unique(ids.begin(), ids.end()), ids.end());
    for (BlockId id : ids) {
        if (scene->findBlock(id) == nullptr) {
            result.diagnostic = makeDiagnostic(StatusCode::E_INVALID_ARGUMENT,
                                               "snapshot.blocks",
                                               "snapshot references unknown block",
                                               id);
            return result;
        }
    }

    auto snapshot = std::make_shared<LoadSnapshot>();
    snapshot->version = version;
    snapshot->scene = std::move(scene);
    snapshot->blocks.reserve(ids.size());
    HashBuilder hash;
    hash.addPod(snapshot->scene->contentHash);
    hash.addPod(static_cast<std::uint64_t>(ids.size()));
    for (BlockId id : ids) {
        const SceneBlock* block = snapshot->scene->findBlock(id);
        const std::uint64_t blockHash = block->contentHash();
        LoadedBlockRef reference;
        reference.id = id;
        reference.contentHash = blockHash;
        reference.block = block;
        snapshot->blockIndex.emplace(id, snapshot->blocks.size());
        snapshot->blocks.push_back(reference);
        hash.addPod(id);
        hash.addPod(blockHash);
    }
    snapshot->contentHash = hash.value();
    result.snapshot = std::move(snapshot);
    result.diagnostic = makeDiagnostic(StatusCode::E_OK, "", "");
    return result;
}

StreamingLoader::StreamingLoader(std::shared_ptr<const Scene> scene,
                                 StreamingConfig config)
    : scene_(std::move(scene)), config_(config) {
    if (config_.batchSize == 0) {
        config_.batchSize = 1;
    }
    if (!scene_) {
        return;
    }
    const auto initial = makeLoadSnapshot(scene_, {}, 0);
    current_ = initial.snapshot;
}

StreamingLoader::~StreamingLoader() {
    stop();
}

void StreamingLoader::start() {
    std::lock_guard<std::mutex> lock(mutex_);
    if (started_ || stopRequested_ || !scene_) {
        return;
    }
    started_ = true;
    worker_ = std::thread(&StreamingLoader::workerMain, this);
}

void StreamingLoader::stop() {
    {
        std::lock_guard<std::mutex> lock(mutex_);
        if (!started_) {
            stopRequested_ = true;
            return;
        }
        stopRequested_ = true;
    }
    condition_.notify_all();
    idleCondition_.notify_all();
    if (worker_.joinable()) {
        worker_.join();
    }
    std::lock_guard<std::mutex> lock(mutex_);
    started_ = false;
}

bool StreamingLoader::QueueOrder::operator()(const QueueEntry& a,
                                             const QueueEntry& b) const noexcept {
    if (a.priority != b.priority) {
        return a.priority > b.priority;
    }
    if (a.distanceSquared != b.distanceSquared) {
        return a.distanceSquared < b.distanceSquared;
    }
    return a.id < b.id;
}

double StreamingLoader::distanceSquaredToBlock(const Camera& camera,
                                               const SceneBlock& block) const {
    const Vec3 delta = block.bounds.center() - camera.position;
    return lengthSquared(delta);
}

bool StreamingLoader::enqueueBlockLocked(BlockId id, const Camera* camera) {
    const SceneBlock* block = scene_->findBlock(id);
    if (block == nullptr) {
        return false;
    }
    if (loadedHashes_.find(id) != loadedHashes_.end() || loading_ == id ||
        queued_.find(id) != queued_.end()) {
        return false;
    }
    QueueEntry entry;
    entry.priority = block->priority;
    entry.distanceSquared =
        camera != nullptr ? distanceSquaredToBlock(*camera, *block) : 0.0;
    entry.id = id;
    queue_.push_back(entry);
    queued_.insert(id);
    return true;
}

bool StreamingLoader::enqueueBlock(BlockId id) {
    std::lock_guard<std::mutex> lock(mutex_);
    const bool inserted = enqueueBlockLocked(id, &viewpoint_);
    if (inserted) {
        std::sort(queue_.begin(), queue_.end(), QueueOrder{});
        condition_.notify_one();
    }
    return inserted;
}

std::size_t StreamingLoader::scheduleForView(const Camera& camera) {
    std::lock_guard<std::mutex> lock(mutex_);
    viewpoint_ = camera;
    queue_.clear();
    queued_.clear();
    std::size_t inserted = 0;
    if (!scene_) {
        return 0;
    }
    queue_.reserve(scene_->blocks.size());
    for (const SceneBlock& block : scene_->blocks) {
        if (enqueueBlockLocked(block.id, &viewpoint_)) {
            ++inserted;
        }
    }
    std::sort(queue_.begin(), queue_.end(), QueueOrder{});
    if (inserted != 0) {
        condition_.notify_one();
    }
    return inserted;
}

std::size_t StreamingLoader::enqueueAll() {
    std::lock_guard<std::mutex> lock(mutex_);
    if (!scene_) {
        return 0;
    }
    std::size_t inserted = 0;
    queue_.reserve(queue_.size() + scene_->blocks.size());
    for (const SceneBlock& block : scene_->blocks) {
        if (enqueueBlockLocked(block.id, &viewpoint_)) {
            ++inserted;
        }
    }
    std::sort(queue_.begin(), queue_.end(), QueueOrder{});
    if (inserted != 0) {
        condition_.notify_one();
    }
    return inserted;
}

void StreamingLoader::setViewpoint(const Camera& camera) {
    std::lock_guard<std::mutex> lock(mutex_);
    viewpoint_ = camera;
}

std::shared_ptr<const LoadSnapshot> StreamingLoader::snapshot() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return current_;
}

std::uint64_t StreamingLoader::latestVersion() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return current_ ? current_->version : 0;
}

std::size_t StreamingLoader::pendingCount() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return queue_.size() + (loading_.has_value() ? 1u : 0u);
}

std::size_t StreamingLoader::loadedCount() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return loadedHashes_.size();
}

void StreamingLoader::publishLocked() {
    std::vector<BlockId> ids;
    ids.reserve(loadedHashes_.size());
    for (const auto& entry : loadedHashes_) {
        ids.push_back(entry.first);
    }
    LoadSnapshotResult next = makeLoadSnapshot(scene_, ids, nextVersion_);
    if (!next) {
        return;
    }
    ++nextVersion_;
    current_ = std::move(next.snapshot);
    idleCondition_.notify_all();
}

void StreamingLoader::workerMain() {
    std::vector<BlockId> completedBatch;
    for (;;) {
        QueueEntry selected;
        {
            std::unique_lock<std::mutex> lock(mutex_);
            condition_.wait(lock, [&]() {
                return stopRequested_ || !queue_.empty();
            });
            if (stopRequested_) {
                break;
            }
            const auto best = std::min_element(queue_.begin(), queue_.end(),
                                               [](const QueueEntry& a,
                                                  const QueueEntry& b) {
                                                   return QueueOrder{}(a, b);
                                               });
            selected = *best;
            queue_.erase(best);
            queued_.erase(selected.id);
            loading_ = selected.id;
        }

        const SceneBlock* block = scene_->findBlock(selected.id);
        if (block != nullptr) {
            std::lock_guard<std::mutex> lock(mutex_);
            if (stopRequested_) {
                loading_.reset();
                break;
            }
            loadedHashes_.emplace(selected.id, block->contentHash());
            loading_.reset();
            completedBatch.push_back(selected.id);
            if (completedBatch.size() >= config_.batchSize || queue_.empty()) {
                publishLocked();
                completedBatch.clear();
            }
            idleCondition_.notify_all();
        } else {
            std::lock_guard<std::mutex> lock(mutex_);
            loading_.reset();
            idleCondition_.notify_all();
        }
    }
}

bool StreamingLoader::waitUntilIdle(std::chrono::milliseconds timeout) {
    std::unique_lock<std::mutex> lock(mutex_);
    return idleCondition_.wait_for(lock, timeout, [&]() {
        return stopRequested_ || (queue_.empty() && !loading_.has_value());
    });
}

}  // namespace preview
