#pragma once

#include "battle/placement.h"
#include "battle/query.h"

#include <cstdint>
#include <string>
#include <vector>

namespace battle {

enum class TraceType : std::uint8_t {
    Pick = 1,
    Box = 2,
    Draft = 3
};

struct TraceRecord {
    TraceType type = TraceType::Pick;
    std::uint64_t sequence = 0;
    std::uint64_t version = 0;
    std::string region_hash;
    std::string previous_hash;
    std::string record_hash;
    StatusCode status = StatusCode::Ok;
    std::string error_name;
    std::string injection_point;

    PickRequest pick;
    Ray recorded_ray;
    bool has_hit = false;
    Hit hit;

    BoxSelectRequest box;
    std::vector<std::uint32_t> box_units;

    PlacementRequest draft;
    bool draft_legal = false;
    PlacementReason draft_reason = PlacementReason::Legal;
    int failed_vertex = -1;
    std::uint32_t failed_cell_x = 0;
    std::uint32_t failed_cell_z = 0;
    std::vector<EntityRef> conflicts;
};

struct TraceFile {
    std::vector<TraceRecord> records;
    std::string final_hash;
    std::string text;
};

class TraceRecorder {
public:
    void record_pick(std::uint64_t version, const std::string& region_hash,
                     const PickRequest& request, const Ray& ray,
                     const Outcome<PickResult>& result);
    void record_box(std::uint64_t version, const std::string& region_hash,
                    const BoxSelectRequest& request,
                    const Outcome<BoxSelectResult>& result);
    void record_placement(std::uint64_t version, const std::string& region_hash,
                          const PlacementRequest& request,
                          const Outcome<PlacementResult>& result);

    std::string text() const;
    Outcome<void> write_file(const std::string& path) const;
    std::size_t size() const noexcept { return lines_.size(); }
    const std::string& final_hash() const noexcept { return previous_hash_; }

private:
    std::uint64_t next_sequence_ = 0;
    std::string previous_hash_ =
        "0000000000000000000000000000000000000000000000000000000000000000";
    std::vector<std::string> lines_;
};

Outcome<TraceFile> load_trace(const std::string& path);
Outcome<std::vector<RegionState>> load_snapshot_directory(const std::string& path);

struct ReplayReport {
    std::uint64_t records = 0;
    std::string final_hash;
};

Outcome<ReplayReport> replay_trace(const TraceFile& trace,
                                   const std::vector<RegionState>& snapshots);
Outcome<ReplayReport> replay_trace_files(const std::string& trace_path,
                                         const std::string& snapshot_directory);

}  // namespace battle

