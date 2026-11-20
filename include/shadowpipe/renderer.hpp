#pragma once

#include <cstdint>
#include <mutex>
#include <string>
#include <unordered_set>
#include <vector>

#include "shadowpipe/cascades.hpp"
#include "shadowpipe/config.hpp"
#include "shadowpipe/pyramid.hpp"
#include "shadowpipe/scene.hpp"
#include "shadowpipe/snapshot.hpp"

namespace shadowpipe {

struct OutputPaths {
    std::string directory = "output";
    std::string snapshot_file;
};

struct OcclusionNodeReport {
    std::uint32_t node_index = 0;
    std::string node_name;
    AABB world_bounds;
    bool frustum_culled = false;
    OcclusionVerdict verdict = OcclusionVerdict::Visible;
    bool ambiguous = false;
    Status status;
    float margin_lower = 0.0f;
    float margin_upper = 0.0f;
    float quantization_error = 0.0f;
    std::vector<OcclusionLevelResult> levels;
};

struct BiasErrorMetric {
    std::uint32_t cascade = 0;
    float texel_world_size = 0.0f;
    float depth_offset_world = 0.0f;
    float normal_offset_world = 0.0f;
    float depth_bias_normalized = 0.0f;
    float self_shadow_probability = 0.0f;
    float shadow_detachment_distance = 0.0f;
    float min_depth_offset_world = 0.0f;
    float max_depth_offset_world = 0.0f;
    float max_self_shadow_probability = 0.0f;
    float max_shadow_detachment_distance = 0.0f;
    std::uint32_t sample_count = 0;
    bool applied_once = true;
};

struct CascadeSwitchEvent {
    std::uint32_t pixel_x = 0;
    std::uint32_t pixel_y = 0;
    std::uint32_t node_index = 0;
    std::uint32_t selected_cascade = 0;
    std::uint32_t alternate_cascade = 0;
    float view_depth = 0.0f;
    float selected_shadow_factor = 0.0f;
    float alternate_shadow_factor = 0.0f;
    float factor_delta = 0.0f;
    float boundary_distance = 0.0f;
};

struct CascadeShadowMap {
    CascadeInfo cascade;
    std::vector<float> depth;
    std::uint64_t content_hash = 0;
};

struct RenderSummary {
    Status status;
    std::uint64_t scene_hash = 0;
    std::uint64_t frame_hash = 0;
    CameraConfig camera;
    LightConfig light;
    BiasParameters bias;
    float switch_tolerance = 0.0f;
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    std::uint32_t mesh_node_count = 0;
    std::uint32_t visible_object_count = 0;
    std::uint32_t occluded_object_count = 0;
    std::uint32_t occlusion_ambiguous_count = 0;
    std::uint32_t frustum_culled_count = 0;
    std::uint32_t cascade_switch_count = 0;
    float cascade_switch_factor_delta_upper_bound = 0.0f;
    float shadow_factor_delta_upper_bound = 0.0f;
    CascadeTable cascade_table;
    std::vector<OcclusionNodeReport> occlusion_nodes;
    PyramidRebuildReport pyramid_rebuild;
    std::vector<PyramidLevelMeta> pyramid_levels;
    std::vector<BiasErrorMetric> bias_metrics;
    std::vector<CascadeSwitchEvent> switch_events;
    ArtifactHashes artifact_hashes;
    SnapshotVersionDiff snapshot_diff;
    bool snapshot_loaded = false;
};

struct RenderResult {
    Status status;
    RenderSummary summary;
    SnapshotData snapshot;
    std::vector<float> shadow_factor;
    std::vector<std::uint8_t> visibility_mask;
    std::vector<float> error_map;
    std::vector<CascadeShadowMap> shadow_maps;
    bool write_heatmap = true;
    bool outputs_written = false;
};

class RenderService {
public:
    RenderResult render(const Scene& scene, const RenderConfig& config,
                        const OutputPaths& paths,
                        const SnapshotData* previous_snapshot = nullptr);

    bool isBusy(std::uint64_t scene_hash) const;

private:
    mutable std::mutex mutex_;
    std::unordered_set<std::uint64_t> active_scenes_;
};

Status clearRenderOutputs(const std::string& directory,
                          const std::string& preserve_snapshot_path = {});
Status writePipelineErrorSummary(const std::string& directory, const Status& status);

}  // namespace shadowpipe

