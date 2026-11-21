#include "terrain/async_controller.h"

#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <filesystem>
#include <map>
#include <sstream>

#include "terrain/binary_io.h"
#include "terrain/terrain_io.h"

namespace terrain {
namespace {

std::uint64_t jobDelayMilliseconds() {
    const char* text = std::getenv("TERRAIN_TEST_JOB_DELAY_MS");
    if (text == nullptr) return 0;
    try {
        return static_cast<std::uint64_t>(std::stoull(text));
    } catch (...) {
        return 0;
    }
}

bool parseSequencePrefix(const std::string& filename, std::uint64_t* sequence) {
    const std::size_t underscore = filename.find('_');
    if (underscore == std::string::npos || underscore == 0) return false;
    const std::string prefix = filename.substr(0, underscore);
    if (!std::all_of(prefix.begin(), prefix.end(),
                     [](char c) { return c >= '0' && c <= '9'; })) {
        return false;
    }
    try {
        *sequence = std::stoull(prefix);
        return true;
    } catch (...) {
        return false;
    }
}

}  // namespace

Status groupPatchDirectory(const std::string& directory,
                           std::vector<PatchBatch>* batches) {
    struct Entry {
        std::string height;
        std::string material;
    };
    std::map<std::uint64_t, Entry> grouped;
    for (const std::string& path : listFiles(directory)) {
        const std::string filename =
            std::filesystem::path(path).filename().string();
        std::uint64_t sequence = 0;
        if (!parseSequencePrefix(filename, &sequence)) continue;
        if (filename.find("height") != std::string::npos &&
            filename.size() > 4 &&
            filename.substr(filename.size() - 4) == ".bin") {
            grouped[sequence].height = path;
        } else if (filename.find("material") != std::string::npos &&
                   filename.size() > 4 &&
                   filename.substr(filename.size() - 4) == ".bin") {
            grouped[sequence].material = path;
        }
    }
    std::vector<PatchBatch> result;
    for (const auto& entry : grouped) {
        PatchBatch batch;
        batch.sequence = entry.first;
        if (!entry.second.height.empty()) {
            HeightPatch patch;
            Status status = loadHeightPatch(entry.second.height, &patch);
            if (!status.ok()) return status;
            batch.height = std::move(patch);
            batch.source_path = entry.second.height;
        }
        if (!entry.second.material.empty()) {
            MaterialPatch patch;
            Status status = loadMaterialPatch(entry.second.material, &patch);
            if (!status.ok()) return status;
            batch.material = std::move(patch);
            if (batch.source_path.empty()) {
                batch.source_path = entry.second.material;
            }
        }
        if (!batch.height && !batch.material) continue;
        batch.patch_hash = computeBatchHash(batch);
        result.push_back(std::move(batch));
    }
    std::sort(result.begin(), result.end(),
              [](const PatchBatch& a, const PatchBatch& b) {
                  return a.sequence < b.sequence;
              });
    if (batches != nullptr) *batches = std::move(result);
    return Status::success();
}

AsyncTerrainController::~AsyncTerrainController() {
    shutting_down_.store(true);
    cancel_requested_.store(true);
    joinWorker();
}

void AsyncTerrainController::joinWorker() {
    if (worker_.joinable()) {
        worker_.join();
    }
}

Status AsyncTerrainController::initialize(HeightField height,
                                          MaterialImage material,
                                          const SceneOptions& options) {
    if (busy_.load()) {
        return Status::error(E_BUSY, "job", "recompute task is already active");
    }
    return scene_.initialize(std::move(height), std::move(material), options);
}

Status AsyncTerrainController::initializeFromFiles(
    const std::string& height_path, const std::string& material_path,
    const SceneOptions& options) {
    if (busy_.load()) {
        return Status::error(E_BUSY, "job", "recompute task is already active");
    }
    return scene_.initializeFromFiles(height_path, material_path, options);
}

Status AsyncTerrainController::initializeFromStore(VersionStore* store,
                                                   const SceneOptions& options) {
    if (busy_.load()) {
        return Status::error(E_BUSY, "job", "recompute task is already active");
    }
    return scene_.loadCommitted(store, options);
}

Status AsyncTerrainController::submit(const PatchBatch& batch,
                                      VersionStore* store,
                                      CommitFault fault) {
    return startJob([this, batch, store, fault](JobResult* result) {
        result->status = scene_.applyBatch(
            batch, store, fault,
            [this]() { return cancel_requested_.load(); }, &result->report);
    });
}

Status AsyncTerrainController::submitOptions(const SceneOptions& options) {
    return startJob([this, options](JobResult* result) {
        result->status = Status::success();
        scene_.rebuildOptions(
            options, &result->status,
            [this]() { return cancel_requested_.load(); });
        result->report.patch_sequence = scene_.version();
        result->report.committed_version = scene_.version();
        result->report.notes.push_back(
            "background rebuild with block_size=" +
            std::to_string(options.quadtree.block_size) +
            " error_threshold=" +
            std::to_string(options.quadtree.error_threshold));
    });
}

Status AsyncTerrainController::startJob(
    std::function<void(JobResult*)> work) {
    std::lock_guard<std::mutex> lock(mutex_);
    if (busy_.load()) {
        return Status::error(E_BUSY, "job",
                             "the terrain already has one recompute task");
    }
    if (worker_.joinable()) {
        worker_.join();
    }
    cancel_requested_.store(false);
    shutting_down_.store(false);
    busy_.store(true);
    job_result_ = JobResult{};
    try {
        worker_ = std::thread([this, work = std::move(work)]() {
            const std::uint64_t delay = jobDelayMilliseconds();
            if (delay > 0) {
                for (std::uint64_t elapsed = 0;
                     elapsed < delay && !cancel_requested_.load();
                     elapsed += 10) {
                    std::this_thread::sleep_for(
                        std::chrono::milliseconds(
                            std::min<std::uint64_t>(10, delay - elapsed)));
                }
            }
            JobResult local;
            try {
                work(&local);
            } catch (const std::exception& exception) {
                local.status = Status::error(
                    E_INVALID_ARGUMENT, "worker",
                    std::string("recompute exception: ") + exception.what());
            } catch (...) {
                local.status = Status::error(
                    E_INVALID_ARGUMENT, "worker",
                    "unknown recompute exception");
            }
            local.completed = true;
            {
                std::lock_guard<std::mutex> guard(mutex_);
                job_result_ = std::move(local);
                busy_.store(false);
            }
            completed_cv_.notify_all();
        });
    } catch (...) {
        busy_.store(false);
        return Status::error(E_INVALID_ARGUMENT, "worker",
                             "cannot create recompute thread");
    }
    return Status::success();
}

Status AsyncTerrainController::cancel() {
    if (!busy_.load()) {
        return Status::error(E_INVALID_ARGUMENT, "job",
                             "no active recompute task");
    }
    cancel_requested_.store(true);
    return Status::success();
}

bool AsyncTerrainController::waitForCompletion(std::uint64_t timeout_ms,
                                               JobResult* result) {
    std::unique_lock<std::mutex> lock(mutex_);
    const bool ready = completed_cv_.wait_for(
        lock, std::chrono::milliseconds(timeout_ms),
        [this]() { return !busy_.load(); });
    if (ready && result != nullptr) {
        *result = job_result_;
    }
    return ready;
}

JobResult AsyncTerrainController::result() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return job_result_;
}

