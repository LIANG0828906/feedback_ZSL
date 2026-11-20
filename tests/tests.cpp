#include <algorithm>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

#include "shadowpipe/cascades.hpp"
#include "shadowpipe/hash.hpp"
#include "shadowpipe/pyramid.hpp"
#include "shadowpipe/renderer.hpp"
#include "shadowpipe/scene.hpp"
#include "shadowpipe/snapshot.hpp"
#include "shadowpipe/synthetic.hpp"

namespace {

int failures = 0;

#define CHECK(condition)                                                        \
    do {                                                                        \
        if (!(condition)) {                                                     \
            std::cerr << "CHECK failed at " << __FILE__ << ":" << __LINE__     \
                      << ": " #condition "\n";                                  \
            ++failures;                                                         \
        }                                                                       \
    } while (false)

std::vector<std::uint8_t> readFile(const std::string& path) {
    std::ifstream in(path, std::ios::binary | std::ios::ate);
    if (!in) {
        return {};
    }
    const std::streamoff size = in.tellg();
    in.seekg(0, std::ios::beg);
    std::vector<std::uint8_t> bytes(static_cast<std::size_t>(size));
    if (!bytes.empty()) {
        in.read(reinterpret_cast<char*>(bytes.data()), size);
    }
    return bytes;
}

std::string readText(const std::string& path) {
    const std::vector<std::uint8_t> bytes = readFile(path);
    return std::string(bytes.begin(), bytes.end());
}

shadowpipe::RenderConfig testConfig() {
    shadowpipe::RenderConfig config;
    config.width = 128;
    config.height = 96;
    config.cascade_count = 3;
    config.shadow_resolution = 128;
    config.pyramid_resolution = 128;
    config.switch_tolerance = 0.02f;
    return config;
}

shadowpipe::OutputPaths outputPaths(const std::string& directory) {
    shadowpipe::OutputPaths paths;
    paths.directory = directory;
    paths.snapshot_file =
        (std::filesystem::path(directory) / "snapshot.bin").string();
    return paths;
}

void testSceneParsing(const std::string& data_directory) {
    const shadowpipe::SceneLoadResult valid =
        shadowpipe::loadSceneFromFile(
            (std::filesystem::path(data_directory) / "synthetic.rscene").string());
    CHECK(valid.status.ok());
    if (!valid.status.ok()) {
        return;
    }
    CHECK(valid.scene.nodes.size() >= 8);
    CHECK(valid.scene.meshes.size() == 3);
    CHECK(valid.scene.content_hash != 0);

    const char* invalid_files[] = {"invalid_index.rscene",
                                   "invalid_transform.rscene",
                                   "invalid_bounds.rscene"};
    for (const char* filename : invalid_files) {
        const shadowpipe::SceneLoadResult invalid =
            shadowpipe::loadSceneFromFile(
                (std::filesystem::path(data_directory) / filename).string());
        CHECK(invalid.status.code == shadowpipe::ErrorCode::SceneInvalid);
        CHECK(invalid.status.node_index == 3);
        CHECK(!invalid.status.node_name.empty());
        CHECK(invalid.scene.nodes.empty());
    }
}

void testCascadesAndPyramid(const shadowpipe::Scene& scene,
                            const shadowpipe::RenderConfig& config) {
    shadowpipe::CascadeTable table;
    const shadowpipe::Status status =
        shadowpipe::buildCascadeTable(scene, config, table);
    CHECK(status.ok());
    CHECK(table.cascades.size() == config.cascade_count);
    CHECK(table.boundaries.size() + 1 == config.cascade_count);
    for (const auto& boundary : table.boundaries) {
        CHECK(boundary.texel_coverage_difference >= 0.0f);
        CHECK(boundary.relative_texel_coverage_difference >= 0.0f);
    }
    CHECK(shadowpipe::selectCascadeForDepth(table, 0.0f) == 0);
    CHECK(shadowpipe::selectCascadeForDepth(table, 1.0e9f) ==
          config.cascade_count - 1);

    shadowpipe::OccluderPyramid pyramid;
    const shadowpipe::Status pyramid_status =
        shadowpipe::buildOccluderPyramid(scene, config, nullptr, pyramid);
    CHECK(pyramid_status.ok());
    CHECK(!pyramid.levels.empty());
    CHECK(pyramid.rebuild_report.rebuilt);
    CHECK(pyramid.rebuild_report.max_quantization_error >= 0.0f);
    CHECK(pyramid.levels.front().meta.quantization_step > 0.0f);
    if (pyramid.levels.size() > 1) {
        CHECK(pyramid.levels.back().meta.quantization_bits <=
              pyramid.levels.front().meta.quantization_bits);
    }

    std::uint32_t ambiguous = 0;
    for (std::size_t node_index = 0; node_index < scene.nodes.size(); ++node_index) {
        const shadowpipe::Node& node = scene.nodes[node_index];
        if (node.mesh_index == shadowpipe::kNoMesh) {
            continue;
        }
        const shadowpipe::OcclusionQueryResult query =
            shadowpipe::queryOcclusion(
                pyramid, node.content_bounds,
                static_cast<std::uint32_t>(node_index));
        CHECK(!query.levels.empty());
        if (query.ambiguous) {
            ++ambiguous;
            CHECK(query.status.code ==
                  shadowpipe::ErrorCode::OcclusionAmbiguous);
            CHECK(std::isfinite(query.margin_lower));
            CHECK(std::isfinite(query.margin_upper));
        }
    }
    CHECK(ambiguous > 0);
}

void testDeterministicRender(const shadowpipe::Scene& scene,
                             const shadowpipe::RenderConfig& config,
                             const std::string& work) {
    shadowpipe::RenderService service;
    const std::string out_a = (std::filesystem::path(work) / "out_a").string();
    const std::string out_b = (std::filesystem::path(work) / "out_b").string();
    const shadowpipe::RenderResult a =
        service.render(scene, config, outputPaths(out_a), nullptr);
    const shadowpipe::RenderResult b =
        service.render(scene, config, outputPaths(out_b), nullptr);
    CHECK(a.status.ok());
    CHECK(b.status.ok());
    CHECK(a.summary.occlusion_ambiguous_count > 0);
    CHECK(a.summary.frustum_culled_count > 0);
    CHECK(a.summary.occluded_object_count > 0);
    CHECK(a.summary.cascade_switch_count > 0);
    CHECK(a.summary.bias_metrics.size() == config.cascade_count);
    CHECK(a.summary.frame_hash == b.summary.frame_hash);

    const char* byte_identical_files[] = {
        "shadow_factor.pgm", "shadow_factor.f32", "visibility_mask.pgm",
        "error_heatmap.ppm", "summary.json",       "snapshot.bin",
    };
    for (const char* filename : byte_identical_files) {
        const std::vector<std::uint8_t> bytes_a =
            readFile((std::filesystem::path(out_a) / filename).string());
        const std::vector<std::uint8_t> bytes_b =
            readFile((std::filesystem::path(out_b) / filename).string());
        CHECK(!bytes_a.empty());
        CHECK(bytes_a == bytes_b);
    }

    const std::string summary =
        readText((std::filesystem::path(out_a) / "summary.json").string());
    CHECK(summary.find("\"cascade_switch_count\"") != std::string::npos);
    CHECK(summary.find("\"occlusion_ambiguous_count\"") != std::string::npos);
    CHECK(summary.find("\"bias_error_metrics\"") != std::string::npos);
    CHECK(summary.find("\"shadow_factor_delta_upper_bound\"") !=
          std::string::npos);
    CHECK(summary.find("\"old_new_level_range_difference_max\"") !=
          std::string::npos);

    const shadowpipe::SnapshotLoadResult loaded =
        shadowpipe::loadSnapshotFile(
            (std::filesystem::path(out_a) / "snapshot.bin").string());
    CHECK(loaded.status.ok());
    if (loaded.status.ok()) {
        CHECK(loaded.snapshot.cascade_table.cascades.size() ==
              config.cascade_count);
        CHECK(!loaded.snapshot.pyramid.levels.empty());
        CHECK(loaded.snapshot.bias.depth_bias == config.depth_bias);
        const std::vector<std::uint8_t> summary_bytes =
            readFile((std::filesystem::path(out_a) / "summary.json").string());
        CHECK(loaded.snapshot.hashes.summary_json ==
              shadowpipe::hashBytes(summary_bytes.data(),
                                    summary_bytes.size()));
        shadowpipe::CascadeTable table;
        CHECK(shadowpipe::buildCascadeTable(scene, config, table).ok());
        const shadowpipe::SnapshotVersionDiff same =
            shadowpipe::compareSnapshotToFrame(loaded.snapshot,
                                               scene.content_hash, config, table);
        CHECK(!same.rerenderRequired());

        shadowpipe::RenderConfig moved = config;
        moved.camera.eye.x += 0.5f;
        const shadowpipe::SnapshotVersionDiff view_diff =
            shadowpipe::compareSnapshotToFrame(loaded.snapshot,
                                               scene.content_hash, moved, table);
        CHECK(view_diff.view_changed);
        CHECK(view_diff.rerenderRequired());

        shadowpipe::CascadeTable changed_table;
        shadowpipe::RenderConfig changed_cascades = config;
        changed_cascades.cascade_count = 2;
        CHECK(shadowpipe::buildCascadeTable(scene, changed_cascades,
                                            changed_table)
                  .ok());
        const shadowpipe::SnapshotVersionDiff cascade_diff =
            shadowpipe::compareSnapshotToFrame(loaded.snapshot,
                                               scene.content_hash, changed_cascades,
                                               changed_table);
        CHECK(cascade_diff.cascade_table_changed);

        std::vector<std::uint8_t> corrupt = readFile(
            (std::filesystem::path(out_a) / "snapshot.bin").string());
        if (!corrupt.empty()) {
            corrupt[corrupt.size() / 2] ^= 0x5au;
            std::ofstream out(
                (std::filesystem::path(work) / "corrupt.snapshot").string(),
                std::ios::binary | std::ios::trunc);
            out.write(reinterpret_cast<const char*>(corrupt.data()),
                      static_cast<std::streamsize>(corrupt.size()));
        }
        const shadowpipe::SnapshotLoadResult mismatch =
            shadowpipe::loadSnapshotFile(
                (std::filesystem::path(work) / "corrupt.snapshot").string());
        CHECK(mismatch.status.code ==
              shadowpipe::ErrorCode::SnapshotMismatch);
    }
}

void testBiasAndCancellation(const shadowpipe::Scene& scene,
                             const shadowpipe::RenderConfig& config,
                             const std::string& work) {
    shadowpipe::RenderService service;
    shadowpipe::RenderConfig invalid = config;
    invalid.depth_bias = -0.1f;
    const std::string invalid_directory =
        (std::filesystem::path(work) / "out_bias").string();
    const shadowpipe::RenderResult invalid_result =
        service.render(scene, invalid, outputPaths(invalid_directory), nullptr);
    CHECK(invalid_result.status.code == shadowpipe::ErrorCode::BiasInvalid);
    CHECK(!std::filesystem::exists(
        std::filesystem::path(invalid_directory) / "shadow_factor.pgm"));
    CHECK(std::filesystem::exists(
        std::filesystem::path(invalid_directory) / "summary.json"));

    const std::string cancel_directory =
        (std::filesystem::path(work) / "out_cancel").string();
    const shadowpipe::RenderResult committed =
        service.render(scene, config, outputPaths(cancel_directory), nullptr);
    CHECK(committed.status.ok());
    const std::string snapshot_path =
        (std::filesystem::path(cancel_directory) / "snapshot.bin").string();
    const std::vector<std::uint8_t> before = readFile(snapshot_path);
    CHECK(!before.empty());

    shadowpipe::RenderConfig cancelled = config;
    auto token = std::make_shared<std::atomic_bool>(false);
    cancelled.cancel = token;
    cancelled.checkpoint_hook = [token](shadowpipe::Checkpoint checkpoint,
                                        std::uint32_t index) {
        if (checkpoint == shadowpipe::Checkpoint::AfterCascade && index == 0) {
            token->store(true);
        }
    };
    const shadowpipe::RenderResult cancelled_result =
        service.render(scene, cancelled, outputPaths(cancel_directory), nullptr);
    CHECK(cancelled_result.status.code == shadowpipe::ErrorCode::Cancelled);
    const std::vector<std::uint8_t> after = readFile(snapshot_path);
    CHECK(before == after);
}

void testBusyWithCheckpoint(const shadowpipe::Scene& scene,
                            const shadowpipe::RenderConfig& config,
                            const std::string& work) {
    shadowpipe::RenderService service;
    std::mutex mutex;
    std::condition_variable condition;
    bool entered = false;
    bool release = false;
    shadowpipe::RenderConfig blocking = config;
    blocking.checkpoint_hook = [&](shadowpipe::Checkpoint checkpoint,
                                   std::uint32_t index) {
        if (checkpoint == shadowpipe::Checkpoint::BeforePyramidLevel && index == 0) {
            std::unique_lock<std::mutex> lock(mutex);
            entered = true;
            condition.notify_all();
            condition.wait(lock, [&] { return release; });
        }
    };

    shadowpipe::RenderResult first_result;
    std::thread worker([&] {
        first_result = service.render(
            scene, blocking,
            outputPaths((std::filesystem::path(work) / "out_busy_a").string()),
            nullptr);
    });
    {
        std::unique_lock<std::mutex> lock(mutex);
        CHECK(condition.wait_for(lock, std::chrono::seconds(5),
                                 [&] { return entered; }));
    }
    const shadowpipe::RenderResult duplicate = service.render(
        scene, config,
        outputPaths((std::filesystem::path(work) / "out_busy_b").string()),
        nullptr);
    CHECK(duplicate.status.code == shadowpipe::ErrorCode::Busy);
    {
        std::lock_guard<std::mutex> lock(mutex);
        release = true;
    }
    condition.notify_all();
    worker.join();
    CHECK(first_result.status.ok());
}

void testChangedViewRerender(const shadowpipe::Scene& scene,
                             const shadowpipe::RenderConfig& config,
                             const std::string& work) {
    shadowpipe::RenderService service;
    const std::string directory =
        (std::filesystem::path(work) / "out_snapshot").string();
    const shadowpipe::RenderResult first =
        service.render(scene, config, outputPaths(directory), nullptr);
    CHECK(first.status.ok());
    const shadowpipe::SnapshotLoadResult loaded =
        shadowpipe::loadSnapshotFile(
            (std::filesystem::path(directory) / "snapshot.bin").string());
    CHECK(loaded.status.ok());
    if (!loaded.status.ok()) {
        return;
    }

    shadowpipe::RenderConfig moved = config;
    moved.camera.eye = shadowpipe::Vec3(2.0f, 6.5f, 16.0f);
    moved.camera.target = shadowpipe::Vec3(0.5f, 2.0f, 0.0f);
    moved.light.direction = shadowpipe::Vec3(-0.55f, -1.0f, -0.15f);
    const shadowpipe::RenderResult rerendered = service.render(
        scene, moved, outputPaths(directory), &loaded.snapshot);
    CHECK(rerendered.status.ok());
    CHECK(rerendered.summary.snapshot_loaded);
    CHECK(rerendered.summary.snapshot_diff.view_changed);
    CHECK(rerendered.summary.snapshot_diff.light_changed);
    CHECK(rerendered.summary.pyramid_rebuild.rebuilt);
    CHECK(rerendered.summary.pyramid_rebuild.level_deltas.size() ==
          rerendered.summary.pyramid_levels.size());
}

}  // namespace

