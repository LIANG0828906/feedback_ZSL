#pragma once

#include <atomic>
#include <cstdint>
#include <optional>
#include <vector>

#include "shadowpipe/cascades.hpp"
#include "shadowpipe/config.hpp"
#include "shadowpipe/scene.hpp"

namespace shadowpipe {

enum class OcclusionVerdict {
    Visible,
    Occluded,
    Ambiguous,
};

const char* occlusionVerdictName(OcclusionVerdict verdict);

struct PyramidLevelMeta {
    std::uint32_t index = 0;
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    std::uint32_t quantization_bits = 0;
    float depth_min = 0.0f;
    float depth_max = 0.0f;
    float quantization_step = 0.0f;
    float max_quantization_error = 0.0f;
};

struct PyramidLevel {
    PyramidLevelMeta meta;
    std::vector<float> depth;
    std::vector<std::uint32_t> quantized;
    std::vector<std::uint32_t> owners;
};

struct LevelRangeDelta {
    std::uint32_t level = 0;
    float old_min = 0.0f;
    float new_min = 0.0f;
    float old_max = 0.0f;
    float new_max = 0.0f;
    float min_difference = 0.0f;
    float max_difference = 0.0f;
    float old_quantization_step = 0.0f;
    float new_quantization_step = 0.0f;
    bool quantization_step_changed = false;
};

struct PyramidRebuildReport {
    bool rebuilt = false;
    std::string reason;
    std::vector<LevelRangeDelta> level_deltas;
    float old_new_level_range_difference_max = 0.0f;
    float max_quantization_error = 0.0f;
};

struct PyramidMetadata {
    LightFrame frame;
    std::vector<PyramidLevelMeta> levels;
};

struct OccluderPyramid {
    LightFrame frame;
    std::vector<PyramidLevel> levels;
    PyramidRebuildReport rebuild_report;
    std::uint64_t content_hash = 0;

    PyramidMetadata metadata() const;
};

struct OcclusionLevelResult {
    std::uint32_t level = 0;
    OcclusionVerdict verdict = OcclusionVerdict::Visible;
    float margin_lower = 0.0f;
    float margin_upper = 0.0f;
    float quantization_error = 0.0f;
    std::uint32_t sampled_texels = 0;
    std::uint32_t covered_texels = 0;
};

struct OcclusionQueryResult {
    OcclusionVerdict verdict = OcclusionVerdict::Visible;
    float margin_lower = 0.0f;
    float margin_upper = 0.0f;
    float quantization_error = 0.0f;
    bool ambiguous = false;
    Status status;
    std::vector<OcclusionLevelResult> levels;
};

Status buildOccluderPyramid(const Scene& scene, const RenderConfig& config,
                            const PyramidMetadata* previous, OccluderPyramid& pyramid);

OcclusionQueryResult queryOcclusion(const OccluderPyramid& pyramid,
                                    const AABB& world_bounds,
                                    std::uint32_t exclude_node = kNoMesh);

}  // namespace shadowpipe

