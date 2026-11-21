#include "chunkpipe/pipeline.hpp"

#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <map>
#include <set>
#include <thread>
#include <unordered_set>

#include "chunkpipe/hash.hpp"

namespace chunkpipe {

namespace {

std::mutex gActiveScenesMutex;
std::set<std::uint32_t> gActiveScenes;

bool acquireSceneTask(std::uint32_t sceneId) {
    std::lock_guard<std::mutex> lock(gActiveScenesMutex);
    if (gActiveScenes.find(sceneId) != gActiveScenes.end()) {
        return false;
    }
    gActiveScenes.insert(sceneId);
    return true;
}

void releaseSceneTask(std::uint32_t sceneId) {
    std::lock_guard<std::mutex> lock(gActiveScenesMutex);
    gActiveScenes.erase(sceneId);
}

struct LoadOutcome {
    std::uint32_t chunkId = 0;
    bool success = false;
    Chunk chunk;
    std::uint64_t contentHash = 0;
    BusinessError error;
};

BusinessError makeError(Code code,
                        const std::string& message,
                        std::uint32_t chunkId = kNoChunk,
                        const std::string& field = {}) {
    BusinessError error;
    error.code = code;
    error.message = message;
    if (chunkId != kNoChunk || !field.empty()) {
        ErrorLocation location;
        location.chunkId = chunkId;
        location.field = field;
        error.locations.push_back(std::move(location));
    }
    return error;
}

std::string idsToCsv(const std::vector<std::uint32_t>& ids) {
    std::string value;
    for (std::size_t index = 0; index < ids.size(); ++index) {
        if (index != 0) {
            value += ",";
        }
        value += std::to_string(ids[index]);
    }
    return value;
}

} // namespace

Pipeline::Pipeline(SceneArchive scene,
                   StreamingOptions options,
                   TraceLogger* logger)
    : scene_(std::move(scene)), options_(std::move(options)), logger_(logger) {}

void Pipeline::seedLoadedChunks(std::vector<Chunk> chunks) {
    for (Chunk& chunk : chunks) {
        loaded_[chunk.id] = std::move(chunk);
    }
    rebuildLoadedIndex();
}

void Pipeline::attachRecovery(RecoveryStore* store) {
    recovery_ = store;
}

void Pipeline::setCancelAfterLoads(std::uint32_t count) {
    options_.cancelAfterLoads = count;
}

void Pipeline::requestCancellation() {
    cancelRequested_.store(true, std::memory_order_release);
}

RunResult Pipeline::tryRun(const Camera& camera) {
    bool expected = false;
    if (!busy_.compare_exchange_strong(expected, true,
                                       std::memory_order_acq_rel,
                                       std::memory_order_acquire)) {
        RunResult result;
        result.code = Code::Busy;
        result.error = makeError(Code::Busy,
                                 "a streaming cull task is already active");
        result.snapshot = latest();
        if (logger_) {
            logger_->event("task_busy");
        }
        return result;
    }
    struct BusyReset {
        std::atomic<bool>& flag;
        ~BusyReset() { flag.store(false, std::memory_order_release); }
    } reset{busy_};
    if (!acquireSceneTask(scene_.sceneId)) {
        RunResult result;
        result.code = Code::Busy;
        result.error = makeError(
            Code::Busy,
            "another pipeline is already streaming and culling this scene");
        result.snapshot = latest();
        if (logger_) {
            logger_->event("task_busy",
                           {{"scene_id", std::to_string(scene_.sceneId)}});
        }
        return result;
    }
    struct SceneTaskReset {
        std::uint32_t sceneId;
        ~SceneTaskReset() { releaseSceneTask(sceneId); }
    } sceneReset{scene_.sceneId};
    cancelRequested_.store(false, std::memory_order_release);

    try {
        return runImpl(camera);
    } catch (const std::exception& exception) {
        RunResult result;
        result.code = Code::Internal;
        result.error =
            makeError(Code::Internal, std::string("pipeline exception: ") +
                                           exception.what());
        result.snapshot = latest();
        if (logger_) {
            logger_->event("pipeline_exception", {{"detail", exception.what()}});
        }
        return result;
    }
}

RunResult Pipeline::runImpl(const Camera& camera) {
    RunResult result;
    result.snapshot = latest();

    std::vector<const Chunk*> pending;
    pending.reserve(scene_.chunks.size());
    for (const Chunk& chunk : scene_.chunks) {
        if (loaded_.find(chunk.id) == loaded_.end()) {
            pending.push_back(&chunk);
        }
    }
    std::sort(pending.begin(), pending.end(), [&](const Chunk* a, const Chunk* b) {
        if (a->priority != b->priority) {
            return a->priority > b->priority;
        }
        const float distanceA = length(a->bounds.center() - camera.position);
        const float distanceB = length(b->bounds.center() - camera.position);
        if (distanceA != distanceB) {
            return distanceA < distanceB;
        }
        return a->id < b->id;
    });
    if (options_.maxChunksPerRun > 0 && pending.size() > options_.maxChunksPerRun) {
        pending.resize(options_.maxChunksPerRun);
    }
    for (const Chunk* chunk : pending) {
        result.scheduled.push_back(chunk->id);
    }
    if (logger_) {
        logger_->event("stream_schedule",
                       {{"count", std::to_string(result.scheduled.size())},
                        {"worker_count", std::to_string(options_.workerCount)},
                        {"order", idsToCsv(result.scheduled)}});
    }

    std::unordered_set<std::uint32_t> failSet(options_.failChunks.begin(),
                                              options_.failChunks.end());
    std::vector<LoadOutcome> outcomes;
    outcomes.reserve(pending.size());
    if (!pending.empty()) {
        const std::uint32_t alreadyLoaded =
            static_cast<std::uint32_t>(loaded_.size());
        const std::uint32_t sceneTotal =
            static_cast<std::uint32_t>(scene_.chunks.size());
        std::atomic<std::size_t> nextIndex{0};
        std::atomic<std::size_t> completed{0};
        std::atomic<std::size_t> successful{0};
        std::atomic<bool> cancelled{false};
        std::mutex outcomesMutex;
        const std::size_t threadCount =
            std::min<std::size_t>(std::max<unsigned>(1, options_.workerCount),
                                  pending.size());
        std::vector<std::thread> workers;
        workers.reserve(threadCount);
        for (std::size_t threadIndex = 0; threadIndex < threadCount; ++threadIndex) {
            workers.emplace_back([&]() {
                while (!cancelled.load(std::memory_order_acquire) &&
                       !cancelRequested_.load(std::memory_order_acquire)) {
                    const std::size_t index =
                        nextIndex.fetch_add(1, std::memory_order_relaxed);
                    if (index >= pending.size()) {
                        break;
                    }
                    const Chunk& source = *pending[index];
                    if (options_.loadDelayMicros > 0) {
                        std::this_thread::sleep_for(
                            std::chrono::microseconds(options_.loadDelayMicros));
                    }
                    LoadOutcome outcome;
                    outcome.chunkId = source.id;
                    if (failSet.find(source.id) != failSet.end()) {
                        outcome.success = false;
                        outcome.error =
                            makeError(Code::LoadFailed,
                                      "deterministic injected chunk load failure",
                                      source.id, "load");
                    } else {
                        outcome.success = true;
                        outcome.chunk = source;
                        outcome.contentHash = chunkContentHash(source);
                    }
                    {
                        std::lock_guard<std::mutex> lock(outcomesMutex);
                        outcomes.push_back(std::move(outcome));
                    }
                    const std::size_t done =
                        completed.fetch_add(1, std::memory_order_acq_rel) + 1;
                    if (outcome.success && options_.onLoadProgress) {
                        const std::size_t successCount =
                            successful.fetch_add(1, std::memory_order_acq_rel) + 1;
                        options_.onLoadProgress(
                            alreadyLoaded +
                                static_cast<std::uint32_t>(successCount),
                            sceneTotal, outcome.chunkId);
                    }
                    if (options_.cancelAfterLoads > 0 &&
                        done >= options_.cancelAfterLoads) {
                        cancelled.store(true, std::memory_order_release);
                    }
                }
            });
        }
        for (std::thread& worker : workers) {
            worker.join();
        }
        result.cancelled = cancelled.load(std::memory_order_acquire) ||
                           cancelRequested_.load(std::memory_order_acquire);
    }

    std::sort(outcomes.begin(), outcomes.end(),
              [](const LoadOutcome& a, const LoadOutcome& b) {
                  return a.chunkId < b.chunkId;
              });
    std::vector<LoadOutcome> successes;
    for (const LoadOutcome& outcome : outcomes) {
        if (!outcome.success) {
            result.loadFailures.push_back(outcome.error);
            if (logger_) {
                const ErrorLocation* location =
                    outcome.error.locations.empty() ? nullptr
                                                    : &outcome.error.locations.front();
                logger_->event(
                    "load_failed",
                    {{"chunk", std::to_string(outcome.chunkId)},
                     {"code", codeName(outcome.error.code)},
                     {"field", location ? location->field : "load"}});
            }
        } else {
            successes.push_back(outcome);
        }
    }

    if (result.cancelled) {
        result.code = Code::Cancelled;
        result.error = makeError(
            Code::Cancelled,
            "streaming task cancelled at a cooperative checkpoint; "
            "the last published snapshot remains active");
        result.snapshot = latest();
        if (logger_) {
            logger_->event("load_cancelled",
                           {{"completed", std::to_string(outcomes.size())},
                            {"published_version",
                             result.snapshot
                                 ? std::to_string(result.snapshot->publishVersion)
                                 : "0"}});
        }
        return result;
    }

    for (const LoadOutcome& outcome : successes) {
        result.newlyLoaded.push_back(outcome.chunkId);
        if (logger_) {
            logger_->event("load_completed",
                           {{"chunk", std::to_string(outcome.chunkId)},
                            {"hash", hashHex(outcome.contentHash)}});
        }
    }

    std::uint32_t commitsThisRun = 0;
    if (recovery_) {
        for (const LoadOutcome& outcome : successes) {
            Result<CommitResult> commit = recovery_->commitChunk(
                scene_, outcome.chunk, outcome.contentHash, 1);
            if (!commit.ok()) {
                result.code = commit.error().code;
                result.error = commit.error();
                result.snapshot = latest();
                if (logger_) {
                    logger_->event("recovery_commit_failed",
                                   {{"chunk", std::to_string(outcome.chunkId)},
                                    {"code", codeName(result.code)}});
                }
                return result;
            }
            if (commit.value().wrote) {
                ++commitsThisRun;
                if (logger_) {
                    logger_->event(
                        "recovery_commit",
                        {{"chunk", std::to_string(outcome.chunkId)},
                         {"sequence", std::to_string(commit.value().sequence)},
                         {"chain", hashHex(commit.value().chainHash)}});
                }
            }
            if (options_.crashAfterCommits > 0 &&
                commitsThisRun >= options_.crashAfterCommits) {
                if (logger_) {
                    logger_->event(
                        "forced_termination",
                        {{"chunk", std::to_string(outcome.chunkId)},
                         {"commits", std::to_string(commitsThisRun)}});
                    logger_->flush();
                }
                std::_Exit(70);
            }
        }
    }

    std::map<std::uint32_t, Chunk> candidateLoaded = loaded_;
    for (const LoadOutcome& outcome : successes) {
        candidateLoaded[outcome.chunkId] = outcome.chunk;
    }
    std::vector<Chunk> cullInput;
    cullInput.reserve(candidateLoaded.size());
    for (auto& entry : candidateLoaded) {
        cullInput.push_back(entry.second);
    }

    if (logger_) {
        logger_->event("cull_begin",
                       {{"loaded", std::to_string(cullInput.size())},
                        {"archive_version", std::to_string(scene_.version)}});
    }
    Result<FrameSnapshot> rendered =
        renderVisibility(scene_, cullInput, camera, scene_.boundaryTolerance,
                         &cancelRequested_);
    if (!rendered.ok()) {
        result.code = rendered.error().code;
        result.error = rendered.error();
        result.snapshot = latest();
        result.cancelled = rendered.error().code == Code::Cancelled;
        if (logger_) {
            const ErrorLocation* location =
                result.error.locations.empty() ? nullptr
                                               : &result.error.locations.front();
            logger_->event(
                "cull_failed",
                {{"code", codeName(result.code)},
                 {"chunk",
                  location ? std::to_string(location->chunkId) : std::string("none")},
                 {"field", location ? location->field : "visibility"}});
        }
        return result;
    }

    loaded_ = std::move(candidateLoaded);
    rebuildLoadedIndex();
    auto snapshot =
        std::make_shared<FrameSnapshot>(std::move(rendered.value()));
    snapshot->publishVersion = nextPublishVersion_++;
    snapshot->contentHash = snapshotContentHash(*snapshot);
    publish(snapshot);
    result.snapshot = snapshot;
    result.code = result.loadFailures.empty() ? Code::Ok : Code::LoadFailed;
    if (result.code == Code::LoadFailed) {
        result.error = makeError(
            Code::LoadFailed,
            "one or more chunks failed to load; failed chunks are absent "
            "from the published visibility mask",
            result.loadFailures.front().locations.empty()
                ? kNoChunk
                : result.loadFailures.front().locations.front().chunkId,
            "load");
    }
    if (logger_) {
        logger_->event(
            "publish",
            {{"version", std::to_string(snapshot->publishVersion)},
             {"content_hash", hashHex(snapshot->contentHash)},
             {"loaded", std::to_string(snapshot->loadedChunkIds.size())},
             {"mask", maskHex(*snapshot)},
             {"frustum_culled", std::to_string(snapshot->stats.frustumCulled)},
             {"occlusion_culled", std::to_string(snapshot->stats.occlusionCulled)},
             {"visible", std::to_string(snapshot->stats.visibleChunks)},
             {"critical", std::to_string(snapshot->stats.criticalChunks)}});
    }
    return result;
}

void Pipeline::publish(std::shared_ptr<FrameSnapshot> snapshot) {
    std::lock_guard<std::mutex> lock(publishedMutex_);
    published_ = std::move(snapshot);
}

std::shared_ptr<const FrameSnapshot> Pipeline::latest() const {
    std::lock_guard<std::mutex> lock(publishedMutex_);
    return published_;
}

Result<std::shared_ptr<const FrameSnapshot>> Pipeline::queryPublished(
    std::uint32_t expectedVersion,
    std::uint64_t expectedHash) const {
    std::shared_ptr<const FrameSnapshot> snapshot = latest();
    if (!snapshot || snapshot->publishVersion != expectedVersion ||
        snapshot->contentHash != expectedHash) {
        BusinessError error;
        error.code = Code::VersionStale;
        error.message = "requested published version or content hash is stale";
        ErrorLocation location;
        location.field = "published.version";
        if (snapshot) {
            location.detail = "expected version=" + std::to_string(expectedVersion) +
                              " hash=" + hashHex(expectedHash) +
                              "; current version=" +
                              std::to_string(snapshot->publishVersion) +
                              " hash=" + hashHex(snapshot->contentHash);
        } else {
            location.detail = "no snapshot has been published";
        }
        error.locations.push_back(std::move(location));
        return Result<std::shared_ptr<const FrameSnapshot>>::failure(
            std::move(error));
    }
    return Result<std::shared_ptr<const FrameSnapshot>>::success(snapshot);
}

void Pipeline::rebuildLoadedIndex() {
    loadedIds_.clear();
    loadedIds_.reserve(loaded_.size());
    for (const auto& entry : loaded_) {
        loadedIds_.push_back(entry.first);
    }
}

} // namespace chunkpipe