int main() {
    const std::filesystem::path work =
        std::filesystem::absolute("shadowpipe_test_work");
    std::error_code ec;
    std::filesystem::remove_all(work, ec);
    std::filesystem::create_directories(work, ec);
    const std::string data_directory = (work / "data").string();
    CHECK(shadowpipe::generateAllSyntheticSamples(data_directory).ok());

    testSceneParsing(data_directory);

    const shadowpipe::SceneLoadResult scene_result =
        shadowpipe::loadSceneFromFile(
            (std::filesystem::path(data_directory) / "synthetic.rscene").string());
    CHECK(scene_result.status.ok());
    if (!scene_result.status.ok()) {
        std::cerr << failures << " test(s) failed\n";
        return 1;
    }
    const shadowpipe::RenderConfig config = testConfig();
    CHECK(shadowpipe::validateRenderConfig(config).ok());
    testCascadesAndPyramid(scene_result.scene, config);
    testDeterministicRender(scene_result.scene, config, work.string());
    testBiasAndCancellation(scene_result.scene, config, work.string());
    testBusyWithCheckpoint(scene_result.scene, config, work.string());
    testChangedViewRerender(scene_result.scene, config, work.string());

    if (failures == 0) {
        std::cout << "all shadowpipe tests passed\n";
        return 0;
    }
    std::cerr << failures << " test(s) failed\n";
    return 1;
}

