#pragma once

#include "preview/renderer.hpp"
#include "preview/sample_scene.hpp"
#include "preview/scene_io.hpp"
#include "preview/streaming.hpp"

#include <atomic>
#include <chrono>
#include <cstdint>
#include <filesystem>
#include <sstream>
#include <stdexcept>
#include <string>
#include <thread>
#include <vector>

#if defined(_WIN32)
#include <process.h>
#else
#include <unistd.h>
#endif

namespace test_support {

#define TEST_CHECK(condition)                                                \
    do {                                                                     \
        if (!(condition)) {                                                  \
            std::ostringstream testMessage;                                  \
            testMessage << "CHECK failed at " << __FILE__ << ":" << __LINE__ \
                        << " -> " #condition;                                \
            throw std::runtime_error(testMessage.str());                     \
        }                                                                    \
    } while (false)

inline std::uint64_t processId() {
#if defined(_WIN32)
    return static_cast<std::uint64_t>(_getpid());
#else
    return static_cast<std::uint64_t>(getpid());
#endif
}

inline std::filesystem::path temporaryDirectory(const std::string& name) {
    const auto path = std::filesystem::temp_directory_path() /
                      ("chunk_preview_" + name + "_" +
                       std::to_string(processId()));
    std::error_code error;
    std::filesystem::remove_all(path, error);
    std::filesystem::create_directories(path, error);
    return path;
}

inline std::shared_ptr<const preview::Scene> validatedScene(
    const preview::SampleSceneData& data) {
    std::vector<std::uint8_t> bytes;
    preview::Diagnostic diagnostic;
    TEST_CHECK(preview::encodeSceneBinary(data.roots, data.blocks, 0, &bytes,
                                          &diagnostic));
    preview::SceneLoadResult loaded = preview::parseSceneBinary(bytes);
    if (!loaded) {
        throw std::runtime_error("sample scene failed validation: " +
                                 loaded.diagnostic.toJson());
    }
    return loaded.scene;
}

inline preview::SampleSceneConfig mediumSceneConfig() {
    preview::SampleSceneConfig config;
    config.gridX = 5;
    config.gridY = 4;
    config.depthLayers = 3;
    config.occluderDensity = 0.72;
    config.seed = 0xabc123ull;
    return config;
}

inline preview::SampleView firstView(
    const preview::SampleSceneConfig& config = mediumSceneConfig()) {
    return preview::generateSampleViews(config, 6, 160, 90).front();
}

inline std::shared_ptr<const preview::LoadSnapshot> loadEverything(
    preview::StreamingLoader* loader,
    std::chrono::milliseconds timeout = std::chrono::milliseconds(30000)) {
    loader->start();
    loader->enqueueAll();
    TEST_CHECK(loader->waitUntilIdle(timeout));
    const auto snapshot = loader->snapshot();
    TEST_CHECK(snapshot != nullptr);
    TEST_CHECK(snapshot->blocks.size() ==
               snapshot->scene->blocks.size());
    return snapshot;
}

inline bool snapshotHashConsistent(const preview::LoadSnapshot& snapshot) {
    const preview::LoadSnapshotResult rebuilt = preview::makeLoadSnapshot(
        snapshot.scene, snapshot.ids(), snapshot.version);
    return rebuilt.snapshot != nullptr &&
           rebuilt.snapshot->contentHash == snapshot.contentHash;
}

inline void sleepBriefly() {
    std::this_thread::sleep_for(std::chrono::milliseconds(2));
}

}  // namespace test_support
