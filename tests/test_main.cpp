#include "battle/injection.h"
#include "battle/bits.h"
#include "battle/sample.h"
#include "battle/sha256.h"

#include <algorithm>
#include <atomic>
#include <cstdint>
#include <cmath>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <functional>
#include <iostream>
#include <iterator>
#include <map>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <system_error>
#include <vector>

namespace {

namespace fs = std::filesystem;

int failures = 0;
std::string current_test;

#define CHECK(condition)                                                      \
    do {                                                                      \
        if (!(condition)) {                                                   \
            ++failures;                                                       \
            std::cerr << "FAIL [" << current_test << "] " << __FILE__ << ":" \
                      << __LINE__ << "  " #condition "\n";                    \
        }                                                                     \
    } while (false)

bool write_bytes(const fs::path& path, const std::string& bytes) {
    std::error_code error;
    if (path.has_parent_path()) {
        fs::create_directories(path.parent_path(), error);
    }
    std::ofstream stream(path, std::ios::binary | std::ios::trunc);
    if (!stream) {
        return false;
    }
    stream.write(bytes.data(), static_cast<std::streamsize>(bytes.size()));
    return static_cast<bool>(stream);
}

std::string read_bytes(const fs::path& path) {
    std::ifstream stream(path, std::ios::binary);
    return std::string((std::istreambuf_iterator<char>(stream)),
                       std::istreambuf_iterator<char>());
}

void set_u32_le(std::string& bytes, std::size_t offset, std::uint32_t value) {
    for (unsigned i = 0; i < 4; ++i) {
        bytes[offset + i] = static_cast<char>((value >> (i * 8U)) & 0xffU);
    }
}

battle::Scene basic_scene() {
    battle::Scene scene;
    scene.grid.width = 20;
    scene.grid.levels = 2;
    scene.grid.depth = 20;
    scene.grid.cell_size = 1.0F;
    scene.grid.origin = {0.0F, 0.0F, 0.0F};
    scene.grid.walkable.assign(
        static_cast<std::size_t>(scene.grid.cell_count()), 1U);

    battle::Unit box_unit;
    box_unit.id = 1;
    box_unit.shape = battle::UnitShape::Aabb;
    box_unit.aabb.min = {1.0F, 0.0F, 4.0F};
    box_unit.aabb.max = {3.0F, 2.0F, 6.0F};
    scene.units.push_back(box_unit);

    battle::Unit capsule_unit;
    capsule_unit.id = 2;
    capsule_unit.shape = battle::UnitShape::Capsule;
    capsule_unit.capsule.a = {7.0F, 0.1F, 5.0F};
    capsule_unit.capsule.b = {7.0F, 2.0F, 5.0F};
    capsule_unit.capsule.radius = 0.5F;
    scene.units.push_back(capsule_unit);

    battle::Cover cover;
    cover.id = 100;
    cover.present = true;
    cover.triangle.a = {12.0F, 0.5F, 8.0F};
    cover.triangle.b = {14.0F, 0.5F, 8.0F};
    cover.triangle.c = {13.0F, 1.5F, 8.0F};
    scene.covers.push_back(cover);
    return scene;
}

battle::Camera unit_camera() {
    return battle::make_look_at_camera(
        {2.0F, 6.0F, 12.0F}, {2.0F, 1.0F, 5.0F},
        0.785398163F, 16.0F / 9.0F, 1024.0F, 768.0F);
}

std::string pick_fingerprint(const battle::PickResult& result) {
    std::string output;
    output += std::to_string(result.region_version);
    output.push_back('|');
    output += result.region_hash;
    output.push_back('|');
    output += result.has_hit
                  ? battle::entity_ref_string(result.hit.entity)
                  : "none";
    if (result.has_hit) {
        const float values[7] = {
            result.hit.point.x, result.hit.point.y, result.hit.point.z,
            result.hit.normal.x, result.hit.normal.y, result.hit.normal.z,
            result.hit.distance};
        for (float value : values) {
            output.push_back('|');
            output += battle::hex32(battle::float_bits(value));
        }
    }
    return output;
}

bool make_store(const battle::Scene& scene,
                std::shared_ptr<battle::RegionStore>& store,
                battle::RegionState* state_out = nullptr) {
    battle::RegionState state = battle::RegionState::from_scene(scene);
    if (state_out != nullptr) {
        *state_out = state;
    }
    battle::Outcome<std::shared_ptr<battle::RegionStore>> created =
        battle::RegionStore::create(state);
    if (!created) {
        CHECK(false);
        return false;
    }
    store = created.value();
    return true;
}

bool test_scene_validation(const fs::path& work) {
    current_test = "scene_validation";
    const battle::Scene scene = basic_scene();
    CHECK(scene.validate().ok());
    const fs::path path = work / "scene" / "valid.bin";
    CHECK(write_bytes(path, scene.encode_binary()));
    battle::Outcome<battle::Scene> loaded = battle::Scene::load_binary(path.string());
    CHECK(loaded.ok());
    if (loaded) {
        CHECK(loaded.value().units.size() == 2);
        CHECK(loaded.value().covers.size() == 1);
        CHECK(loaded.value().encode_binary() == scene.encode_binary());
    }

    battle::Scene degenerate_box = scene;
    degenerate_box.units[0].aabb.max = degenerate_box.units[0].aabb.min;
    battle::Outcome<void> invalid_box = degenerate_box.validate();
    CHECK(!invalid_box.ok());
    CHECK(invalid_box.error().code == battle::StatusCode::SceneInvalid);
    CHECK(invalid_box.error().entity.kind == battle::EntityKind::Unit);
    CHECK(invalid_box.error().entity.id == 1);

    battle::Scene degenerate_capsule = scene;
    degenerate_capsule.units[1].capsule.radius = 0.0F;
    battle::Outcome<void> invalid_capsule = degenerate_capsule.validate();
    CHECK(!invalid_capsule.ok());
    CHECK(invalid_capsule.error().code == battle::StatusCode::SceneInvalid);
    CHECK(invalid_capsule.error().entity.id == 2);

    battle::Scene bad_marker = scene;
    bad_marker.grid.walkable[3] = 7;
    battle::Outcome<void> invalid_marker = bad_marker.validate();
    CHECK(!invalid_marker.ok());
    CHECK(invalid_marker.error().code == battle::StatusCode::SceneInvalid);
    CHECK(invalid_marker.error().injection_point == "scene.grid.walkable.marker");

    std::string missing_marker = scene.encode_binary();
    set_u32_le(missing_marker, 44, 0);
    const fs::path missing_path = work / "scene" / "missing-markers.bin";
    CHECK(write_bytes(missing_path, missing_marker));
    battle::Outcome<battle::Scene> missing =
        battle::Scene::load_binary(missing_path.string());
    CHECK(!missing.ok());
    CHECK(missing.error().code == battle::StatusCode::SceneInvalid);
    CHECK(missing.error().injection_point == "scene.grid.walkable.count");

    std::string bad_shape = scene.encode_binary();
    const std::size_t unit_shape_offset =
        48 + static_cast<std::size_t>(scene.grid.cell_count()) + 4 + 4 + 4;
    bad_shape[unit_shape_offset] = static_cast<char>(9);
    const fs::path shape_path = work / "scene" / "bad-shape.bin";
    CHECK(write_bytes(shape_path, bad_shape));
    battle::Outcome<battle::Scene> shape =
        battle::Scene::load_binary(shape_path.string());
    CHECK(!shape.ok());
    CHECK(shape.error().code == battle::StatusCode::SceneInvalid);
    CHECK(shape.error().entity.kind == battle::EntityKind::Unit);

    std::string bad_index = scene.encode_binary();
    const std::size_t unit_index_offset =
        48 + static_cast<std::size_t>(scene.grid.cell_count()) + 4 + 4;
    set_u32_le(bad_index, unit_index_offset, 99);
    const fs::path index_path = work / "scene" / "bad-index.bin";
    CHECK(write_bytes(index_path, bad_index));
    battle::Outcome<battle::Scene> index =
        battle::Scene::load_binary(index_path.string());
    CHECK(!index.ok());
    CHECK(index.error().code == battle::StatusCode::SceneInvalid);
    CHECK(index.error().injection_point == "scene.unit.index");
    CHECK(index.error().entity.kind == battle::EntityKind::Unit);
    return true;
}

bool test_ray_and_selection(const fs::path&) {
    current_test = "ray_and_selection";
    std::shared_ptr<battle::RegionStore> store;
    if (!make_store(basic_scene(), store)) {
        return false;
    }
    battle::Outcome<battle::SnapshotHandle> acquired = store->acquire();
    CHECK(acquired.ok());
    if (!acquired) {
        return false;
    }
    battle::PickRequest request;
    request.camera = unit_camera();
    request.screen = {request.camera.viewport_width * 0.5F,
                      request.camera.viewport_height * 0.5F};
    battle::Outcome<battle::PickResult> first =
        battle::pick_screen(*store, acquired.value(), request);
    CHECK(first.ok());
    CHECK(first.value().has_hit);
    CHECK(first.value().hit.entity.kind == battle::EntityKind::Unit);
    CHECK(first.value().hit.entity.id == 1);
    const std::string first_fingerprint = pick_fingerprint(first.value());
    for (int i = 0; i < 6; ++i) {
        battle::Outcome<battle::PickResult> repeated =
            battle::pick_screen(*store, acquired.value(), request);
        CHECK(repeated.ok());
        CHECK(pick_fingerprint(repeated.value()) == first_fingerprint);
        CHECK(battle::float_bits(repeated.value().ray.direction.x) ==
              battle::float_bits(first.value().ray.direction.x));
    }

    battle::PickRequest ground_request;
    ground_request.camera = battle::make_look_at_camera(
        {10.0F, 9.0F, 12.0F}, {10.0F, 0.0F, 10.0F},
        0.75F, 16.0F / 9.0F, 800.0F, 600.0F);
    ground_request.screen = {400.0F, 300.0F};
    battle::Outcome<battle::PickResult> ground =
        battle::pick_screen(*store, acquired.value(), ground_request);
    CHECK(ground.ok());
    CHECK(ground.value().has_hit);
    CHECK(ground.value().hit.entity.kind == battle::EntityKind::Ground);

    battle::PickRequest cover_request;
    cover_request.camera = battle::make_look_at_camera(
        {13.0F, 6.0F, 18.0F}, {13.0F, 0.8333333F, 8.0F},
        0.75F, 16.0F / 9.0F, 800.0F, 600.0F);
    cover_request.screen = {400.0F, 300.0F};
    battle::Outcome<battle::PickResult> cover =
        battle::pick_screen(*store, acquired.value(), cover_request);
    CHECK(cover.ok());
    CHECK(cover.value().has_hit);
    CHECK(cover.value().hit.entity.kind == battle::EntityKind::Cover);

    battle::Outcome<battle::PickResult> capsule = battle::pick_ray(
        *store, acquired.value(),
        battle::Ray{{7.0F, 1.0F, 12.0F}, {0.0F, 0.0F, -1.0F}});
    CHECK(capsule.ok());
    CHECK(capsule.value().has_hit);
    CHECK(capsule.value().hit.entity.kind == battle::EntityKind::Unit);
    CHECK(capsule.value().hit.entity.id == 2);

    battle::BoxSelectRequest box_request;
    box_request.camera = battle::make_look_at_camera(
        {10.0F, 18.0F, 24.0F}, {10.0F, 0.0F, 10.0F},
        0.95F, 16.0F / 9.0F, 1024.0F, 768.0F);
    box_request.rect = {0.0F, 0.0F, 1024.0F, 768.0F};
    battle::Outcome<battle::BoxSelectResult> selected =
        battle::box_select(*store, acquired.value(), box_request);
    CHECK(selected.ok());
    CHECK(std::find(selected.value().unit_ids.begin(),
                    selected.value().unit_ids.end(), 1) !=
          selected.value().unit_ids.end());
    CHECK(std::find(selected.value().unit_ids.begin(),
                    selected.value().unit_ids.end(), 2) !=
          selected.value().unit_ids.end());

    battle::PickRequest degenerate = request;
    degenerate.camera.up = degenerate.camera.forward;
    battle::Outcome<battle::PickResult> bad_camera =
        battle::pick_screen(*store, acquired.value(), degenerate);
    CHECK(!bad_camera.ok());
    CHECK(bad_camera.error().code == battle::StatusCode::RayInvalid);

    battle::PickRequest outside = request;
    outside.screen = {-1.0F, 3.0F};
    battle::Outcome<battle::PickResult> bad_screen =
        battle::pick_screen(*store, acquired.value(), outside);
    CHECK(!bad_screen.ok());
    CHECK(bad_screen.error().code == battle::StatusCode::RayInvalid);

    battle::Outcome<battle::PickResult> zero_ray = battle::pick_ray(
        *store, acquired.value(),
        battle::Ray{{0.0F, 0.0F, 0.0F}, {0.0F, 0.0F, 0.0F}});
    CHECK(!zero_ray.ok());
    CHECK(zero_ray.error().code == battle::StatusCode::RayInvalid);
    CHECK(store->release(acquired.value()).ok());
    return true;
}

bool test_versions_and_concurrency(const fs::path&) {
    current_test = "versions_and_concurrency";
    std::shared_ptr<battle::RegionStore> store;
    battle::RegionState initial;
    if (!make_store(basic_scene(), store, &initial)) {
        return false;
    }
    battle::Outcome<battle::SnapshotHandle> old_handle = store->acquire();
    CHECK(old_handle.ok());
    if (!old_handle) {
        return false;
    }
    battle::PickRequest request;
    request.camera = unit_camera();
    request.screen = {request.camera.viewport_width * 0.5F,
                      request.camera.viewport_height * 0.5F};
    battle::Outcome<battle::PickResult> old_pick =
        battle::pick_screen(*store, old_handle.value(), request);
    CHECK(old_pick.ok());
    CHECK(old_pick.value().hit.entity.id == 1);

    battle::UnitMove move;
    move.id = 1;
    move.delta = {8.0F, 0.0F, 0.0F};
    battle::Outcome<battle::RegionState> next =
        battle::apply_region_mutation(initial, move);
    CHECK(next.ok());
    if (!next) {
        return false;
    }
    CHECK(next.value().version == 2);
    CHECK(next.value().content_hash != initial.content_hash);
    battle::Outcome<battle::PublishedRegionPtr> published =
        store->publish(next.value());
    CHECK(published.ok());

    battle::Outcome<battle::PickResult> old_again =
        battle::pick_screen(*store, old_handle.value(), request);
    CHECK(old_again.ok());
    CHECK(old_again.value().region_version == 1);
    CHECK(old_again.value().hit.entity.id == 1);

    battle::Outcome<battle::SnapshotHandle> new_handle = store->acquire();
    CHECK(new_handle.ok());
    battle::Outcome<battle::PickResult> new_pick =
        battle::pick_screen(*store, new_handle.value(), request);
    CHECK(new_pick.ok());
    CHECK(new_pick.value().region_version == 2);

    battle::PickRequest stale_version = request;
    stale_version.expected_version = 1;
    battle::Outcome<battle::PickResult> stale =
        battle::pick_screen(*store, new_handle.value(), stale_version);
    CHECK(!stale.ok());
    CHECK(stale.error().code == battle::StatusCode::RegionStale);
    battle::PickRequest stale_hash = request;
    stale_hash.expected_hash = initial.content_hash;
    battle::Outcome<battle::PickResult> hash_mismatch =
        battle::pick_screen(*store, new_handle.value(), stale_hash);
    CHECK(!hash_mismatch.ok());
    CHECK(hash_mismatch.error().code == battle::StatusCode::RegionStale);

    battle::RegionState bad_hash = next.value();
    bad_hash.version = 3;
    bad_hash.content_hash = "deadbeef";
    battle::Outcome<battle::PublishedRegionPtr> bad_publish =
        store->publish(bad_hash);
    CHECK(!bad_publish.ok());
    CHECK(bad_publish.error().code == battle::StatusCode::RegionStale);

    CHECK(store->release(old_handle.value()).ok());
    battle::Outcome<battle::PickResult> released =
        battle::pick_screen(*store, old_handle.value(), request);
    CHECK(!released.ok());
    CHECK(released.error().code == battle::StatusCode::SnapshotStale);

    battle::Outcome<battle::SnapshotHandle> archived = store->acquire_version(1);
    CHECK(archived.ok());
    if (archived) {
        battle::Outcome<battle::PickResult> archive_pick =
            battle::pick_screen(*store, archived.value(), request);
        CHECK(archive_pick.ok());
        CHECK(archive_pick.value().hit.entity.id == 1);
        CHECK(store->release(archived.value()).ok());
    }

    CHECK(new_handle.ok());
    battle::InjectionSpec corrupt;
    corrupt.kind = battle::InjectionKind::CorruptSnapshot;
    battle::Outcome<battle::PickResult> corruption =
        battle::InjectionGate::pick_screen(
            *store, new_handle.value(), request, corrupt);
    CHECK(!corruption.ok());
    CHECK(corruption.error().code == battle::StatusCode::SnapshotStale);
    CHECK(corruption.error().injection_point.find("injection.corrupt_snapshot") ==
          0);

    std::shared_ptr<battle::RegionStore> concurrent_store;
    if (!make_store(basic_scene(), concurrent_store)) {
        return false;
    }
    std::mutex results_mutex;
    std::map<std::uint64_t, std::pair<std::string, std::string>> observed;
    std::atomic<bool> go{false};
    std::atomic<bool> writer_done{false};
    std::thread writer([&]() {
        while (!go.load()) {
        }
        for (int i = 0; i < 160; ++i) {
            battle::RegionState current = concurrent_store->current_state_copy();
            battle::UnitMove step;
            step.id = 1;
            step.delta = {0.01F, 0.0F, 0.0F};
            battle::Outcome<battle::RegionState> mutated =
                battle::apply_region_mutation(current, step);
            if (!mutated ||
                !concurrent_store->publish(mutated.value()).ok()) {
                CHECK(false);
                break;
            }
        }
        writer_done.store(true);
    });
    std::vector<std::thread> readers;
    for (int reader = 0; reader < 4; ++reader) {
        readers.emplace_back([&]() {
            while (!go.load()) {
            }
            int iterations = 0;
            do {
                battle::Outcome<battle::SnapshotHandle> handle =
                    concurrent_store->acquire();
                if (!handle) {
                    CHECK(false);
                    break;
                }
                battle::Outcome<battle::PickResult> picked =
                    battle::pick_screen(*concurrent_store, handle.value(), request);
                if (picked) {
                    CHECK(picked.value().region_version == handle.value().version);
                    CHECK(picked.value().region_hash == handle.value().content_hash);
                    std::lock_guard<std::mutex> lock(results_mutex);
                    observed.emplace(
                        handle.value().version,
                        std::make_pair(handle.value().content_hash,
                                       pick_fingerprint(picked.value())));
                } else {
                    CHECK(false);
                }
                CHECK(concurrent_store->release(handle.value()).ok());
                ++iterations;
            } while (!writer_done.load() || iterations < 24);
        });
    }
    go.store(true);
    writer.join();
    for (std::thread& reader : readers) {
        reader.join();
    }
    CHECK(!observed.empty());
    for (const auto& entry : observed) {
        battle::Outcome<battle::SnapshotHandle> handle =
            concurrent_store->acquire_version(entry.first, entry.second.first);
        CHECK(handle.ok());
        if (!handle) {
            continue;
        }
        battle::Outcome<battle::PickResult> replay =
            battle::pick_screen(*concurrent_store, handle.value(), request);
        CHECK(replay.ok());
        if (replay) {
            CHECK(pick_fingerprint(replay.value()) == entry.second.second);
        }
        CHECK(concurrent_store->release(handle.value()).ok());
    }
    return true;
}

bool test_placement(const fs::path&) {
    current_test = "placement";
    battle::Scene scene = basic_scene();
    std::shared_ptr<battle::RegionStore> store;
    if (!make_store(scene, store)) {
        return false;
    }
    battle::Outcome<battle::SnapshotHandle> handle = store->acquire();
    CHECK(handle.ok());
    if (!handle) {
        return false;
    }
    battle::PlacementRequest legal;
    legal.region.vertices = {
        {16.0F, 0.0F, 2.0F},
        {18.0F, 0.0F, 2.0F},
        {18.0F, 0.0F, 4.0F},
        {16.0F, 0.0F, 4.0F}};
    battle::Outcome<battle::PlacementResult> legal_result =
        battle::validate_placement(*store, handle.value(), legal);
    CHECK(legal_result.ok());
    CHECK(legal_result.value().legal);
    CHECK(legal_result.value().reason == battle::PlacementReason::Legal);

    battle::PlacementRequest unit_overlap;
    unit_overlap.region.vertices = {
        {0.5F, 0.0F, 3.5F},
        {3.5F, 0.0F, 3.5F},
        {3.5F, 0.0F, 6.5F},
        {0.5F, 0.0F, 6.5F}};
    battle::Outcome<battle::PlacementResult> unit_result =
        battle::validate_placement(*store, handle.value(), unit_overlap);
    CHECK(unit_result.ok());
    CHECK(!unit_result.value().legal);
    CHECK(unit_result.value().reason == battle::PlacementReason::UnitOverlap);
    CHECK(!unit_result.value().conflicts.empty());
    CHECK(unit_result.value().conflicts.front().kind ==
          battle::EntityKind::Unit);
    CHECK(unit_result.value().failed_vertex >= 0);

    battle::PlacementRequest cover_overlap;
    cover_overlap.region.vertices = {
        {11.5F, 0.0F, 7.5F},
        {14.5F, 0.0F, 7.5F},
        {14.5F, 0.0F, 8.5F},
        {11.5F, 0.0F, 8.5F}};
    battle::Outcome<battle::PlacementResult> cover_result =
        battle::validate_placement(*store, handle.value(), cover_overlap);
    CHECK(cover_result.ok());
    CHECK(!cover_result.value().legal);
    CHECK(cover_result.value().reason == battle::PlacementReason::CoverOverlap);

    battle::PlacementRequest self;
    self.region.vertices = {
        {5.0F, 0.0F, 10.0F},
        {7.0F, 0.0F, 12.0F},
        {7.0F, 0.0F, 10.0F},
        {5.0F, 0.0F, 12.0F}};
    battle::Outcome<battle::PlacementResult> self_result =
        battle::validate_placement(*store, handle.value(), self);
    CHECK(!self_result.ok());
    CHECK(self_result.error().code == battle::StatusCode::RegionInvalid);
    CHECK(self_result.error().vertex_index >= 0);

    battle::PlacementRequest outside;
    outside.region.vertices = {
        {30.0F, 0.0F, 2.0F},
        {31.0F, 0.0F, 2.0F},
        {31.0F, 0.0F, 3.0F}};
    battle::Outcome<battle::PlacementResult> outside_result =
        battle::validate_placement(*store, handle.value(), outside);
    CHECK(!outside_result.ok());
    CHECK(outside_result.error().code == battle::StatusCode::RegionInvalid);
    CHECK(outside_result.error().vertex_index == 0);

    battle::PlacementRequest off_plane = legal;
    off_plane.region.vertices[2].y = 1.0F;
    battle::Outcome<battle::PlacementResult> plane_result =
        battle::validate_placement(*store, handle.value(), off_plane);
    CHECK(!plane_result.ok());
    CHECK(plane_result.error().code == battle::StatusCode::RegionInvalid);
    CHECK(plane_result.error().vertex_index == 2);

    const std::string old_hash = handle.value().content_hash;
    battle::GridChange block;
    block.x = 10;
    block.y = 0;
    block.z = 2;
    block.walkable = false;
    battle::RegionState current = store->current_state_copy();
    battle::Outcome<battle::RegionState> blocked =
        battle::apply_region_mutation(current, block);
    CHECK(blocked.ok());
    if (blocked) {
        CHECK(store->publish(blocked.value()).ok());
    }
    CHECK(store->release(handle.value()).ok());
    battle::Outcome<battle::SnapshotHandle> current_handle = store->acquire();
    CHECK(current_handle.ok());
    if (!current_handle) {
        return false;
    }
    battle::PlacementRequest blocked_region;
    blocked_region.region.vertices = {
        {10.2F, 0.0F, 2.2F},
        {10.6F, 0.0F, 2.2F},
        {10.6F, 0.0F, 2.6F},
        {10.2F, 0.0F, 2.6F}};
    battle::Outcome<battle::PlacementResult> blocked_result =
        battle::validate_placement(*store, current_handle.value(), blocked_region);
    CHECK(blocked_result.ok());
    CHECK(!blocked_result.value().legal);
    CHECK(blocked_result.value().reason == battle::PlacementReason::BlockedCell);
    CHECK(blocked_result.value().failed_cell_x == 10);
    CHECK(blocked_result.value().failed_cell_z == 2);

    battle::PlacementRequest old_expected = legal;
    old_expected.expected_hash = old_hash;
    battle::Outcome<battle::PlacementResult> stale =
        battle::validate_placement(*store, current_handle.value(), old_expected);
    CHECK(!stale.ok());
    CHECK(stale.error().code == battle::StatusCode::RegionStale);

    battle::InjectionSpec injection;
    injection.kind = battle::InjectionKind::SelfIntersectingDraft;
    battle::Outcome<battle::PlacementResult> injected =
        battle::InjectionGate::placement(
            *store, current_handle.value(), legal, injection);
    CHECK(!injected.ok());
    CHECK(injected.error().code == battle::StatusCode::RegionInvalid);
    CHECK(injected.error().injection_point ==
          "injection.self_intersecting_draft");
    CHECK(store->release(current_handle.value()).ok());
    return true;
}

bool test_injection(const fs::path&) {
    current_test = "injection";
    std::shared_ptr<battle::RegionStore> store;
    if (!make_store(basic_scene(), store)) {
        return false;
    }
    battle::Outcome<battle::SnapshotHandle> handle = store->acquire();
    CHECK(handle.ok());
    if (!handle) {
        return false;
    }
    battle::PickRequest request;
    request.camera = unit_camera();
    request.screen = {request.camera.viewport_width * 0.5F,
                      request.camera.viewport_height * 0.5F};

    battle::InjectionSpec degenerate;
    degenerate.kind = battle::InjectionKind::DegenerateUnitBounds;
    degenerate.target_unit = 1;
    battle::Outcome<battle::PickResult> degenerate_result =
        battle::InjectionGate::pick_screen(
            *store, handle.value(), request, degenerate);
    CHECK(!degenerate_result.ok());
    CHECK(degenerate_result.error().code == battle::StatusCode::SceneInvalid);
    CHECK(degenerate_result.error().entity.kind == battle::EntityKind::Unit);
    CHECK(degenerate_result.error().entity.id == 1);
    CHECK(degenerate_result.error().injection_point ==
          "injection.degenerate_bounds");

    battle::InjectionSpec zero;
    zero.kind = battle::InjectionKind::ZeroLengthRay;
    battle::Outcome<battle::PickResult> zero_result =
        battle::InjectionGate::pick_ray(
            *store, handle.value(),
            battle::Ray{{1.0F, 1.0F, 1.0F}, {2.0F, 3.0F, 4.0F}}, zero);
    CHECK(!zero_result.ok());
    CHECK(zero_result.error().code == battle::StatusCode::RayInvalid);
    CHECK(zero_result.error().injection_point.find("injection.zero_length_ray") ==
          0);

    battle::InjectionSpec outside;
    outside.kind = battle::InjectionKind::OutOfBoundsScreen;
    battle::PickRequest outside_request = request;
    outside_request.screen = {request.camera.viewport_width + 1.0F, 0.0F};
    battle::Outcome<battle::PickResult> outside_result =
        battle::InjectionGate::pick_screen(
            *store, handle.value(), outside_request, outside);
    CHECK(!outside_result.ok());
    CHECK(outside_result.error().code == battle::StatusCode::RayInvalid);
    CHECK(outside_result.error().injection_point ==
          "injection.out_of_bounds_screen");

    battle::InjectionSpec corrupt;
    corrupt.kind = battle::InjectionKind::CorruptSnapshot;
    battle::Outcome<battle::PickResult> corrupt_result =
        battle::InjectionGate::pick_screen(
            *store, handle.value(), request, corrupt);
    CHECK(!corrupt_result.ok());
    CHECK(corrupt_result.error().code == battle::StatusCode::SnapshotStale);

    CHECK(store->release(handle.value()).ok());
    battle::Outcome<battle::PickResult> stale_release =
        battle::InjectionGate::pick_screen(
            *store, handle.value(), request, degenerate);
    CHECK(!stale_release.ok());
    CHECK(stale_release.error().code == battle::StatusCode::SnapshotStale);
    return true;
}

bool test_trace(const fs::path& work) {
    current_test = "trace";
    battle::Scene scene = basic_scene();
    std::shared_ptr<battle::RegionStore> store;
    battle::RegionState initial;
    if (!make_store(scene, store, &initial)) {
        return false;
    }
    battle::PickRequest request;
    request.camera = unit_camera();
    request.screen = {request.camera.viewport_width * 0.5F,
                      request.camera.viewport_height * 0.5F};
    battle::Outcome<battle::SnapshotHandle> first_handle = store->acquire();
    CHECK(first_handle.ok());
    if (!first_handle) {
        return false;
    }
    battle::Outcome<battle::PickResult> first_pick =
        battle::pick_screen(*store, first_handle.value(), request);
    CHECK(first_pick.ok());
    battle::TraceRecorder recorder;
    recorder.record_pick(first_handle.value().version,
                         first_handle.value().content_hash, request,
                         first_pick.value().ray, first_pick);
    CHECK(store->release(first_handle.value()).ok());

    battle::UnitMove move;
    move.id = 1;
    move.delta = {4.0F, 0.0F, 0.0F};
    battle::Outcome<battle::RegionState> moved =
        battle::apply_region_mutation(initial, move);
    CHECK(moved.ok());
    if (!moved) {
        return false;
    }
    CHECK(store->publish(moved.value()).ok());
    battle::Outcome<battle::SnapshotHandle> second_handle = store->acquire();
    CHECK(second_handle.ok());
    battle::Outcome<battle::PickResult> second_pick =
        battle::pick_screen(*store, second_handle.value(), request);
    CHECK(second_pick.ok());
    recorder.record_pick(second_handle.value().version,
                         second_handle.value().content_hash, request,
                         second_pick.value().ray, second_pick);

    battle::PlacementRequest draft;
    draft.region.vertices = {
        {16.0F, 0.0F, 2.0F},
        {18.0F, 0.0F, 2.0F},
        {18.0F, 0.0F, 4.0F},
        {16.0F, 0.0F, 4.0F}};
    battle::Outcome<battle::PlacementResult> placement =
        battle::validate_placement(*store, second_handle.value(), draft);
    CHECK(placement.ok());
    recorder.record_placement(second_handle.value().version,
                              second_handle.value().content_hash, draft,
                              placement);

    battle::BoxSelectRequest box;
    box.camera = request.camera;
    box.rect = {0.0F, 0.0F, 1024.0F, 768.0F};
    battle::Outcome<battle::BoxSelectResult> selected =
        battle::box_select(*store, second_handle.value(), box);
    CHECK(selected.ok());
    recorder.record_box(second_handle.value().version,
                        second_handle.value().content_hash, box, selected);
    CHECK(store->release(second_handle.value()).ok());

    battle::TraceRecorder duplicate;
    duplicate.record_pick(first_handle.value().version,
                          first_handle.value().content_hash, request,
                          first_pick.value().ray, first_pick);
    duplicate.record_pick(second_handle.value().version,
                          second_handle.value().content_hash, request,
                          second_pick.value().ray, second_pick);
    duplicate.record_placement(second_handle.value().version,
                               second_handle.value().content_hash, draft,
                               placement);
    duplicate.record_box(second_handle.value().version,
                         second_handle.value().content_hash, box, selected);
    CHECK(recorder.text() == duplicate.text());
    CHECK(recorder.text().find("timestamp") == std::string::npos);

    const fs::path trace_path = work / "trace" / "trace.log";
    CHECK(recorder.write_file(trace_path.string()).ok());
    battle::Outcome<battle::TraceFile> loaded =
        battle::load_trace(trace_path.string());
    CHECK(loaded.ok());
    CHECK(loaded.value().records.size() == 4);
    if (!loaded) {
        return false;
    }
    const std::vector<battle::RegionState> snapshots = {initial, moved.value()};
    battle::Outcome<battle::ReplayReport> replay =
        battle::replay_trace(loaded.value(), snapshots);
    CHECK(replay.ok());
    CHECK(replay.ok() && replay.value().records == 4);

    std::string missing_version = recorder.text();
    const std::size_t version_position = missing_version.find("|version=");
    missing_version.erase(version_position, missing_version.find('|', version_position + 1) -
                                                   version_position);
    const fs::path missing_trace = work / "trace" / "missing-version.log";
    CHECK(write_bytes(missing_trace, missing_version));
    battle::Outcome<battle::TraceFile> missing =
        battle::load_trace(missing_trace.string());
    CHECK(!missing.ok());
    CHECK(missing.error().code == battle::StatusCode::TraceInvalid);
    CHECK(missing.error().injection_point.find("version") != std::string::npos);

    std::string timestamped = recorder.text();
    const std::size_t hash_position = timestamped.find("|record_hash=");
    timestamped.insert(hash_position, "|timestamp=123");
    const fs::path timestamp_trace = work / "trace" / "timestamp.log";
    CHECK(write_bytes(timestamp_trace, timestamped));
    battle::Outcome<battle::TraceFile> timestamp =
        battle::load_trace(timestamp_trace.string());
    CHECK(!timestamp.ok());
    CHECK(timestamp.error().code == battle::StatusCode::TraceInvalid);

    std::string tampered = recorder.text();
    const std::size_t hit_position = tampered.find("|hit=unit:1");
    CHECK(hit_position != std::string::npos);
    tampered.replace(hit_position, 12, "|hit=unit:999");
    const fs::path tampered_trace = work / "trace" / "tampered.log";
    CHECK(write_bytes(tampered_trace, tampered));
    battle::Outcome<battle::TraceFile> bad =
        battle::load_trace(tampered_trace.string());
    CHECK(!bad.ok());
    CHECK(bad.error().code == battle::StatusCode::TraceInvalid);

    std::string broken_chain = recorder.text();
    const std::size_t second_line_start = broken_chain.find('\n') + 1;
    const std::size_t second_line_end = broken_chain.find('\n', second_line_start);
    std::string second_line =
        broken_chain.substr(second_line_start, second_line_end - second_line_start);
    const std::size_t prev_position = second_line.find("prev_hash=");
    CHECK(prev_position != std::string::npos);
    second_line.replace(prev_position + 10, 64,
                        "1111111111111111111111111111111111111111111111111111111111111111");
    const std::string body =
        second_line.substr(0, second_line.rfind("|record_hash="));
    second_line = body + "|record_hash=" +
                  battle::digest_hex(battle::sha256(body));
    broken_chain.replace(second_line_start, second_line_end - second_line_start,
                         second_line);
    const fs::path broken_trace = work / "trace" / "broken-chain.log";
    CHECK(write_bytes(broken_trace, broken_chain));
    battle::Outcome<battle::TraceFile> broken =
        battle::load_trace(broken_trace.string());
    CHECK(!broken.ok());
    CHECK(broken.error().code == battle::StatusCode::TraceInvalid);
    CHECK(broken.error().injection_point.find("prev_hash") != std::string::npos);
    return true;
}

std::map<std::string, std::string> collect_tree(const fs::path& root) {
    std::map<std::string, std::string> files;
    for (const fs::directory_entry& entry :
         fs::recursive_directory_iterator(root)) {
        if (entry.is_regular_file()) {
            files.emplace(fs::relative(entry.path(), root).generic_string(),
                          read_bytes(entry.path()));
        }
    }
    return files;
}

bool test_samples(const fs::path& work) {
    current_test = "samples";
    const fs::path first = work / "samples-a";
    const fs::path second = work / "samples-b";
    battle::Outcome<battle::GenerationReport> generated_first =
        battle::generate_samples(20261121, first.string(), 3);
    CHECK(generated_first.ok());
    if (!generated_first) {
        return false;
    }
    battle::Outcome<battle::GenerationReport> generated_second =
        battle::generate_samples(20261121, second.string(), 3);
    CHECK(generated_second.ok());
    CHECK(generated_first.value().manifest_hash ==
          generated_second.value().manifest_hash);
    CHECK(collect_tree(first) == collect_tree(second));

    const fs::path case_root = first / "case-000";
    const fs::path actual_trace = work / "samples-actual" / "trace.log";
    const fs::path actual_snapshots = work / "samples-actual" / "snapshots";
    battle::Outcome<battle::RunReport> actual = battle::run_experiment(
        (case_root / "scene.bin").string(),
        (case_root / "ops.txt").string(),
        actual_trace.string(), actual_snapshots.string());
    CHECK(actual.ok());
    if (!actual) {
        return false;
    }
    CHECK(read_bytes(actual_trace) ==
          read_bytes(case_root / "baseline" / "trace.log"));

    battle::Outcome<battle::ReplayReport> replay =
        battle::replay_trace_files(actual_trace.string(),
                                   actual_snapshots.string());
    CHECK(replay.ok());
    CHECK(replay.ok() && replay.value().records == actual.value().records);

    battle::Outcome<battle::TraceFile> trace =
        battle::load_trace(actual_trace.string());
    CHECK(trace.ok());
    bool saw_legal = false;
    bool saw_rejected = false;
    bool saw_pick = false;
    if (trace) {
        for (const battle::TraceRecord& record : trace.value().records) {
            CHECK(record.version > 0);
            if (record.type == battle::TraceType::Pick) {
                saw_pick = true;
            }
            if (record.type == battle::TraceType::Draft &&
                record.status == battle::StatusCode::Ok) {
                saw_legal = saw_legal || record.draft_legal;
                saw_rejected = saw_rejected || !record.draft_legal;
            }
        }
    }
    CHECK(saw_pick);
    CHECK(saw_legal);
    CHECK(saw_rejected);
    return true;
}

bool test_deterministic_rerun() {
    current_test = "deterministic_rerun";
    std::shared_ptr<battle::RegionStore> store;
    if (!make_store(basic_scene(), store)) {
        return false;
    }
    battle::PickRequest request;
    request.camera = unit_camera();
    request.screen = {512.0F, 384.0F};
    std::vector<std::string> fingerprints;
    for (int run = 0; run < 20; ++run) {
        battle::Outcome<battle::SnapshotHandle> handle = store->acquire();
        if (!handle) {
            CHECK(false);
            return false;
        }
        battle::Outcome<battle::PickResult> pick =
            battle::pick_screen(*store, handle.value(), request);
        CHECK(pick.ok());
        if (pick) {
            fingerprints.push_back(pick_fingerprint(pick.value()));
        }
        CHECK(store->release(handle.value()).ok());
    }
    for (const std::string& fingerprint : fingerprints) {
        CHECK(fingerprint == fingerprints.front());
    }
    return true;
}

}  // namespace

int main(int argc, char** argv) {
    fs::path work = fs::current_path() / "test-work";
    if (argc > 1) {
        work = argv[1];
    }
    std::error_code error;
    fs::remove_all(work, error);
    fs::create_directories(work, error);
    if (error) {
        std::cerr << "cannot prepare test directory: " << error.message() << "\n";
        return 2;
    }

    test_scene_validation(work);
    test_ray_and_selection(work);
    test_versions_and_concurrency(work);
    test_placement(work);
    test_injection(work);
    test_trace(work);
    test_samples(work);
    test_deterministic_rerun();

    if (failures != 0) {
        std::cerr << failures << " test assertion(s) failed\n";
        return 1;
    }
    std::cout << "all battle tests passed\n";
    return 0;
}
