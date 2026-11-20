#pragma once

#include "battle/bvh.h"
#include "battle/scene.h"
#include "battle/sha256.h"

#include <cstdint>
#include <memory>
#include <mutex>
#include <string>
#include <unordered_map>
#include <vector>

namespace battle {

struct RegionState {
    std::uint64_t version = 1;
    Grid grid;
    std::vector<Unit> units;
    std::vector<Cover> covers;
    std::string content_hash;

    static RegionState from_scene(const Scene& scene);

    Digest compute_digest() const;
    std::string compute_hash() const;
    void seal();
    const Unit* find_unit(std::uint32_t id) const;
    const Cover* find_cover(std::uint32_t id) const;
};

struct PublishedRegion {
    RegionState state;
    Bvh unit_bvh;
    Bvh cover_bvh;
    std::string acceleration_hash;
};

using PublishedRegionPtr = std::shared_ptr<const PublishedRegion>;

struct SnapshotHandle {
    std::uint64_t token = 0;
    std::uint64_t version = 0;
    std::string content_hash;
    PublishedRegionPtr published;

    explicit operator bool() const noexcept { return token != 0; }
};

Outcome<PublishedRegionPtr> build_published_region(const RegionState& state);
std::string compute_acceleration_hash(const Bvh& unit_bvh,
                                      const Bvh& cover_bvh);
Outcome<RegionState> apply_region_mutation(const RegionState& base,
                                           const GridChange* grid_change,
                                           const UnitMove* unit_move,
                                           const CoverChange* cover_change);
Outcome<RegionState> apply_region_mutation(const RegionState& base,
                                           const GridChange& grid_change);
Outcome<RegionState> apply_region_mutation(const RegionState& base,
                                           const UnitMove& unit_move);
Outcome<RegionState> apply_region_mutation(const RegionState& base,
                                           const CoverChange& cover_change);

std::string encode_region_payload(const RegionState& state);
Outcome<RegionState> decode_region_payload(const std::string& payload);
Outcome<void> save_region_snapshot(const std::string& path, const RegionState& state);
Outcome<RegionState> load_region_snapshot(const std::string& path);

class RegionStore {
public:
    static Outcome<std::shared_ptr<RegionStore>> create(RegionState initial);
    static Outcome<std::shared_ptr<RegionStore>> create_archive(
        std::vector<RegionState> states);

    Outcome<SnapshotHandle> acquire(std::uint64_t expected_version = 0,
                                    const std::string& expected_hash = {});
    Outcome<SnapshotHandle> acquire_version(std::uint64_t version,
                                            const std::string& expected_hash = {});
    Outcome<PublishedRegionPtr> resolve(const SnapshotHandle& handle) const;
    Outcome<void> release(const SnapshotHandle& handle);
    Outcome<PublishedRegionPtr> publish(RegionState next);

    std::uint64_t current_version() const;
    std::string current_hash() const;
    PublishedRegionPtr current_snapshot() const;
    RegionState current_state_copy() const;

private:
    RegionStore() = default;
    Outcome<PublishedRegionPtr> register_snapshot(PublishedRegionPtr snapshot);

    mutable std::mutex publish_mutex_;
    PublishedRegionPtr current_;
    mutable std::mutex handles_mutex_;
    std::uint64_t next_token_ = 1;
    std::unordered_map<std::uint64_t, PublishedRegionPtr> handles_;
    mutable std::mutex archive_mutex_;
    std::unordered_map<std::uint64_t, PublishedRegionPtr> archive_;
};

}  // namespace battle

