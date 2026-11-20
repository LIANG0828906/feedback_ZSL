#pragma once

#include "battle/region.h"

#include <glm/vec3.hpp>

#include <cstdint>
#include <string>
#include <vector>

namespace battle {

enum class PlacementReason : std::uint8_t {
    Legal = 0,
    BlockedCell = 1,
    UnitOverlap = 2,
    CoverOverlap = 3
};

const char* placement_reason_name(PlacementReason reason) noexcept;

struct DraftRegion {
    std::vector<glm::vec3> vertices;
};

struct PlacementRequest {
    DraftRegion region;
    std::uint64_t expected_version = 0;
    std::string expected_hash;
    float unit_clearance = 0.0F;
    float cover_clearance = 0.0F;
};

struct PlacementResult {
    std::uint64_t region_version = 0;
    std::string region_hash;
    bool legal = false;
    PlacementReason reason = PlacementReason::Legal;
    int failed_vertex = -1;
    std::uint32_t failed_cell_x = 0;
    std::uint32_t failed_cell_z = 0;
    std::vector<EntityRef> conflicts;
    std::string detail;
};

Outcome<PlacementResult> validate_placement(const RegionStore& store,
                                            const SnapshotHandle& handle,
                                            const PlacementRequest& request);

}  // namespace battle

