#include "test_helpers.hpp"

#include <algorithm>
#include <cstring>
#include <fstream>
#include <future>
#include <iostream>
#include <set>

namespace {

preview::RenderConfig renderConfig(const std::filesystem::path& snapshotPath) {
    preview::RenderConfig config;
    config.pyramid.width = 160;
    config.pyramid.height = 90;
    config.pyramid.quantizationBits = 18;
    config.pyramid.maxLevels = 10;
    config.cancellationCheckpointBlocks = 8;
    config.snapshotPath = snapshotPath.string();
    return config;
}

const preview::BlockVisibility* findDecision(
    const std::vector<preview::BlockVisibility>& decisions,
    preview::BlockId id) {
    for (const preview::BlockVisibility& decision : decisions) {
        if (decision.id == id) {
            return &decision;
        }
    }
    return nullptr;
}

void testDeterministicRenderAndSummary() {
    const auto config = test_support::mediumSceneConfig();
    const auto scene = test_support::validatedScene(
        preview::generateSampleScene(config));
    preview::StreamingLoader loader(scene, preview::StreamingConfig{3});
    const auto snapshot = test_support::loadEverything(&loader);
    const auto directory = test_support::temporaryDirectory("render");
    const auto snapshotPath = directory / "frame.snapshot";
    preview::Renderer renderer(loader, renderConfig(snapshotPath));
    const preview::Camera camera = test_support::firstView(config).camera;
    preview::RenderRequest request;
    request.camera = camera;

    const preview::RenderResult first = renderer.render(request);
    if (!first) {
        throw std::runtime_error("first render failed: " +
                                 first.diagnostic.toJson());
    }
    TEST_CHECK(first.snapshotVersion == snapshot->version);
    TEST_CHECK(first.pyramidVersion >= 1);
    TEST_CHECK(first.mask.width == camera.width);
    TEST_CHECK(first.mask.height == camera.height);
    TEST_CHECK(first.mask.pixels.size() ==
               static_cast<std::size_t>(camera.width * camera.height));
    TEST_CHECK(first.mask.contentHash != 0);
    preview::VisibilityMask mismatchedMask = first.mask;
    mismatchedMask.snapshotVersion += 1;
    preview::DepthPyramid maskBindingPyramid;
    maskBindingPyramid.version = first.pyramidVersion;
    TEST_CHECK(preview::validateVisibilityMaskBinding(
                   first.mask, *snapshot, maskBindingPyramid) ==
               preview::StatusCode::E_OK);
    TEST_CHECK(preview::validateVisibilityMaskBinding(
                   mismatchedMask, *snapshot, maskBindingPyramid) ==
               preview::StatusCode::E_SNAPSHOT_STALE);
    TEST_CHECK(first.summary.totalBlocks == snapshot->blocks.size());
    TEST_CHECK(first.summary.visibleBlocks + first.summary.occludedBlocks +
                   first.summary.uncertainBlocks + first.summary.culledBlocks ==
               first.summary.totalBlocks);
    TEST_CHECK(first.summary.visibleBlocks > 0);
    TEST_CHECK(first.mask.nonEmptyPixels() > 0);
    TEST_CHECK(first.summary.maskHash == first.mask.contentHash);
    TEST_CHECK(first.pyramidRebuild.rebuilt);
    TEST_CHECK(first.summary.summaryHash() != 0);

    const preview::RenderResult second = renderer.render(request);
    if (!second) {
        throw std::runtime_error("second render failed: " +
                                 second.diagnostic.toJson());
    }
    TEST_CHECK(second.snapshotVersion == first.snapshotVersion);
    TEST_CHECK(second.pyramidVersion == first.pyramidVersion);
    TEST_CHECK(second.mask.contentHash == first.mask.contentHash);
    TEST_CHECK(second.mask.pixels == first.mask.pixels);
    TEST_CHECK(second.summary.canonicalJson() == first.summary.canonicalJson());
    TEST_CHECK(second.summary.summaryHash() == first.summary.summaryHash());
    TEST_CHECK(!second.pyramidRebuild.rebuilt);

    std::filesystem::remove_all(directory);
}

void testUncertaintyClassification() {
    preview::SceneBlock block;
    block.id = 7;
    block.bounds.min = {-0.5, -0.5, -5.5};
    block.bounds.max = {0.5, 0.5, -4.5};

    preview::Camera camera;
    camera.position = {0.0, 0.0, 0.0};
    camera.target = {0.0, 0.0, -10.0};
    camera.nearPlane = 0.1;
    camera.farPlane = 20.0;
    camera.width = 100;
    camera.height = 100;

    preview::DepthPyramid pyramid;
    pyramid.version = 3;
    pyramid.key.nearPlane = 0.1;
    pyramid.key.farPlane = 20.0;
    pyramid.key.quantizedDepthMax = 20.0;
    pyramid.key.width = 100;
    pyramid.key.height = 100;
    pyramid.key.levels = 1;
    preview::DepthPyramidLayer layer;
    layer.level = 0;
    layer.width = 100;
    layer.height = 100;
    layer.quantStep = 1.0;
    layer.codes.assign(10000, 4u);
    pyramid.layers.push_back(std::move(layer));

    const preview::BlockVisibility uncertain =
        preview::evaluateBlockVisibility(block, camera, pyramid);
    TEST_CHECK(uncertain.decision == preview::VisibilityDecision::Uncertain);
    TEST_CHECK(uncertain.code == preview::StatusCode::E_OCCLUSION_UNCERTAIN);
    TEST_CHECK(uncertain.marginLower <= 0.0);
    TEST_CHECK(uncertain.marginUpper >= 0.0);
    TEST_CHECK(uncertain.marginLower < uncertain.marginUpper);
    TEST_CHECK(uncertain.selectedLevel == 0);
    TEST_CHECK(uncertain.errors.pyramidQuantizationErrorWorld > 0.0);

    pyramid.layers[0].codes.assign(10000, 2u);
    const preview::BlockVisibility occluded =
        preview::evaluateBlockVisibility(block, camera, pyramid);
    TEST_CHECK(occluded.decision == preview::VisibilityDecision::Occluded);
    TEST_CHECK(occluded.marginLower > 0.0);

    preview::SceneBlock frontBlock = block;
    frontBlock.id = 8;
    frontBlock.bounds.min.z = -1.5;
    frontBlock.bounds.max.z = -0.5;
    pyramid.layers[0].codes.assign(10000, 4u);
    const preview::BlockVisibility visible =
        preview::evaluateBlockVisibility(frontBlock, camera, pyramid);
    TEST_CHECK(visible.decision == preview::VisibilityDecision::Visible);

    preview::DepthPyramid layeredPyramid;
    layeredPyramid.version = 5;
    layeredPyramid.key = pyramid.key;
    layeredPyramid.key.levels = 2;
    preview::DepthPyramidLayer fineLayer;
    fineLayer.level = 0;
    fineLayer.width = 100;
    fineLayer.height = 100;
    fineLayer.quantStep = 0.1;
    fineLayer.codes.assign(10000, 0u);
    preview::DepthPyramidLayer coarseLayer;
    coarseLayer.level = 1;
    coarseLayer.width = 50;
    coarseLayer.height = 50;
    coarseLayer.quantStep = 2.0;
    coarseLayer.codes.assign(2500, 2u);
    layeredPyramid.layers.push_back(std::move(fineLayer));
    layeredPyramid.layers.push_back(std::move(coarseLayer));
    const preview::BlockVisibility descended =
        preview::evaluateBlockVisibility(block, camera, layeredPyramid);
    TEST_CHECK(descended.decision == preview::VisibilityDecision::Occluded);
    TEST_CHECK(descended.selectedLevel == 0);
    TEST_CHECK(descended.layersVisited == 2);
    TEST_CHECK(descended.layerSwitches == 1);
    TEST_CHECK(descended.errors.layerSwitchJumpWorld > 0.0);
}

void testSnapshotVersionIsolation() {
    const auto config = test_support::mediumSceneConfig();
    const auto scene = test_support::validatedScene(
        preview::generateSampleScene(config));
    preview::StreamingLoader loader(scene, preview::StreamingConfig{1});
    loader.start();
    std::vector<preview::BlockId> initialIds;
    for (std::size_t i = 0;
         i < std::min<std::size_t>(4, scene->blocks.size()); ++i) {
        initialIds.push_back(scene->blocks[i].id);
        TEST_CHECK(loader.enqueueBlock(scene->blocks[i].id));
    }
    TEST_CHECK(loader.waitUntilIdle());
    const auto oldSnapshot = loader.snapshot();
    TEST_CHECK(oldSnapshot != nullptr);
    TEST_CHECK(oldSnapshot->blocks.size() == initialIds.size());
    preview::BlockId extraId = preview::kInvalidBlockId;
    for (const preview::SceneBlock& block : scene->blocks) {
        if (oldSnapshot->findBlock(block.id) == nullptr) {
            extraId = block.id;
            break;
        }
    }
    TEST_CHECK(extraId != preview::kInvalidBlockId);

    preview::RenderConfig configRenderer =
        renderConfig(std::filesystem::path{});
    configRenderer.snapshotPath.clear();
    std::atomic<bool> callbackRan{false};
    configRenderer.onSnapshotBound = [&](const preview::LoadSnapshot& bound) {
        callbackRan.store(true);
        TEST_CHECK(bound.version == oldSnapshot->version);
        loader.enqueueBlock(extraId);
        TEST_CHECK(loader.waitUntilIdle());
    };
    preview::Renderer renderer(loader, configRenderer);
    preview::RenderRequest request;
    request.camera = test_support::firstView(config).camera;
    const preview::RenderResult rendered = renderer.render(request);
    if (!rendered) {
        throw std::runtime_error("version isolation render failed: " +
                                 rendered.diagnostic.toJson());
    }
    TEST_CHECK(callbackRan.load());
    TEST_CHECK(rendered.snapshotVersion == oldSnapshot->version);
    TEST_CHECK(rendered.mask.snapshotVersion == oldSnapshot->version);
    TEST_CHECK(rendered.mask.loadContentHash == oldSnapshot->contentHash);
    TEST_CHECK(rendered.summary.loadContentHash == oldSnapshot->contentHash);
    TEST_CHECK(rendered.summary.totalBlocks == oldSnapshot->blocks.size());
    TEST_CHECK(rendered.newerSnapshotAvailable);
    TEST_CHECK(loader.latestVersion() != oldSnapshot->version);

    preview::RenderRequest staleRequest = request;
    staleRequest.expectedSnapshotVersion = oldSnapshot->version;
    const preview::RenderResult stale = renderer.render(staleRequest);
    TEST_CHECK(stale.code == preview::StatusCode::E_SNAPSHOT_STALE);
    TEST_CHECK(!renderer.busy());
}

void testBusyAndCooperativeCancellation() {
    const auto config = test_support::mediumSceneConfig();
    const auto scene = test_support::validatedScene(
        preview::generateSampleScene(config));
    preview::StreamingLoader loader(scene, preview::StreamingConfig{4});
    test_support::loadEverything(&loader);
    const preview::Camera camera = test_support::firstView(config).camera;
    preview::RenderRequest request;
    request.camera = camera;

    std::promise<void> boundPromise;
    std::future<void> boundFuture = boundPromise.get_future();
    std::promise<void> releasePromise;
    std::shared_future<void> releaseFuture =
        releasePromise.get_future().share();
    preview::RenderConfig busyConfig = renderConfig(std::filesystem::path{});
    busyConfig.snapshotPath.clear();
    busyConfig.onSnapshotBound = [&](const preview::LoadSnapshot&) {
        boundPromise.set_value();
        releaseFuture.wait();
    };
    preview::Renderer busyRenderer(loader, busyConfig);
    preview::AsyncRenderSubmission submission =
        busyRenderer.submitRender(request);
    TEST_CHECK(submission.ok());
    boundFuture.wait();

    const preview::AsyncRenderSubmission duplicate =
        busyRenderer.submitRender(request);
    TEST_CHECK(duplicate.code == preview::StatusCode::E_BUSY);
    preview::Renderer secondRendererSameScene(loader, busyConfig);
    const preview::AsyncRenderSubmission crossRenderer =
        secondRendererSameScene.submitRender(request);
    TEST_CHECK(crossRenderer.code == preview::StatusCode::E_BUSY);
    TEST_CHECK(secondRendererSameScene.busy());
    const preview::RenderResult synchronousDuplicate =
        busyRenderer.render(request);
    TEST_CHECK(synchronousDuplicate.code == preview::StatusCode::E_BUSY);
    releasePromise.set_value();
    const preview::RenderResult first = submission.future.get();
    TEST_CHECK(first.code == preview::StatusCode::E_OK);
    TEST_CHECK(!busyRenderer.busy());

    const auto directory = test_support::temporaryDirectory("cancel");
    const auto snapshotPath = directory / "committed.snapshot";
    auto cancelFlag = std::make_shared<std::atomic<bool>>(false);
    preview::RenderConfig cancelConfig = renderConfig(snapshotPath);
    cancelConfig.externalCancel = cancelFlag;
    preview::Renderer cancelRenderer(loader, cancelConfig);
    const preview::RenderResult committed = cancelRenderer.render(request);
    TEST_CHECK(committed.code == preview::StatusCode::E_OK);

    std::ifstream before(snapshotPath, std::ios::binary);
    const std::string beforeBytes((std::istreambuf_iterator<char>(before)),
                                  std::istreambuf_iterator<char>());
    TEST_CHECK(!beforeBytes.empty());
    cancelFlag->store(true);
    const preview::RenderResult cancelled = cancelRenderer.render(request);
    TEST_CHECK(cancelled.code == preview::StatusCode::E_CANCELLED);
    TEST_CHECK(!cancelRenderer.busy());
    std::ifstream after(snapshotPath, std::ios::binary);
    const std::string afterBytes((std::istreambuf_iterator<char>(after)),
                                 std::istreambuf_iterator<char>());
    TEST_CHECK(beforeBytes == afterBytes);
    TEST_CHECK(checkSnapshotRecovery(snapshotPath.string(), *loader.snapshot(),
                                     camera)
                   .ok());
    std::filesystem::remove_all(directory);
}

}  // namespace

int main() {
    try {
        testDeterministicRenderAndSummary();
        testUncertaintyClassification();
        testSnapshotVersionIsolation();
        testBusyAndCooperativeCancellation();
        std::cout << "test_render: PASS\n";
        return 0;
    } catch (const std::exception& exception) {
        std::cerr << "test_render: FAIL: " << exception.what() << "\n";
        return 1;
    }
}