Status AsyncTerrainController::recoverPending(
    VersionStore* store, const std::string& patch_directory,
    RecoveryReport* recovery) {
    RecoveryReport local;
    if (store == nullptr) {
        local.status = Status::error(E_INVALID_ARGUMENT, "store",
                                     "version store is required");
        if (recovery != nullptr) *recovery = local;
        return local.status;
    }
    if (busy_.load()) {
        local.status = Status::error(E_BUSY, "job",
                                     "recompute task is already active");
        if (recovery != nullptr) *recovery = local;
        return local.status;
    }
    Status status = scene_.loadCommitted(store, scene_.options());
    if (!status.ok()) {
        local.status = status;
        if (recovery != nullptr) *recovery = local;
        return status;
    }
    local.started_at_version = scene_.version();
    std::vector<PatchBatch> batches;
    status = groupPatchDirectory(patch_directory, &batches);
    if (!status.ok()) {
        local.status = status;
        if (recovery != nullptr) *recovery = local;
        return status;
    }
    std::uint64_t expected = scene_.version() + 1u;
    for (const PatchBatch& batch : batches) {
        if (batch.sequence < expected) {
            ++local.skipped_committed_patches;
            continue;
        }
        if (batch.sequence > expected) {
            local.status = Status::error(
                E_RECOVERY_FAILED, "patch_sequence",
                "pending patch sequence gap; expected " +
                    std::to_string(expected) + ", found " +
                    std::to_string(batch.sequence));
            break;
        }
        SceneUpdateReport report;
        status = scene_.applyBatch(
            batch, store, CommitFault::None, []() { return false; }, &report);
        if (!status.ok()) {
            local.status = status;
            local.notes.push_back(report.stableString());
            break;
        }
        ++local.committed_batches;
        ++expected;
        local.notes.push_back("replayed patch " +
                              std::to_string(batch.sequence) + ": " +
                              report.stableString());
    }
    local.finished_at_version = scene_.version();
    if (local.status.ok()) {
        local.status = Status::success();
    }
    if (recovery != nullptr) *recovery = std::move(local);
    return local.status;
}

}  // namespace terrain

