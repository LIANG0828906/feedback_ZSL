#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <iostream>
#include <limits>
#include <map>
#include <set>
#include <string>
#include <vector>

#ifndef _WIN32
#include <sys/wait.h>
#include <unistd.h>
#endif

#include "terrain/async_controller.h"
#include "terrain/hash.h"
#include "terrain/sample_generator.h"
#include "terrain/scene.h"
#include "terrain/terrain_io.h"
#include "terrain/version_store.h"

namespace {

int g_failures = 0;
int g_checks = 0;

void check(bool condition, const std::string& message) {
    ++g_checks;
    if (!condition) {
        ++g_failures;
        std::cerr << "FAIL: " << message << "\n";
    }
}

template <typename T>
void checkEqual(const T& actual, const T& expected, const std::string& message) {
    ++g_checks;
    if (!(actual == expected)) {
        ++g_failures;
        std::cerr << "FAIL: " << message << " actual=" << actual
                  << " expected=" << expected << "\n";
    }
}

void checkStatus(const terrain::Status& status, terrain::ErrorCode code,
                 const std::string& message,
                 const std::string& expected_field = {}) {
    ++g_checks;
    if (status.code != code) {
        ++g_failures;
        std::cerr << "FAIL: " << message
                  << " status=" << status.toString() << " expected="
                  << terrain::errorCodeName(code) << "\n";
    }
    if (!expected_field.empty() && status.field != expected_field) {
        ++g_failures;
        std::cerr << "FAIL: " << message
                  << " field=" << status.field
                  << " expected_field=" << expected_field << "\n";
    }
}

void setEnvValue(const char* key, const char* value) {
#ifdef _WIN32
    _putenv_s(key, value == nullptr ? "" : value);
#else
    if (value == nullptr) {
        unsetenv(key);
    } else {
        setenv(key, value, 1);
    }
#endif
}

std::string uniqueTempRoot() {
    const auto now = std::chrono::high_resolution_clock::now()
                         .time_since_epoch()
                         .count();
#ifdef _WIN32
    const std::string pid = "win";
#else
    const std::string pid = std::to_string(static_cast<long long>(getpid()));
#endif
    return (std::filesystem::temp_directory_path() /
            ("terrain_acceptance_" + pid + "_" + std::to_string(now)))
        .string();
}

void writeU32(std::vector<std::uint8_t>* bytes, std::size_t offset,
              std::uint32_t value) {
    (*bytes)[offset] = static_cast<std::uint8_t>(value & 0xffu);
    (*bytes)[offset + 1] = static_cast<std::uint8_t>((value >> 8u) & 0xffu);
    (*bytes)[offset + 2] = static_cast<std::uint8_t>((value >> 16u) & 0xffu);
    (*bytes)[offset + 3] = static_cast<std::uint8_t>((value >> 24u) & 0xffu);
}

bool sameHeight(const terrain::HeightField& a,
                const terrain::HeightField& b) {
    return a.width == b.width && a.height == b.height &&
           a.cell_spacing == b.cell_spacing &&
           a.baseline_version == b.baseline_version &&
           a.heights.size() == b.heights.size() &&
           std::memcmp(a.heights.data(), b.heights.data(),
                       a.heights.size() * sizeof(float)) == 0;
}

bool sameMaterial(const terrain::MaterialImage& a,
                  const terrain::MaterialImage& b) {
    return a.width == b.width && a.height == b.height &&
           a.baseline_version == b.baseline_version &&
           a.indices.size() == b.indices.size() &&
           std::memcmp(a.indices.data(), b.indices.data(),
                       a.indices.size() * sizeof(std::uint32_t)) == 0;
}

terrain::PatchBatch loadBatch(const std::string& height_path,
                              const std::string& material_path) {
    terrain::PatchBatch batch;
    if (!height_path.empty()) {
        terrain::HeightPatch patch;
        const terrain::Status status =
            terrain::loadHeightPatch(height_path, &patch);
        check(status.ok(), "load height patch " + height_path);
        batch.sequence = patch.sequence;
        batch.height = std::move(patch);
    }
    if (!material_path.empty()) {
        terrain::MaterialPatch patch;
        const terrain::Status status =
            terrain::loadMaterialPatch(material_path, &patch);
        check(status.ok(), "load material patch " + material_path);
        if (!batch.height) batch.sequence = patch.sequence;
        batch.material = std::move(patch);
    }
    batch.patch_hash = terrain::computeBatchHash(batch);
    return batch;
}

std::string findPatch(const terrain::SamplePaths& paths,
                      std::uint64_t sequence, bool material) {
    const std::vector<std::string>& list =
        material ? paths.material_patches : paths.height_patches;
    const std::string needle =
        material ? "_material.bin" : "_height.bin";
    const std::string prefix = [&]() {
        std::string s = std::to_string(sequence);
        while (s.size() < 4) s.insert(s.begin(), '0');
        return s;
    }();
    for (const std::string& path : list) {
        if (path.find(prefix + needle) != std::string::npos) return path;
    }
    return {};
}

void testFormatValidation(const terrain::SamplePaths& paths,
                          terrain::HeightField* base,
                          terrain::MaterialImage* material) {
    terrain::Status status = terrain::loadHeightField(paths.height_field, base);
    checkStatus(status, terrain::E_OK, "load valid height");
    status = terrain::loadMaterialImage(paths.material_image, material);
    checkStatus(status, terrain::E_OK, "load valid material");
    check(base->width == 129 && base->height == 129,
          "sample dimensions are deterministic");
    check(!base->payload_hash.empty(), "height payload hash is present");
    check(!material->payload_hash.empty(), "material payload hash is present");

    std::vector<std::uint8_t> bytes = terrain::encodeHeightField(*base);
    writeU32(&bytes, 12, 0);
    terrain::HeightField parsed;
    status = terrain::parseHeightField(bytes, "corrupt-width", &parsed);
    checkStatus(status, terrain::E_HEIGHT_INVALID, "width contradiction",
                "width");

    bytes = terrain::encodeHeightField(*base);
    writeU32(&bytes, 8, 9);
    status = terrain::parseHeightField(bytes, "corrupt-format", &parsed);
    checkStatus(status, terrain::E_HEIGHT_INVALID, "format contradiction",
                "format");

    terrain::HeightField nonfinite = *base;
    nonfinite.at(7, 9) = std::numeric_limits<float>::infinity();
    bytes = terrain::encodeHeightField(nonfinite);
    status = terrain::parseHeightField(bytes, "nonfinite", &parsed);
    checkStatus(status, terrain::E_HEIGHT_INVALID, "nonfinite elevation",
                "height[7,9]");

    terrain::HeightField range = *base;
    range.at(5, 6) = 1000.0f;
    bytes = terrain::encodeHeightField(range);
    status = terrain::parseHeightField(bytes, "range", &parsed);
    checkStatus(status, terrain::E_HEIGHT_INVALID, "range contradiction",
                "valid_range[5,6]");

    bytes = terrain::encodeHeightField(*base);
    bytes.resize(bytes.size() - 3);
    status = terrain::parseHeightField(bytes, "truncated", &parsed);
    checkStatus(status, terrain::E_HEIGHT_INVALID, "truncated payload",
                "payload_bytes");

    terrain::MaterialImage bad_material = *material;
    bad_material.at(11, 12) = 99;
    bytes = terrain::encodeMaterialImage(bad_material);
    terrain::MaterialImage parsed_material;
    status = terrain::parseMaterialImage(bytes, "bad-material",
                                         &parsed_material);
    checkStatus(status, terrain::E_HEIGHT_INVALID, "material palette index",
                "material.index[" +
                    std::to_string(static_cast<std::size_t>(12) *
                                       bad_material.width +
                                   11) +
                    "]");

    terrain::Quadtree tree;
    status = tree.build(*base, {});
    checkStatus(status, terrain::E_OK, "quadtree build");
    check(!tree.leaves().empty(), "quadtree has leaves");
    check(tree.node(tree.rootId()) != nullptr, "quadtree has root node");
    std::uint64_t covered = 0;
    for (std::uint64_t id : tree.leaves()) {
        const terrain::QuadNode* node = tree.node(id);
        check(node != nullptr, "leaf node exists");
        if (node == nullptr) continue;
        check(node->min_height <= node->max_height,
              "leaf min/max are ordered");
        check(node->content_hash.size() == 64, "leaf content hash is SHA-256");
        covered += static_cast<std::uint64_t>(node->rect.width) *
                   node->rect.height;
        for (std::uint64_t other : tree.leaves()) {
            if (other <= id) continue;
            const terrain::QuadNode* b = tree.node(other);
            check(!node->rect.intersects(b->rect),
                  "leaf rectangles do not overlap");
        }
    }
    const terrain::QuadNode* root = tree.node(tree.rootId());
    check(covered == static_cast<std::uint64_t>(root->rect.width) *
                          root->rect.height,
          "leaves exactly cover root");
}

void checkSurfaceTruth(const terrain::TerrainScene& scene,
                       const std::string& label) {
    terrain::SurfaceCache truth;
    truth.computeFull(scene.height(), scene.material());
    float max_angle = 0.0f;
    float max_material = 0.0f;
    for (std::size_t i = 0; i < truth.normals().size(); ++i) {
        const terrain::Vec3& a = scene.surface().normals()[i];
        const terrain::Vec3& b = truth.normals()[i];
        const terrain::Vec3 cp = terrain::cross(a, b);
        const float angle =
            std::atan2(std::sqrt(terrain::dot(cp, cp)),
                       std::max(-1.0f, std::min(1.0f, terrain::dot(a, b)))) *
            180.0f / 3.14159265358979323846f;
        max_angle = std::max(max_angle, angle);
        const terrain::Vec3& ma = scene.surface().material_colors()[i];
        const terrain::Vec3& mb = truth.material_colors()[i];
        max_material =
            std::max(max_material,
                     std::max(std::fabs(ma.x - mb.x),
                              std::max(std::fabs(ma.y - mb.y),
                                       std::fabs(ma.z - mb.z))));
    }
    check(max_angle <= scene.options().normal_tolerance_deg,
          label + ": normals match full truth");
    check(max_material <= scene.options().material_tolerance,
          label + ": material blend matches full truth");
    std::string detail;
    const terrain::Status render_status =
        scene.validateFullRender(nullptr, &detail);
    checkStatus(render_status, terrain::E_OK,
                label + ": independent full render matches");
}

void testPatchPipeline(const terrain::SamplePaths& paths) {
    terrain::TerrainScene scene;
    terrain::Status status =
        scene.initializeFromFiles(paths.height_field, paths.material_image);
    checkStatus(status, terrain::E_OK, "initialize scene");
    const std::size_t initial_leaves = scene.quadtree().leaves().size();
    const std::string initial_height_hash = scene.height().payload_hash;
    const std::string initial_material_hash = scene.material().payload_hash;

    terrain::PatchBatch out_of_bounds =
        loadBatch(paths.out_of_bounds_patch, {});
    terrain::SceneUpdateReport report;
    status = scene.applyBatch(out_of_bounds, nullptr, terrain::CommitFault::None,
                              {}, &report);
    checkStatus(status, terrain::E_PATCH_BOUNDS, "out-of-bounds patch rejected",
                "target");
    check(scene.height().payload_hash == initial_height_hash,
          "bounds failure does not mutate heights");
    check(scene.material().payload_hash == initial_material_hash,
          "bounds failure does not mutate materials");
    checkEqual(scene.version(), std::uint64_t{0},
               "bounds failure preserves version");

    terrain::PatchBatch stale = loadBatch(paths.stale_patch, {});
    status = scene.applyBatch(stale, nullptr, terrain::CommitFault::None, {},
                              &report);
    checkStatus(status, terrain::E_PATCH_STALE, "stale patch rejected",
                "baseline_version");
    check(scene.height().payload_hash == initial_height_hash,
          "stale failure does not mutate heights");
    checkEqual(scene.version(), std::uint64_t{0},
               "stale failure preserves version");

    terrain::PatchBatch out_of_range;
    out_of_range.sequence = 1;
    terrain::HeightPatch range_patch;
    range_patch.target = terrain::Rect{0, 0, 2, 2};
    range_patch.cell_spacing = scene.height().cell_spacing;
    range_patch.height_type = 0;
    range_patch.baseline_version = 0;
    range_patch.sequence = 1;
    range_patch.values.assign(4, 1000.0f);
    range_patch.payload_hash = terrain::toHex(terrain::hashFloats(
        range_patch.values));
    out_of_range.height = range_patch;
    out_of_range.patch_hash = terrain::computeBatchHash(out_of_range);
    status = scene.applyBatch(out_of_range, nullptr,
                              terrain::CommitFault::None, {}, &report);
    checkStatus(status, terrain::E_HEIGHT_INVALID,
                "out-of-range patch values rejected",
                "patch.valid_range[0]");
    check(scene.height().payload_hash == initial_height_hash,
          "invalid patch value does not mutate heights");

    std::vector<terrain::PatchBatch> batches;
    for (std::uint64_t sequence = 1; sequence <= 4; ++sequence) {
        terrain::PatchBatch batch =
            loadBatch(findPatch(paths, sequence, false),
                      findPatch(paths, sequence, true));
        checkEqual(batch.sequence, sequence, "batch sequence is parsed");
        batches.push_back(std::move(batch));
    }
    std::set<terrain::DependencyKind> observed_kinds;
    for (const terrain::PatchBatch& batch : batches) {
        status = scene.applyBatch(batch, nullptr, terrain::CommitFault::None,
                                  {}, &report);
        checkStatus(status, terrain::E_OK,
                    "apply patch " + std::to_string(batch.sequence));
        checkEqual(scene.version(), batch.sequence,
                   "patch advances committed version");
        check(report.seams.clean(), "mesh seams are closed");
        check(report.seams.max_deviation <= scene.options().seam_tolerance,
              "seam deviation is inside tolerance");
        check(!report.seams.seams.empty(), "seam edges are enumerated");
        check(report.surface.max_normal_angle_deg <=
                  scene.options().normal_tolerance_deg,
              "reported incremental normal error is inside tolerance");
        check(report.surface.max_material_delta <=
                  scene.options().material_tolerance,
              "reported material blend error is inside tolerance");
        check(report.full_incremental_equal,
              "incremental frame equals full frame");
        check(report.repeated_incremental_equal,
              "consecutive incremental frames are deterministic");
        check(report.full_render_hash == report.incremental_render_hash,
              "full and incremental color hashes match");
        check(report.repeated_render_hash == report.incremental_render_hash,
              "repeated incremental hash matches");
        check(!report.invalidation.fallback_full,
              "invalidation does not fall back to full rerender");
        check(!report.invalidation.invalid_blocks.empty(),
              "patch invalidates dependency nodes");
        check(report.invalidation.invalid_blocks.size() <
                  scene.quadtree().leaves().size(),
              "localized patch does not invalidate every block");
        check(report.recomputed_blocks.size() <
                  scene.quadtree().leaves().size(),
              "localized patch does not recompute every block");
        check(!report.reused_blocks.empty(),
              "unaffected block buffers are reused");
        check(report.render.rerendered_blocks <
                  scene.quadtree().leaves().size(),
              "offscreen renderer rerenders only invalid blocks");
        for (std::uint64_t block : report.invalidation.invalid_blocks) {
            check(!report.invalidation.reasonFor(block).empty(),
                  "every invalid block has a recorded reason");
        }
        for (const terrain::DependencyNode& node :
             report.invalidation.invalid_nodes) {
            observed_kinds.insert(node.kind);
        }
        checkSurfaceTruth(scene, "patch " + std::to_string(batch.sequence));
    }
    check(observed_kinds.count(terrain::DependencyKind::Height) == 1,
          "dependency graph invalidates height nodes");
    check(observed_kinds.count(terrain::DependencyKind::Material) == 1,
          "dependency graph invalidates material nodes");
    check(observed_kinds.count(terrain::DependencyKind::LodDecision) == 1,
          "dependency graph invalidates LOD nodes");
    check(observed_kinds.count(terrain::DependencyKind::Normal) == 1,
          "dependency graph invalidates normal nodes");
    check(observed_kinds.count(terrain::DependencyKind::MaterialBlend) == 1,
          "dependency graph invalidates material blend nodes");
    check(observed_kinds.count(terrain::DependencyKind::Seam) == 1,
          "dependency graph invalidates seam nodes");
    check(observed_kinds.count(terrain::DependencyKind::Offscreen) == 1,
          "dependency graph invalidates offscreen nodes");
    check(scene.quadtree().leaves().size() <= initial_leaves + 32,
          "incremental quadtree growth is bounded");

    terrain::SeamReport seams = scene.verifySeams();
    check(seams.clean(), "final seam verification is clean");
    check(!seams.seams.empty(), "final seams exist");
    {
        const terrain::OffscreenFrame& frame = scene.renderer().frame();
        std::size_t uncovered = 0;
        for (std::size_t i = 0; i < frame.heat.size(); ++i) {
            if (frame.color[i * 4u] == 16u &&
                frame.color[i * 4u + 1u] == 20u &&
                frame.color[i * 4u + 2u] == 26u) {
                ++uncovered;
            }
        }
        checkEqual(uncovered, std::size_t{0},
                   "offscreen frame has no uncovered block pixels");
    }
    if (!seams.seams.empty()) {
        const terrain::SeamCheck& check_seam = seams.seams.front();
        std::map<std::uint64_t, terrain::BlockMesh> damaged = scene.meshes();
        const terrain::QuadNode* a = scene.quadtree().node(check_seam.block_a);
        const terrain::QuadNode* b = scene.quadtree().node(check_seam.block_b);
        check(a != nullptr && b != nullptr, "seam block nodes resolve");
        if (a != nullptr && b != nullptr) {
            std::uint32_t seam_x = 0;
            std::uint32_t seam_y = 0;
            if (check_seam.vertical) {
                seam_x = a->rect.right() == b->rect.x ? a->rect.right()
                                                      : b->rect.right();
                seam_y = check_seam.overlap_begin;
            } else {
                seam_x = check_seam.overlap_begin;
                seam_y = a->rect.bottom() == b->rect.y ? a->rect.bottom()
                                                       : b->rect.bottom();
            }
            bool damaged_vertex = false;
            for (terrain::MeshVertex& vertex :
                 damaged[check_seam.block_a].vertices) {
                if (vertex.grid_x == seam_x && vertex.grid_y == seam_y) {
                    vertex.position.y += 1.0f;
                    damaged_vertex = true;
                    break;
                }
            }
            check(damaged_vertex, "seam vertex is shared by both block meshes");
            const terrain::SeamReport crack = terrain::verifyMeshSeams(
                scene.quadtree(), damaged, scene.options().seam_tolerance);
            checkStatus(crack.status, terrain::E_CRACK_UNRESOLVED,
                        "injected crack is located");
            check(crack.status.block_a == check_seam.block_a &&
                      crack.status.block_b == check_seam.block_b,
                  "crack status names both adjacent blocks");
        }
    }
}

void testDeterministicTail(const terrain::SamplePaths& paths) {
    terrain::TerrainScene first;
    terrain::TerrainScene second;
    checkStatus(first.initializeFromFiles(paths.height_field,
                                          paths.material_image),
                terrain::E_OK, "deterministic scene A");
    checkStatus(second.initializeFromFiles(paths.height_field,
                                           paths.material_image),
                terrain::E_OK, "deterministic scene B");
    terrain::PatchBatch batch =
        loadBatch(findPatch(paths, 1, false), findPatch(paths, 1, true));
    terrain::SceneUpdateReport report_a;
    terrain::SceneUpdateReport report_b;
    checkStatus(first.applyBatch(batch, nullptr, terrain::CommitFault::None, {},
                                 &report_a),
                terrain::E_OK, "deterministic patch A");
    checkStatus(second.applyBatch(batch, nullptr, terrain::CommitFault::None, {},
                                  &report_b),
                terrain::E_OK, "deterministic patch B");
    checkEqual(report_a.render.color_hash, report_b.render.color_hash,
               "deterministic color hash");
    checkEqual(report_a.render.heat_hash, report_b.render.heat_hash,
               "deterministic heat hash");
    check(report_a.invalidation.invalid_blocks ==
              report_b.invalidation.invalid_blocks,
          "deterministic invalidation set");
    check(report_a.recomputed_blocks == report_b.recomputed_blocks,
          "deterministic recompute list");
}

void testCommitAndRecovery(const terrain::SamplePaths& paths,
                           const std::string& root) {
    const std::string store_root = (std::filesystem::path(root) /
                                    "version_store").string();
    terrain::VersionStore store(store_root);
    terrain::Status status =
        store.initializeFromFiles(paths.height_field, paths.material_image);
    checkStatus(status, terrain::E_OK, "initialize version store");
    status = store.verifyChain();
    checkStatus(status, terrain::E_OK, "verify initial manifest chain");
    terrain::AsyncTerrainController controller;
    terrain::SceneOptions options;
    status = controller.initializeFromStore(&store, options);
    checkStatus(status, terrain::E_OK, "scene loads committed snapshot");

    terrain::PatchBatch batch1 =
        loadBatch(findPatch(paths, 1, false), findPatch(paths, 1, true));
    terrain::SceneUpdateReport report;
    status = controller.scene().applyBatch(
        batch1, &store, terrain::CommitFault::None, {}, &report);
    checkStatus(status, terrain::E_OK, "commit patch 1");
    checkEqual(store.currentVersion(), std::uint64_t{1},
               "store advances to version 1");
    checkStatus(store.verifyChain(), terrain::E_OK,
                "chain verifies after patch 1");

    terrain::VersionManifest manifest_before;
    checkStatus(store.loadManifest(&manifest_before), terrain::E_OK,
                "load manifest before failed commit");
    const std::string old_manifest_hash = manifest_before.manifest_hash;
    const std::uint64_t old_version = store.currentVersion();
    terrain::PatchBatch batch2 =
        loadBatch(findPatch(paths, 2, false), findPatch(paths, 2, true));
    status = controller.scene().applyBatch(
        batch2, &store, terrain::CommitFault::BeforeManifest, {}, &report);
    checkStatus(status, terrain::E_COMMIT_FAILED,
                "injected commit failure reported");
    checkEqual(store.currentVersion(), old_version,
               "failed commit does not advance store");
    checkEqual(controller.scene().version(), old_version,
               "failed commit preserves scene version");
    terrain::VersionManifest manifest_after;
    checkStatus(store.loadManifest(&manifest_after), terrain::E_OK,
                "load manifest after failed commit");
    checkEqual(manifest_after.manifest_hash, old_manifest_hash,
               "failed commit preserves current manifest");
    checkStatus(store.verifyChain(), terrain::E_OK,
                "chain still verifies after failed commit");

    status = controller.scene().applyBatch(
        batch2, &store, terrain::CommitFault::None, {}, &report);
    checkStatus(status, terrain::E_OK, "retry patch 2 commits");
    checkEqual(store.currentVersion(), std::uint64_t{2},
               "store advances to version 2");

#ifndef _WIN32
    const pid_t pid = fork();
    if (pid == 0) {
        setEnvValue("TERRAIN_FORCE_KILL", "1");
        terrain::VersionStore child_store(store_root);
        terrain::TerrainScene child_scene;
        const terrain::Status loaded =
            child_scene.loadCommitted(&child_store, options);
        if (!loaded.ok()) _exit(80);
        terrain::PatchBatch child_batch =
            loadBatch(findPatch(paths, 3, false), findPatch(paths, 3, true));
        terrain::SceneUpdateReport child_report;
        child_scene.applyBatch(child_batch, &child_store,
                               terrain::CommitFault::BeforeManifest,
                               []() { return false; }, &child_report);
        _exit(81);
    } else {
        int child_status = 0;
        waitpid(pid, &child_status, 0);
        check(WIFEXITED(child_status) && WEXITSTATUS(child_status) == 77,
              "child process is force-killed during commit");
        terrain::VersionStore restarted(store_root);
        terrain::VersionManifest recovered_manifest;
        checkStatus(restarted.loadCurrent(nullptr, nullptr, &recovered_manifest),
                    terrain::E_OK, "restart loads last committed manifest");
        checkEqual(recovered_manifest.version, std::uint64_t{2},
                   "force-kill leaves prior committed version");
        terrain::AsyncTerrainController recovery_controller;
        terrain::RecoveryReport recovery;
        status = recovery_controller.recoverPending(
            &restarted, paths.patch_directory, &recovery);
        checkStatus(status, terrain::E_OK, "recovery replays pending patches");
        checkEqual(recovery.started_at_version, std::uint64_t{2},
                   "recovery starts from committed version");
        checkEqual(recovery.finished_at_version, std::uint64_t{4},
                   "recovery reaches final patch");
        checkEqual(recovery.skipped_committed_patches, std::uint32_t{2},
                   "recovery skips already committed patches");
        checkEqual(recovery.committed_batches, std::uint32_t{2},
                   "recovery commits only uncommitted patches");
        checkStatus(restarted.verifyChain(), terrain::E_OK,
                    "recovered manifest chain verifies");
    }
#else
    std::cout << "NOTE: hard-kill recovery fork test skipped on Windows\n";
#endif
}

void testAsyncBusyCancel(const terrain::SamplePaths& paths) {
    terrain::AsyncTerrainController controller;
    terrain::SceneOptions options;
    terrain::Status status = controller.initializeFromFiles(
        paths.height_field, paths.material_image, options);
    checkStatus(status, terrain::E_OK, "async controller initializes");
    terrain::PatchBatch batch =
        loadBatch(findPatch(paths, 1, false), findPatch(paths, 1, true));
    setEnvValue("TERRAIN_TEST_JOB_DELAY_MS", "700");
    status = controller.submit(batch, nullptr);
    checkStatus(status, terrain::E_OK, "first async submit accepted");
    status = controller.submit(batch, nullptr);
    checkStatus(status, terrain::E_BUSY, "duplicate submit returns E_BUSY",
                "job");
    status = controller.cancel();
    checkStatus(status, terrain::E_OK, "cooperative cancel accepted");
    terrain::JobResult result;
    check(controller.waitForCompletion(10000, &result),
          "cancelled worker releases handle");
    checkStatus(result.status, terrain::E_CANCELLED, "cancel result");
    checkEqual(controller.scene().version(), std::uint64_t{0},
               "cancel preserves last committed version");
    check(!controller.busy(), "cancel releases busy state");
    setEnvValue("TERRAIN_TEST_JOB_DELAY_MS", nullptr);
    status = controller.submit(batch, nullptr);
    checkStatus(status, terrain::E_OK, "submit after cancel accepted");
    result = {};
    check(controller.waitForCompletion(10000, &result),
          "post-cancel worker completes");
    checkStatus(result.status, terrain::E_OK, "post-cancel patch commits");
    checkEqual(controller.scene().version(), std::uint64_t{1},
               "post-cancel version advances");
    terrain::SceneOptions changed = controller.scene().options();
    changed.quadtree.block_size = 16;
    status = controller.submitOptions(changed);
    checkStatus(status, terrain::E_OK, "background option rebuild accepted");
    result = {};
    check(controller.waitForCompletion(10000, &result),
          "background option rebuild completes");
    checkStatus(result.status, terrain::E_OK, "background option rebuild");
    checkEqual(controller.scene().options().quadtree.block_size,
               std::uint32_t{16}, "block size setting is applied");
    checkStatus(controller.scene().validateFullRender(nullptr, nullptr),
                terrain::E_OK, "rebuild full render is valid");
    const std::size_t refined_leaves =
        controller.scene().quadtree().leaves().size();
    changed.quadtree.error_threshold = 50.0f;
    status = controller.submitOptions(changed);
    checkStatus(status, terrain::E_OK, "threshold rebuild accepted");
    result = {};
    check(controller.waitForCompletion(10000, &result),
          "threshold rebuild completes");
    checkStatus(result.status, terrain::E_OK, "threshold rebuild");
    check(controller.scene().quadtree().leaves().size() <= refined_leaves,
          "larger error threshold coalesces leaves");
}

}  // namespace

int main() {
    const std::string root = uniqueTempRoot();
    std::filesystem::create_directories(root);
    terrain::SamplePaths paths;
    terrain::Status status =
        terrain::generateDeterministicSamples(root + "/samples", &paths);
    checkStatus(status, terrain::E_OK, "generate deterministic samples");

    terrain::HeightField base;
    terrain::MaterialImage material;
    testFormatValidation(paths, &base, &material);
    testPatchPipeline(paths);
    testDeterministicTail(paths);
    testCommitAndRecovery(paths, root);
    testAsyncBusyCancel(paths);
    checkStatus(terrain::VersionStore(root + "/missing").verifyChain(),
                terrain::E_RECOVERY_FAILED,
                "missing manifest produces recovery failure");

    std::error_code ec;
    std::filesystem::remove_all(root, ec);
    std::cout << "checks=" << g_checks << " failures=" << g_failures << "\n";
    if (g_failures != 0) {
        std::cerr << "TERRAIN ACCEPTANCE FAILED\n";
        return 1;
    }
    std::cout << "TERRAIN ACCEPTANCE PASSED\n";
    return 0;
}

