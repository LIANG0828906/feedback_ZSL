#pragma once

#include <cstdint>
#include <string>

#include "shadowpipe/cascades.hpp"
#include "shadowpipe/config.hpp"
#include "shadowpipe/pyramid.hpp"

namespace shadowpipe {

constexpr std::uint32_t kSnapshotFormatVersion = 1;

struct ArtifactHashes {
    std::uint64_t shadow_factor_pgm = 0;
    std::uint64_t shadow_factor_f32 = 0;
    std::uint64_t visibility_mask_pgm = 0;
    std::uint64_t error_heatmap_ppm = 0;
    std::uint64_t cascade_depth_maps = 0;
    std::uint64_t summary_json = 0;
};

struct SnapshotData {
    std::uint32_t format_version = kSnapshotFormatVersion;
    std::uint64_t scene_hash = 0;
    CameraConfig camera;
    LightConfig light;
    CascadeTable cascade_table;
    BiasParameters bias;
    float switch_tolerance = 0.05f;
    PyramidMetadata pyramid;
    std::uint32_t image_width = 0;
    std::uint32_t image_height = 0;
    ArtifactHashes hashes;
};

struct SnapshotLoadResult {
    Status status;
    SnapshotData snapshot;
};

struct SnapshotVersionDiff {
    bool scene_changed = false;
    bool view_changed = false;
    bool light_changed = false;
    bool cascade_table_changed = false;
    bool bias_changed = false;

    bool rerenderRequired() const {
        return scene_changed || view_changed || light_changed || cascade_table_changed ||
               bias_changed;
    }
};

Status writeSnapshotFile(const std::string& path, const SnapshotData& snapshot);
SnapshotLoadResult loadSnapshotFile(const std::string& path);
SnapshotVersionDiff compareSnapshotToFrame(const SnapshotData& previous,
                                           std::uint64_t scene_hash,
                                           const RenderConfig& config,
                                           const CascadeTable& cascade_table);

}  // namespace shadowpipe

