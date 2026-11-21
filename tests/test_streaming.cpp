#include "test_helpers.hpp"

#include <iostream>
#include <mutex>
#include <set>

namespace {

void testDeduplicationAndPriorities() {
    preview::SampleSceneConfig config;
    config.gridX = 3;
    config.gridY = 3;
    config.depthLayers = 2;
    const auto scene = test_support::validatedScene(
        preview::generateSampleScene(config));
    preview::StreamingLoader loader(scene, preview::StreamingConfig{4});
    const preview::BlockId first = scene->blocks.front().id;
    TEST_CHECK(loader.enqueueBlock(first));
    TEST_CHECK(!loader.enqueueBlock(first));
    TEST_CHECK(loader.pendingCount() == 1);
    loader.setViewpoint(test_support::firstView(config).camera);
    const std::size_t scheduled = loader.scheduleForView(
        test_support::firstView(config).camera);
    TEST_CHECK(scheduled == scene->blocks.size());
    loader.start();
    TEST_CHECK(loader.waitUntilIdle());
    const auto snapshot = loader.snapshot();
    TEST_CHECK(snapshot != nullptr);
    TEST_CHECK(snapshot->blocks.size() == scene->blocks.size());
    TEST_CHECK(loader.loadedCount() == scene->blocks.size());
    TEST_CHECK(snapshot->version >= 1);
    TEST_CHECK(test_support::snapshotHashConsistent(*snapshot));
}

void testImmutableSnapshotReplacement() {
    preview::SampleSceneConfig config;
    config.gridX = 8;
    config.gridY = 6;
    config.depthLayers = 4;
    config.occluderDensity = 0.8;
    const auto scene = test_support::validatedScene(
        preview::generateSampleScene(config));
    preview::StreamingLoader loader(scene, preview::StreamingConfig{1});
    loader.start();
    std::atomic<bool> stopRead{false};
    std::atomic<bool> readerFailed{false};
    std::mutex failureMutex;
    std::string failureMessage;
    std::vector<std::thread> readers;
    for (int readerIndex = 0; readerIndex < 2; ++readerIndex) {
        readers.emplace_back([&]() {
            try {
                while (!stopRead.load()) {
                    const auto snapshot = loader.snapshot();
                    if (!snapshot) {
                        throw std::runtime_error("null snapshot");
                    }
                    std::set<preview::BlockId> ids;
                    for (const preview::LoadedBlockRef& block :
                         snapshot->blocks) {
                        if (block.block == nullptr ||
                            block.block->id != block.id ||
                            !ids.insert(block.id).second) {
                            throw std::runtime_error("mixed snapshot blocks");
                        }
                    }
                    if (!test_support::snapshotHashConsistent(*snapshot)) {
                        throw std::runtime_error("snapshot hash mismatch");
                    }
                }
            } catch (const std::exception& exception) {
                std::lock_guard<std::mutex> lock(failureMutex);
                failureMessage = exception.what();
                readerFailed.store(true);
            }
        });
    }

    loader.scheduleForView(test_support::firstView(config).camera);
    TEST_CHECK(loader.waitUntilIdle());
    stopRead.store(true);
    for (std::thread& reader : readers) {
        reader.join();
    }
    TEST_CHECK(!readerFailed.load());
    const auto snapshot = loader.snapshot();
    TEST_CHECK(snapshot != nullptr);
    TEST_CHECK(snapshot->version > 1);
    TEST_CHECK(snapshot->blocks.size() == scene->blocks.size());
    TEST_CHECK(test_support::snapshotHashConsistent(*snapshot));
}

}  // namespace

int main() {
    try {
        testDeduplicationAndPriorities();
        testImmutableSnapshotReplacement();
        std::cout << "test_streaming: PASS\n";
        return 0;
    } catch (const std::exception& exception) {
        std::cerr << "test_streaming: FAIL: " << exception.what() << "\n";
        return 1;
    }
}
