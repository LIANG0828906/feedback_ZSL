#pragma once

#include "preview/occlusion.hpp"
#include "preview/visibility.hpp"

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <functional>
#include <future>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <vector>

namespace preview {

struct RenderConfig {
    DepthPyramidConfig pyramid;
    OcclusionConfig occlusion;
    std::size_t cancellationCheckpointBlocks = 16;
    std::string snapshotPath;
    std::shared_ptr<std::atomic<bool>> externalCancel;
    std::function<void(const LoadSnapshot&)> onSnapshotBound;
};

struct RenderRequest {
    Camera camera;
    std::optional<std::uint64_t> expectedSnapshotVersion;
};

struct RenderResult {
    StatusCode code = StatusCode::E_INTERNAL;
    Diagnostic diagnostic;
    std::uint64_t jobId = 0;
    std::uint64_t snapshotVersion = 0;
    std::uint64_t pyramidVersion = 0;
    VisibilityMask mask;
    FrameErrorSummary summary;
    std::vector<BlockVisibility> blocks;
    PyramidRebuildReport pyramidRebuild;
    bool newerSnapshotAvailable = false;

    bool ok() const noexcept { return code == StatusCode::E_OK; }
    explicit operator bool() const noexcept { return ok(); }
};

struct AsyncRenderSubmission {
    StatusCode code = StatusCode::E_INTERNAL;
    Diagnostic diagnostic;
    std::uint64_t jobId = 0;
    std::future<RenderResult> future;

    bool ok() const noexcept { return code == StatusCode::E_OK; }
    explicit operator bool() const noexcept { return ok(); }
};

class Renderer {
public:
    Renderer(std::shared_ptr<StreamingLoader> loader, RenderConfig config = {});
    // Non-owning convenience overload; the loader must outlive the renderer.
    Renderer(StreamingLoader& loader, RenderConfig config = {});
    ~Renderer();

    Renderer(const Renderer&) = delete;
    Renderer& operator=(const Renderer&) = delete;

    RenderResult render(const RenderRequest& request);
    AsyncRenderSubmission submitRender(const RenderRequest& request);

    void cancelCurrentRender();
    bool busy() const noexcept {
        return busyFlag_ != nullptr && busyFlag_->load();
    }
    std::uint64_t lastCommittedSnapshotVersion() const;
    std::shared_ptr<const StreamingLoader> loader() const { return loader_; }

private:
    struct BusyGuard {
        explicit BusyGuard(Renderer* owner) : renderer(owner) {}
        ~BusyGuard() { renderer->releaseBusy(); }
        BusyGuard(const BusyGuard&) = delete;
        BusyGuard& operator=(const BusyGuard&) = delete;
        Renderer* renderer;
    };

    RenderResult executeRender(const RenderRequest& request,
                               std::uint64_t jobId,
                               const std::shared_ptr<std::atomic<bool>>& jobCancel);
    void releaseBusy();
    void markJobActive();
    bool cancellationRequested(
        const std::shared_ptr<std::atomic<bool>>& jobCancel) const noexcept;
    void waitUntilNotBusy();

    std::shared_ptr<StreamingLoader> loader_;
    RenderConfig config_;
    std::shared_ptr<std::atomic<bool>> busyFlag_;
    std::atomic<bool> hasActiveJob_{false};
    std::atomic<std::uint64_t> nextJobId_{1};
    mutable std::mutex stateMutex_;
    std::condition_variable busyCondition_;
    std::shared_ptr<std::atomic<bool>> currentJobCancel_;
    std::shared_ptr<const DepthPyramid> previousPyramid_;
    std::uint64_t nextPyramidVersion_ = 1;
    std::uint64_t lastCommittedSnapshotVersion_ = 0;
};

struct SnapshotRecoveryCheck {
    StatusCode code = StatusCode::E_OK;
    Diagnostic diagnostic;
    bool requiresRerender = false;
    std::vector<std::string> changes;
    std::uint64_t recoveredSnapshotVersion = 0;
    std::uint64_t recoveredMaskHash = 0;

    bool ok() const noexcept { return code == StatusCode::E_OK; }
    explicit operator bool() const noexcept { return ok(); }
};

SnapshotRecoveryCheck checkSnapshotRecovery(
    const std::string& path,
    const LoadSnapshot& currentLoadSnapshot,
    const Camera& currentCamera);

}  // namespace preview
