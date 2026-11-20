#include "shadowpipe/pyramid.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <limits>
#include <string>

#include "shadowpipe/hash.hpp"
#include "shadowpipe/raster.hpp"

namespace shadowpipe {
namespace {

Status cancellationStatus(const RenderConfig& config) {
    if (config.cancel != nullptr && config.cancel->load(std::memory_order_relaxed)) {
        return Status::error(ErrorCode::Cancelled, "render cancelled at pyramid checkpoint");
    }
    return Status::success();
}

void callCheckpoint(const RenderConfig& config, Checkpoint checkpoint,
                    std::uint32_t level) {
    if (config.checkpoint_hook) {
        config.checkpoint_hook(checkpoint, level);
    }
}

bool nearlyEqual(float a, float b) {
    return std::abs(a - b) <=
           std::max(1.0e-7f, std::max(std::abs(a), std::abs(b)) * 1.0e-6f);
}

void quantizeLevel(PyramidLevel& level, std::uint32_t bits) {
    level.meta.quantization_bits = bits;
    level.meta.depth_min = kInfinity;
    level.meta.depth_max = -kInfinity;
    for (const float depth : level.depth) {
        if (std::isfinite(depth)) {
            level.meta.depth_min = std::min(level.meta.depth_min, depth);
            level.meta.depth_max = std::max(level.meta.depth_max, depth);
        }
    }
    if (!std::isfinite(level.meta.depth_min) ||
        !std::isfinite(level.meta.depth_max)) {
        level.meta.depth_min = 0.0f;
        level.meta.depth_max = 1.0f;
    }
    const float range = level.meta.depth_max - level.meta.depth_min;
    const std::uint32_t max_code = (1u << bits) - 1u;
    if (range <= 1.0e-12f) {
        level.meta.quantization_step = 0.0f;
        level.meta.max_quantization_error = 0.0f;
        level.quantized.assign(level.depth.size(), 0u);
        for (std::size_t i = 0; i < level.depth.size(); ++i) {
            level.depth[i] = level.meta.depth_min;
        }
        return;
    }
    level.meta.quantization_step = range / static_cast<float>(max_code);
    level.meta.max_quantization_error = level.meta.quantization_step * 0.5f;
    level.quantized.resize(level.depth.size());
    for (std::size_t i = 0; i < level.depth.size(); ++i) {
        const float normalized =
            (level.depth[i] - level.meta.depth_min) / level.meta.quantization_step;
        const long code = std::lround(
            std::max(0.0f, std::min(static_cast<float>(max_code), normalized)));
        level.quantized[i] = static_cast<std::uint32_t>(code);
        level.depth[i] =
            level.meta.depth_min + static_cast<float>(code) * level.meta.quantization_step;
    }
}

PyramidLevel downsampleLevel(const PyramidLevel& source) {
    PyramidLevel destination;
    destination.meta.index = source.meta.index + 1u;
    destination.meta.width = std::max(1u, (source.meta.width + 1u) / 2u);
    destination.meta.height = std::max(1u, (source.meta.height + 1u) / 2u);
    destination.depth.assign(
        static_cast<std::size_t>(destination.meta.width) * destination.meta.height,
        1.0f);
    destination.owners.assign(destination.depth.size(), kNoMesh);
    for (std::uint32_t y = 0; y < destination.meta.height; ++y) {
        for (std::uint32_t x = 0; x < destination.meta.width; ++x) {
            float minimum = 1.0f;
            std::uint32_t owner = kNoMesh;
            for (std::uint32_t oy = 0; oy < 2; ++oy) {
                for (std::uint32_t ox = 0; ox < 2; ++ox) {
                    const std::uint32_t sx = x * 2u + ox;
                    const std::uint32_t sy = y * 2u + oy;
                    if (sx < source.meta.width && sy < source.meta.height) {
                        const std::size_t source_index =
                            static_cast<std::size_t>(sy) * source.meta.width + sx;
                        if (source.depth[source_index] < minimum) {
                            minimum = source.depth[source_index];
                            if (source_index < source.owners.size()) {
                                owner = source.owners[source_index];
                            }
                        }
                    }
                }
            }
            const std::size_t destination_index =
                static_cast<std::size_t>(y) * destination.meta.width + x;
            destination.depth[destination_index] = minimum;
            destination.owners[destination_index] = owner;
        }
    }
    return destination;
}

}  // namespace

const char* occlusionVerdictName(OcclusionVerdict verdict) {
    switch (verdict) {
        case OcclusionVerdict::Visible:
            return "VISIBLE";
        case OcclusionVerdict::Occluded:
            return "OCCLUDED";
        case OcclusionVerdict::Ambiguous:
            return "AMBIGUOUS";
    }
    return "AMBIGUOUS";
}

PyramidMetadata OccluderPyramid::metadata() const {
    PyramidMetadata out;
    out.frame = frame;
    out.levels.reserve(levels.size());
    for (const PyramidLevel& level : levels) {
        out.levels.push_back(level.meta);
    }
    return out;
}

Status buildOccluderPyramid(const Scene& scene, const RenderConfig& config,
                            const PyramidMetadata* previous,
                            OccluderPyramid& pyramid) {
    pyramid = {};
    Status status = cancellationStatus(config);
    if (!status.ok()) {
        return status;
    }
    if (config.pyramid_resolution == 0 ||
        config.pyramid_resolution > 2048) {
        return Status::error(ErrorCode::InvalidArgument,
                             "pyramid resolution must be in [1, 2048]");
    }

    AABB scene_bounds;
    for (const Node& node : scene.nodes) {
        scene_bounds.expand(node.content_bounds.valid() ? node.content_bounds
                                                       : node.declared_bounds);
    }
    if (!scene_bounds.valid()) {
        return Status::error(ErrorCode::InternalError,
                             "scene has no valid bounds for the depth pyramid");
    }
    scene_bounds.pad(0.05f);

    const Vec3 light_forward = glm::normalize(config.light.direction);
    Vec3 reference_up(0.0f, 1.0f, 0.0f);
    if (std::abs(glm::dot(reference_up, light_forward)) > 0.98f) {
        reference_up = Vec3(1.0f, 0.0f, 0.0f);
    }
    Vec3 light_right = glm::cross(reference_up, light_forward);
    if (glm::length(light_right) < 1.0e-6f) {
        return Status::error(ErrorCode::InvalidArgument,
                             "cannot construct a stable light-space basis");
    }
    light_right = glm::normalize(light_right);
    const Vec3 light_up = glm::normalize(glm::cross(light_forward, light_right));

    LightFrame frame;
    frame.axis_x = light_right;
    frame.axis_y = light_up;
    frame.axis_z = light_forward;
    ProjectedBounds projected = projectAABB(scene_bounds, frame);
    const float x_pad = std::max(0.01f, (projected.x_max - projected.x_min) * 0.01f);
    const float y_pad = std::max(0.01f, (projected.y_max - projected.y_min) * 0.01f);
    const float z_pad = std::max(0.01f, (projected.z_max - projected.z_min) * 0.01f);
    frame.x_min = projected.x_min - x_pad;
    frame.x_max = projected.x_max + x_pad;
    frame.y_min = projected.y_min - y_pad;
    frame.y_max = projected.y_max + y_pad;
    frame.z_min = projected.z_min - z_pad;
    frame.z_max = projected.z_max + z_pad;
    pyramid.frame = frame;

    const std::vector<WorldTriangle> triangles = buildWorldTriangles(scene);
    PyramidLevel base;
    base.meta.index = 0;
    base.meta.width = config.pyramid_resolution;
    base.meta.height = config.pyramid_resolution;

    callCheckpoint(config, Checkpoint::BeforePyramidLevel, 0);
    status = cancellationStatus(config);
    if (!status.ok()) {
        return status;
    }
    rasterizeDepth(triangles, frame, base.meta.width, base.meta.height, base.depth,
                   nullptr, &base.owners);
    quantizeLevel(base, 24);
    pyramid.levels.push_back(std::move(base));
    callCheckpoint(config, Checkpoint::AfterPyramidLevel, 0);
    status = cancellationStatus(config);
    if (!status.ok()) {
        return status;
    }

    while ((pyramid.levels.back().meta.width > 1u ||
            pyramid.levels.back().meta.height > 1u) &&
           pyramid.levels.size() < 12u) {
        const std::uint32_t next_index =
            static_cast<std::uint32_t>(pyramid.levels.size());
        callCheckpoint(config, Checkpoint::BeforePyramidLevel, next_index);
        status = cancellationStatus(config);
        if (!status.ok()) {
            return status;
        }
        PyramidLevel next = downsampleLevel(pyramid.levels.back());
        quantizeLevel(next, std::max(12u, 24u - next_index * 2u));
        pyramid.levels.push_back(std::move(next));
        callCheckpoint(config, Checkpoint::AfterPyramidLevel, next_index);
        status = cancellationStatus(config);
        if (!status.ok()) {
            return status;
        }
    }

    PyramidRebuildReport& report = pyramid.rebuild_report;
    report.rebuilt = true;
    report.reason = "same_parameters_rebuilt_for_frame_consistency";
    if (previous != nullptr && previous->levels.size() == pyramid.levels.size() &&
        nearlyEqual(previous->frame.x_min, frame.x_min) &&
        nearlyEqual(previous->frame.x_max, frame.x_max) &&
        nearlyEqual(previous->frame.y_min, frame.y_min) &&
        nearlyEqual(previous->frame.y_max, frame.y_max) &&
        nearlyEqual(previous->frame.z_min, frame.z_min) &&
        nearlyEqual(previous->frame.z_max, frame.z_max)) {
    } else if (previous != nullptr) {
        report.reason = "light_or_scene_bounds_changed";
    }

    for (std::size_t i = 0; i < pyramid.levels.size(); ++i) {
        const PyramidLevelMeta& current = pyramid.levels[i].meta;
        LevelRangeDelta delta;
        delta.level = current.index;
        delta.new_min = current.depth_min;
        delta.new_max = current.depth_max;
        delta.new_quantization_step = current.quantization_step;
        if (previous != nullptr && i < previous->levels.size()) {
            const PyramidLevelMeta& old = previous->levels[i];
            delta.old_min = old.depth_min;
            delta.old_max = old.depth_max;
            delta.old_quantization_step = old.quantization_step;
            delta.min_difference = current.depth_min - old.depth_min;
            delta.max_difference = current.depth_max - old.depth_max;
            delta.quantization_step_changed =
                !nearlyEqual(current.quantization_step, old.quantization_step);
            report.old_new_level_range_difference_max =
                std::max({report.old_new_level_range_difference_max,
                          std::abs(delta.min_difference),
                          std::abs(delta.max_difference)});
        } else {
            delta.old_min = current.depth_min;
            delta.old_max = current.depth_max;
            delta.old_quantization_step = current.quantization_step;
            delta.min_difference = 0.0f;
            delta.max_difference = 0.0f;
        }
        report.max_quantization_error =
            std::max(report.max_quantization_error, current.max_quantization_error);
        report.level_deltas.push_back(delta);
    }

    Hasher hasher;
    hasher.string("occluder-pyramid-v1");
    hasher.f32(frame.axis_x.x);
    hasher.f32(frame.axis_x.y);
    hasher.f32(frame.axis_x.z);
    hasher.f32(frame.axis_y.x);
    hasher.f32(frame.axis_y.y);
    hasher.f32(frame.axis_y.z);
    hasher.f32(frame.axis_z.x);
    hasher.f32(frame.axis_z.y);
    hasher.f32(frame.axis_z.z);
    hasher.f32(frame.x_min);
    hasher.f32(frame.x_max);
    hasher.f32(frame.y_min);
    hasher.f32(frame.y_max);
    hasher.f32(frame.z_min);
    hasher.f32(frame.z_max);
    for (const PyramidLevel& level : pyramid.levels) {
        hasher.u32(level.meta.width);
        hasher.u32(level.meta.height);
        hasher.u32(level.meta.quantization_bits);
        hasher.f32(level.meta.depth_min);
        hasher.f32(level.meta.depth_max);
        hasher.f32(level.meta.quantization_step);
        hasher.u64(level.quantized.size());
        for (const std::uint32_t value : level.quantized) {
            hasher.u32(value);
        }
        for (const std::uint32_t owner : level.owners) {
            hasher.u32(owner);
        }
    }
    pyramid.content_hash = hasher.value();
    return Status::success();
}

OcclusionQueryResult queryOcclusion(const OccluderPyramid& pyramid,
                                    const AABB& world_bounds,
                                    std::uint32_t exclude_node) {
    OcclusionQueryResult result;
    if (pyramid.levels.empty() || !world_bounds.valid()) {
        result.verdict = OcclusionVerdict::Visible;
        result.margin_lower = -kInfinity;
        result.margin_upper = kInfinity;
        return result;
    }
    const ProjectedBounds projected = projectAABB(world_bounds, pyramid.frame);
    const float frame_x_range = pyramid.frame.x_max - pyramid.frame.x_min;
    const float frame_y_range = pyramid.frame.y_max - pyramid.frame.y_min;
    if (projected.x_max < pyramid.frame.x_min ||
        projected.x_min > pyramid.frame.x_max ||
        projected.y_max < pyramid.frame.y_min ||
        projected.y_min > pyramid.frame.y_max) {
        result.verdict = OcclusionVerdict::Visible;
        result.margin_lower = -kInfinity;
        result.margin_upper = kInfinity;
        return result;
    }

    result.levels.resize(pyramid.levels.size());
    for (std::size_t reverse = pyramid.levels.size(); reverse > 0; --reverse) {
        const PyramidLevel& level = pyramid.levels[reverse - 1];
        OcclusionLevelResult level_result;
        level_result.level = level.meta.index;
        level_result.quantization_error = level.meta.max_quantization_error;

        float fx0 = (projected.x_min - pyramid.frame.x_min) / frame_x_range *
                    static_cast<float>(level.meta.width);
        float fx1 = (projected.x_max - pyramid.frame.x_min) / frame_x_range *
                    static_cast<float>(level.meta.width);
        float fy0 = (1.0f - (projected.y_max - pyramid.frame.y_min) / frame_y_range) *
                    static_cast<float>(level.meta.height);
        float fy1 = (1.0f - (projected.y_min - pyramid.frame.y_min) / frame_y_range) *
                    static_cast<float>(level.meta.height);
        fx0 = std::max(0.0f, std::min(static_cast<float>(level.meta.width - 1u), fx0));
        fx1 = std::max(0.0f, std::min(static_cast<float>(level.meta.width - 1u), fx1));
        fy0 = std::max(0.0f, std::min(static_cast<float>(level.meta.height - 1u), fy0));
        fy1 = std::max(0.0f, std::min(static_cast<float>(level.meta.height - 1u), fy1));
        const std::uint32_t x0 =
            static_cast<std::uint32_t>(std::floor(std::min(fx0, fx1)));
        const std::uint32_t x1 =
            static_cast<std::uint32_t>(std::ceil(std::max(fx0, fx1)));
        const std::uint32_t y0 =
            static_cast<std::uint32_t>(std::floor(std::min(fy0, fy1)));
        const std::uint32_t y1 =
            static_cast<std::uint32_t>(std::ceil(std::max(fy0, fy1)));

        float min_depth = kInfinity;
        float max_depth = -kInfinity;
        std::uint32_t sampled = 0;
        std::uint32_t occupied = 0;
        std::uint32_t front_covered = 0;
        const float quant_error = level.meta.max_quantization_error;
        const float near_depth = projected.z_min;
        const float far_depth = projected.z_max;
        for (std::uint32_t y = y0; y <= y1; ++y) {
            for (std::uint32_t x = x0; x <= x1; ++x) {
                ++sampled;
                const float depth =
                    level.depth[static_cast<std::size_t>(y) * level.meta.width + x];
                const std::uint32_t owner =
                    static_cast<std::size_t>(y) * level.meta.width + x <
                            level.owners.size()
                        ? level.owners[static_cast<std::size_t>(y) *
                                           level.meta.width +
                                       x]
                        : kNoMesh;
                if (depth >= 0.999999f) {
                    continue;
                }
                if (exclude_node != kNoMesh && owner == exclude_node) {
                    continue;
                }
                ++occupied;
                min_depth = std::min(min_depth, depth);
                max_depth = std::max(max_depth, depth);
                if (depth < near_depth - quant_error) {
                    ++front_covered;
                }
            }
        }
        level_result.sampled_texels = sampled;
        level_result.covered_texels = occupied;

        if (occupied == 0) {
            level_result.verdict = OcclusionVerdict::Visible;
            level_result.margin_lower = -kInfinity;
            level_result.margin_upper = kInfinity;
        } else {
            level_result.margin_lower = near_depth - max_depth - quant_error;
            level_result.margin_upper = far_depth - min_depth + quant_error;
            if (front_covered == sampled) {
                level_result.verdict = OcclusionVerdict::Occluded;
            } else if (min_depth - quant_error > far_depth) {
                level_result.verdict = OcclusionVerdict::Visible;
            } else {
                level_result.verdict = OcclusionVerdict::Ambiguous;
            }
        }
        result.levels[level.meta.index] = level_result;
    }

    result.verdict = result.levels.front().verdict;
    result.margin_lower = result.levels.front().margin_lower;
    result.margin_upper = result.levels.front().margin_upper;
    result.quantization_error = result.levels.front().quantization_error;
    result.ambiguous = result.verdict == OcclusionVerdict::Ambiguous;
    if (result.ambiguous) {
        result.status = Status::error(
            ErrorCode::OcclusionAmbiguous,
            "depth margin interval overlaps pyramid quantization error; object retained");
    }
    return result;
}

}  // namespace shadowpipe

