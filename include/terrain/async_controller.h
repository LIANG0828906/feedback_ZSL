#pragma once

#include <atomic>
#include <condition_variable>
#include <functional>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

#include "terrain/scene.h"

namespace terrain {

struct JobResult {
    Status status;
    SceneUpdateReport report;
    bool completed = false;
};

struct RecoveryReport {
    Status status;
    std::uint64_t started_at_version = 0;
    std::uint64_t finished_at_version = 0;
    std::uint32_t committed_batches = 0;
    std::uint32_t skipped_committed_patches = 0;
    std::vector<std::string> notes;
};

Status groupPatchDirectory(const std::string& directory,
                           std::vector<PatchBatch>* batches);

class AsyncTerrainController {
public:
    AsyncTerrainController() = default;
    ~AsyncTerrainController();

    AsyncTerrainController(const AsyncTerrainController&) = delete;
    AsyncTerrainController& operator=(const AsyncTerrainController&) = delete;

    Status initialize(HeightField height, MaterialImage material,
                      const SceneOptions& options);
    Status initializeFromFiles(const std::string& height_path,
                               const std::string& material_path,
                               const SceneOptions& options);
    Status initializeFromStore(VersionStore* store,
                               const SceneOptions& options);

    Status submit(const PatchBatch& batch, VersionStore* store,
                  CommitFault fault = CommitFault::None);
    Status submitOptions(const SceneOptions& options);
    Status cancel();
    bool busy() const { return busy_.load(); }
    bool waitForCompletion(std::uint64_t timeout_ms, JobResult* result);
    JobResult result() const;
    Status recoverPending(VersionStore* store,
                          const std::string& patch_directory,
                          RecoveryReport* recovery);

    TerrainScene& scene() { return scene_; }
    const TerrainScene& scene() const { return scene_; }

private:
    Status startJob(std::function<void(JobResult*)> work);
    void joinWorker();

    TerrainScene scene_;
    std::atomic<bool> busy_{false};
    std::atomic<bool> cancel_requested_{false};
    std::atomic<bool> shutting_down_{false};
    mutable std::mutex mutex_;
    std::condition_variable completed_cv_;
    std::thread worker_;
    JobResult job_result_;
};

}  // namespace terrain

