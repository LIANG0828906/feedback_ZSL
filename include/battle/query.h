#pragma once

#include "battle/camera.h"
#include "battle/region.h"

#include <cstdint>
#include <string>
#include <vector>

namespace battle {

struct Hit {
    EntityRef entity;
    glm::vec3 point{0.0F};
    glm::vec3 normal{0.0F, 1.0F, 0.0F};
    float distance = 0.0F;
};

struct PickRequest {
    Camera camera;
    ScreenPoint screen;
    std::uint64_t expected_version = 0;
    std::string expected_hash;
};

struct PickResult {
    std::uint64_t region_version = 0;
    std::string region_hash;
    Camera camera;
    ScreenPoint screen;
    Ray ray;
    bool has_hit = false;
    Hit hit;
};

struct ScreenRect {
    float min_x = 0.0F;
    float min_y = 0.0F;
    float max_x = 0.0F;
    float max_y = 0.0F;
};

struct BoxSelectRequest {
    Camera camera;
    ScreenRect rect;
    std::uint64_t expected_version = 0;
    std::string expected_hash;
};

struct BoxSelectResult {
    std::uint64_t region_version = 0;
    std::string region_hash;
    std::vector<std::uint32_t> unit_ids;
};

Outcome<PublishedRegionPtr> validate_query_snapshot(
    const RegionStore& store, const SnapshotHandle& handle,
    std::uint64_t expected_version, const std::string& expected_hash);
Outcome<PickResult> pick_screen(const RegionStore& store,
                                const SnapshotHandle& handle,
                                const PickRequest& request);
Outcome<PickResult> pick_screen_snapshot(const PublishedRegionPtr& snapshot,
                                         const PickRequest& request);
Outcome<PickResult> pick_ray(const RegionStore& store,
                             const SnapshotHandle& handle, Ray ray,
                             std::uint64_t expected_version = 0,
                             const std::string& expected_hash = {});
Outcome<BoxSelectResult> box_select(const RegionStore& store,
                                    const SnapshotHandle& handle,
                                    const BoxSelectRequest& request);

}  // namespace battle

