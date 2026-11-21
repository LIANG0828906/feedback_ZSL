#include "preview/renderer.hpp"

#include "preview/culling.hpp"
#include "preview/snapshot.hpp"

#include <algorithm>
#include <cmath>
#include <exception>
#include <limits>
#include <mutex>
#include <unordered_map>
#include <utility>

namespace preview {

namespace {

std::mutex gSceneBusyMutex;
std::unordered_map<std::uint64_t, std::weak_ptr<std::atomic<bool>>> gSceneBusy;

std::shared_ptr<std::atomic<bool>> sceneBusyFor(
    const std::shared_ptr<StreamingLoader>& loader) {
    std::uint64_t key = 0;
    if (loader) {
        const auto snapshot = loader->snapshot();
        if (snapshot && snapshot->scene) {
            key = snapshot->scene->contentHash;
        }
    }
    if (key == 0) {
        return std::make_shared<std::atomic<bool>>(false);
    }
    std::lock_guard<std::mutex> lock(gSceneBusyMutex);
    auto& entry = gSceneBusy[key];
    auto shared = entry.lock();
    if (!shared) {
        shared = std::make_shared<std::atomic<bool>>(false);
        entry = shared;
    }
    return shared;
}

}  // namespace

Renderer::Renderer(std::shared_ptr<StreamingLoader> loader, RenderConfig config)
    : loader_(std::move(loader)),
      config_(std::move(config)),
      busyFlag_(sceneBusyFor(loader_)) {
    if (config_.cancellationCheckpointBlocks == 0) {
        config_.cancellationCheckpointBlocks = 16;
    }
    if (config_.pyramid.maxLevels > 20) {
        config_.pyramid.maxLevels = 20;
    }
}

Renderer::Renderer(StreamingLoader& loader, RenderConfig config)
    : Renderer(std::shared_ptr<StreamingLoader>(&loader,
                                                [](StreamingLoader*) {}),
               std::move(config)) {}

Renderer::~Renderer() {
    cancelCurrentRender();
    waitUntilNotBusy();
}

void Renderer::releaseBusy() {
    {
        std::lock_guard<std::mutex> lock(stateMutex_);
        currentJobCancel_.reset();
    }
    if (hasActiveJob_.exchange(false)) {
        busyFlag_->store(false);
    }
    busyCondition_.notify_all();
}

void Renderer::markJobActive() {
    hasActiveJob_.store(true);
}

void Renderer::waitUntilNotBusy() {
    std::unique_lock<std::mutex> lock(stateMutex_);
    busyCondition_.wait(lock,
                        [&]() { return !hasActiveJob_.load(); });
}

void Renderer::cancelCurrentRender() {
    std::lock_guard<std::mutex> lock(stateMutex_);
    if (currentJobCancel_) {
        currentJobCancel_->store(true);
    }
}

bool Renderer::cancellationRequested(
    const std::shared_ptr<std::atomic<bool>>& jobCancel) const noexcept {
    return (jobCancel && jobCancel->load()) ||
           (config_.externalCancel && config_.externalCancel->load());
}

std::uint64_t Renderer::lastCommittedSnapshotVersion() const {
    std::lock_guard<std::mutex> lock(stateMutex_);
    return lastCommittedSnapshotVersion_;
}

RenderResult Renderer::render(const RenderRequest& request) {
    bool expected = false;
    if (!busyFlag_->compare_exchange_strong(expected, true)) {
        RenderResult result;
        result.code = StatusCode::E_BUSY;
        result.diagnostic = makeDiagnostic(
            StatusCode::E_BUSY, "render.job",
            "a render task is already active for this scene");
        return result;
    }
    const std::uint64_t jobId = nextJobId_.fetch_add(1);
    markJobActive();
    BusyGuard guard(this);
    try {
        auto jobCancel = std::make_shared<std::atomic<bool>>(false);
        {
            std::lock_guard<std::mutex> lock(stateMutex_);
            currentJobCancel_ = jobCancel;
        }
        return executeRender(request, jobId, jobCancel);
    } catch (const std::exception& exception) {
        RenderResult result;
        result.code = StatusCode::E_INTERNAL;
        result.diagnostic = makeDiagnostic(
            StatusCode::E_INTERNAL, "render.exception", exception.what());
        result.jobId = jobId;
        return result;
    } catch (...) {
        RenderResult result;
        result.code = StatusCode::E_INTERNAL;
        result.diagnostic = makeDiagnostic(
            StatusCode::E_INTERNAL, "render.exception", "unknown render exception");
        result.jobId = jobId;
        return result;
    }
}

AsyncRenderSubmission Renderer::submitRender(const RenderRequest& request) {
    AsyncRenderSubmission submission;
    bool expected = false;
    if (!busyFlag_->compare_exchange_strong(expected, true)) {
        submission.code = StatusCode::E_BUSY;
        submission.diagnostic = makeDiagnostic(
            StatusCode::E_BUSY, "render.job",
            "a render task is already active for this scene");
        return submission;
    }
    const std::uint64_t jobId = nextJobId_.fetch_add(1);
    markJobActive();
    try {
        auto jobCancel = std::make_shared<std::atomic<bool>>(false);
        {
            std::lock_guard<std::mutex> lock(stateMutex_);
            currentJobCancel_ = jobCancel;
        }
        submission.code = StatusCode::E_OK;
        submission.diagnostic = makeDiagnostic(StatusCode::E_OK, "", "");
        submission.jobId = jobId;
        submission.future = std::async(
            std::launch::async,
            [this, request, jobId, jobCancel]() -> RenderResult {
                BusyGuard guard(this);
                try {
                    return executeRender(request, jobId, jobCancel);
                } catch (const std::exception& exception) {
                    RenderResult result;
                    result.code = StatusCode::E_INTERNAL;
                    result.diagnostic = makeDiagnostic(
                        StatusCode::E_INTERNAL, "render.exception",
                        exception.what());
                    result.jobId = jobId;
                    return result;
                } catch (...) {
                    RenderResult result;
                    result.code = StatusCode::E_INTERNAL;
                    result.diagnostic = makeDiagnostic(
                        StatusCode::E_INTERNAL, "render.exception",
                        "unknown render exception");
                    result.jobId = jobId;
                    return result;
                }
            });
        return submission;
    } catch (const std::exception& exception) {
        releaseBusy();
        submission.code = StatusCode::E_INTERNAL;
        submission.diagnostic = makeDiagnostic(
            StatusCode::E_INTERNAL, "render.submit", exception.what());
        return submission;
    } catch (...) {
        releaseBusy();
        submission.code = StatusCode::E_INTERNAL;
        submission.diagnostic = makeDiagnostic(
            StatusCode::E_INTERNAL, "render.submit",
            "unknown error while starting render task");
        return submission;
    }
}

RenderResult Renderer::executeRender(
    const RenderRequest& request,
    std::uint64_t jobId,
    const std::shared_ptr<std::atomic<bool>>& jobCancel) {
    RenderResult result;
    result.jobId = jobId;
    if (!loader_) {
        result.code = StatusCode::E_INTERNAL;
        result.diagnostic = makeDiagnostic(StatusCode::E_INTERNAL, "render.loader",
                                           "streaming loader is null");
        return result;
    }
    if (request.camera.width <= 0 || request.camera.height <= 0 ||
        !(request.camera.nearPlane > 0.0) ||
        !(request.camera.farPlane > request.camera.nearPlane)) {
        result.code = StatusCode::E_INVALID_ARGUMENT;
        result.diagnostic = makeDiagnostic(
            StatusCode::E_INVALID_ARGUMENT, "render.camera",
            "invalid camera dimensions or depth range");
        return result;
    }

    const std::shared_ptr<const LoadSnapshot> snapshot = loader_->snapshot();
    if (!snapshot) {
        result.code = StatusCode::E_INTERNAL;
        result.diagnostic = makeDiagnostic(StatusCode::E_INTERNAL,
                                           "render.load_snapshot",
                                           "load snapshot is unavailable");
        return result;
    }
    result.snapshotVersion = snapshot->version;
    if (request.expectedSnapshotVersion.has_value() &&
        *request.expectedSnapshotVersion != snapshot->version) {
        result.code = StatusCode::E_SNAPSHOT_STALE;
        result.diagnostic = makeDiagnostic(
            StatusCode::E_SNAPSHOT_STALE, "render.expected_snapshot_version",
            "bound load snapshot does not match the expected version");
        return result;
    }
    if (config_.onSnapshotBound) {
        config_.onSnapshotBound(*snapshot);
    }
    if (cancellationRequested(jobCancel)) {
        result.code = StatusCode::E_CANCELLED;
        result.diagnostic = makeDiagnostic(
            StatusCode::E_CANCELLED, "render.checkpoint",
            "render cancelled after snapshot binding");
        return result;
    }

    const PyramidKey desiredKey =
        makeDepthPyramidKey(*snapshot, request.camera, config_.pyramid);
    std::shared_ptr<const DepthPyramid> pyramid;
    if (previousPyramid_ && previousPyramid_->key == desiredKey) {
        pyramid = previousPyramid_;
        result.pyramidRebuild.rebuilt = false;
        result.pyramidRebuild.oldVersion = pyramid->version;
        result.pyramidRebuild.newVersion = pyramid->version;
        result.pyramidRebuild.oldLevels =
            static_cast<int>(pyramid->layers.size());
        result.pyramidRebuild.newLevels =
            static_cast<int>(pyramid->layers.size());
        result.pyramidRebuild.oldDepthMin = pyramid->key.nearPlane;
        result.pyramidRebuild.newDepthMin = pyramid->key.nearPlane;
        result.pyramidRebuild.oldDepthMax = pyramid->key.quantizedDepthMax;
        result.pyramidRebuild.newDepthMax = pyramid->key.quantizedDepthMax;
        result.pyramidRebuild.oldConfigHash = pyramid->key.hash();
        result.pyramidRebuild.newConfigHash = pyramid->key.hash();
    } else {
        const std::uint64_t pyramidVersion = nextPyramidVersion_;
        DepthPyramidBuildResult build = buildDepthPyramid(
            *snapshot, request.camera, config_.pyramid, pyramidVersion,
            jobCancel.get());
        if (!build) {
            result.code = build.diagnostic.code;
            result.diagnostic = build.diagnostic;
            return result;
        }
        ++nextPyramidVersion_;
        pyramid = std::move(build.pyramid);
        result.pyramidRebuild = makeRebuildReport(previousPyramid_, *pyramid);
        previousPyramid_ = pyramid;
    }
    result.pyramidVersion = pyramid->version;
    if (cancellationRequested(jobCancel)) {
        result.code = StatusCode::E_CANCELLED;
        result.diagnostic = makeDiagnostic(
            StatusCode::E_CANCELLED, "render.checkpoint",
            "render cancelled after pyramid construction");
        return result;
    }

    result.blocks.reserve(snapshot->blocks.size());
    std::size_t visited = 0;
    for (const LoadedBlockRef& reference : snapshot->blocks) {
        if (reference.block == nullptr) {
            result.code = StatusCode::E_INTERNAL;
            result.diagnostic = makeDiagnostic(
                StatusCode::E_INTERNAL, "render.blocks",
                "load snapshot contains a null block", reference.id);
            return result;
        }
        result.blocks.push_back(evaluateBlockVisibility(
            *reference.block, request.camera, *pyramid, config_.occlusion));
        ++visited;
        if ((visited % config_.cancellationCheckpointBlocks) == 0 &&
            cancellationRequested(jobCancel)) {
            result.code = StatusCode::E_CANCELLED;
            result.diagnostic = makeDiagnostic(
                StatusCode::E_CANCELLED, "render.block_batch",
                "render cancelled at a block batch checkpoint");
            result.blocks.clear();
            return result;
        }
    }
    if (cancellationRequested(jobCancel)) {
        result.code = StatusCode::E_CANCELLED;
        result.diagnostic = makeDiagnostic(
            StatusCode::E_CANCELLED, "render.checkpoint",
            "render cancelled before mask composition");
        result.blocks.clear();
        return result;
    }

    result.mask = composeVisibilityMask(*snapshot, request.camera, *pyramid,
                                        result.blocks);
    Diagnostic bindingDiagnostic;
    const StatusCode bindingStatus = validateVisibilityMaskBinding(
        result.mask, *snapshot, *pyramid, &bindingDiagnostic);
    if (bindingStatus != StatusCode::E_OK) {
        result.code = bindingStatus;
        result.diagnostic = bindingDiagnostic;
        result.mask = VisibilityMask{};
        result.blocks.clear();
        return result;
    }
    if (cancellationRequested(jobCancel)) {
        result.code = StatusCode::E_CANCELLED;
        result.diagnostic = makeDiagnostic(
            StatusCode::E_CANCELLED, "render.checkpoint",
            "render cancelled before summary commit");
        result.mask = VisibilityMask{};
        result.blocks.clear();
        return result;
    }

    FrameErrorSummary summary;
    summary.snapshotVersion = snapshot->version;
    summary.pyramidVersion = pyramid->version;
    summary.loadContentHash = snapshot->contentHash;
    summary.maskHash = result.mask.contentHash;
    summary.totalBlocks = result.blocks.size();
    for (const BlockVisibility& block : result.blocks) {
        switch (block.decision) {
            case VisibilityDecision::Visible:
                ++summary.visibleBlocks;
                break;
            case VisibilityDecision::Occluded:
                ++summary.occludedBlocks;
                break;
            case VisibilityDecision::Uncertain:
                ++summary.uncertainBlocks;
                summary.maskDeviationUpperBoundPixels += block.screenBounds.area();
                break;
            case VisibilityDecision::FrustumCulled:
                ++summary.culledBlocks;
                break;
        }
        summary.layerSwitches +=
            static_cast<std::uint64_t>(block.layerSwitches);
        summary.bboxTransformErrorContribution +=
            block.errors.bboxTransformErrorWorld;
        summary.pyramidQuantizationErrorContribution +=
            block.errors.pyramidQuantizationErrorWorld;
        summary.layerSwitchJumpContribution +=
            block.errors.layerSwitchJumpWorld;
        summary.frustumBoundaryErrorContribution +=
            block.errors.frustumBoundaryErrorWorld;
        summary.occlusionErrorUpperBoundMax =
            std::max(summary.occlusionErrorUpperBoundMax,
                     block.errors.occlusionErrorUpperBoundWorld);
    }
    summary.criticalBlocks = summary.uncertainBlocks;
    result.summary = summary;
    result.newerSnapshotAvailable =
        loader_->latestVersion() != snapshot->version;

    if (!config_.snapshotPath.empty()) {
        SnapshotDocument document =
            makeSnapshotDocument(*snapshot, request.camera, *pyramid,
                                 result.mask.contentHash, result.mask.width,
                                 result.mask.height, result.summary);
        const SnapshotSaveResult saved =
            saveSnapshotAtomic(config_.snapshotPath, document);
        if (!saved) {
            result.code = saved.code;
            result.diagnostic = saved.diagnostic;
            return result;
        }
    }
    {
        std::lock_guard<std::mutex> lock(stateMutex_);
        lastCommittedSnapshotVersion_ = snapshot->version;
    }
    result.code = StatusCode::E_OK;
    result.diagnostic = makeDiagnostic(StatusCode::E_OK, "", "");
    return result;
}

SnapshotRecoveryCheck checkSnapshotRecovery(
    const std::string& path,
    const LoadSnapshot& currentLoadSnapshot,
    const Camera& currentCamera) {
    SnapshotRecoveryCheck check;
    SnapshotCurrentState current =
        currentStateFromSnapshot(currentLoadSnapshot, currentCamera);
    const SnapshotLoadResult loaded = loadSnapshot(path, current);
    check.code = loaded.code;
    check.diagnostic = loaded.diagnostic;
    check.requiresRerender = loaded.requiresRerender;
    check.changes = loaded.changes;
    if (loaded) {
        check.recoveredSnapshotVersion = loaded.document.snapshotVersion;
        check.recoveredMaskHash = loaded.document.maskHash;
    }
    return check;
}

}  // namespace preview
