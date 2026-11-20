#include "shadowpipe/renderer.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <exception>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <limits>
#include <locale>
#include <sstream>
#include <string>
#include <utility>

#include "shadowpipe/hash.hpp"
#include "shadowpipe/raster.hpp"

namespace shadowpipe {
namespace {

struct BiasSample {
    float depth_offset_world = 0.0f;
    float normal_offset_world = 0.0f;
    float depth_bias_normalized = 0.0f;
    float depth_offset_texels = 0.0f;
    float normal_offset_texels = 0.0f;
    float total_offset_texels = 0.0f;
    float self_shadow_probability = 0.0f;
    float shadow_detachment_distance = 0.0f;
};

struct SurfaceHit {
    bool hit = false;
    float t = kInfinity;
    Vec3 position{0.0f};
    Vec3 normal{0.0f};
    std::uint32_t node_index = 0;
};

std::string joinPath(const std::string& directory, const std::string& name) {
    return (std::filesystem::path(directory) / name).string();
}

bool cancellationRequested(const RenderConfig& config) {
    return config.cancel != nullptr &&
           config.cancel->load(std::memory_order_relaxed);
}

Status checkpoint(const RenderConfig& config, Checkpoint kind,
                  std::uint32_t index) {
    if (config.checkpoint_hook) {
        config.checkpoint_hook(kind, index);
    }
    if (cancellationRequested(config)) {
        return Status::error(ErrorCode::Cancelled,
                             "render cancelled at a cooperative checkpoint");
    }
    return Status::success();
}

bool frustumCulled(const AABB& bounds, const Mat4& view_projection) {
    if (!bounds.valid()) {
        return true;
    }
    const auto row = [&view_projection](int index) {
        return Vec4(view_projection[0][index], view_projection[1][index],
                    view_projection[2][index], view_projection[3][index]);
    };
    const Vec4 row0 = row(0);
    const Vec4 row1 = row(1);
    const Vec4 row2 = row(2);
    const Vec4 row3 = row(3);
    const Vec4 planes[6] = {
        row3 + row0, row3 - row0, row3 + row1,
        row3 - row1, row3 + row2, row3 - row2,
    };
    // An AABB is culled only when every corner is outside the same plane.
    for (const Vec4& plane : planes) {
        bool all_outside = true;
        for (const Vec3& corner : bounds.corners()) {
            if (glm::dot(plane, Vec4(corner, 1.0f)) >= 0.0f) {
                all_outside = false;
                break;
            }
        }
        if (all_outside) {
            return true;
        }
    }
    return false;
}

bool intersectTriangle(const Vec3& origin, const Vec3& direction,
                       const WorldTriangle& triangle, float& distance) {
    const Vec3 edge1 = triangle.p1 - triangle.p0;
    const Vec3 edge2 = triangle.p2 - triangle.p0;
    const Vec3 p = glm::cross(direction, edge2);
    const float determinant = glm::dot(edge1, p);
    if (std::abs(determinant) < 1.0e-8f) {
        return false;
    }
    const float inverse = 1.0f / determinant;
    const Vec3 s = origin - triangle.p0;
    const float u = inverse * glm::dot(s, p);
    if (u < 0.0f || u > 1.0f) {
        return false;
    }
    const Vec3 q = glm::cross(s, edge1);
    const float v = inverse * glm::dot(direction, q);
    if (v < 0.0f || u + v > 1.0f) {
        return false;
    }
    const float t = inverse * glm::dot(edge2, q);
    if (t <= 1.0e-5f) {
        return false;
    }
    distance = t;
    return true;
}

SurfaceHit tracePixel(const std::vector<WorldTriangle>& triangles,
                      const std::vector<std::uint8_t>& allowed_nodes,
                      const Vec3& origin, const Vec3& direction) {
    SurfaceHit hit;
    for (const WorldTriangle& triangle : triangles) {
        if (triangle.node_index >= allowed_nodes.size() ||
            allowed_nodes[triangle.node_index] == 0) {
            continue;
        }
        float distance = 0.0f;
        if (!intersectTriangle(origin, direction, triangle, distance)) {
            continue;
        }
        if (distance < hit.t) {
            hit.hit = true;
            hit.t = distance;
            hit.position = origin + direction * distance;
            hit.normal = triangle.normal;
            hit.node_index = triangle.node_index;
        }
    }
    return hit;
}

BiasSample computeBias(const CascadeInfo& cascade, const RenderConfig& config,
                       float cosine) {
    const float safe_cosine = std::max(0.20f, std::min(1.0f, cosine));
    const float sine = std::sqrt(std::max(0.0f, 1.0f - safe_cosine * safe_cosine));
    const float slope = sine / safe_cosine;
    BiasSample sample;
    sample.depth_offset_texels =
        config.depth_bias + config.slope_scale * slope;
    sample.normal_offset_texels = config.normal_bias * sine;
    sample.total_offset_texels =
        sample.depth_offset_texels + sample.normal_offset_texels;
    sample.depth_offset_world =
        sample.depth_offset_texels * cascade.texel_world_size;
    sample.normal_offset_world =
        sample.normal_offset_texels * cascade.texel_world_size;
    sample.depth_bias_normalized =
        sample.depth_offset_world / std::max(cascade.depth_range, 1.0e-9f);
    sample.self_shadow_probability = clamp01(
        std::exp(-(0.65f * sample.depth_offset_texels +
                   0.35f * sample.normal_offset_texels)));
    sample.shadow_detachment_distance =
        sample.normal_offset_world + sample.depth_offset_world;
    return sample;
}

float sampleShadowMap(const CascadeShadowMap& map, const Vec3& world_position,
                      const Vec3& normal, const BiasSample& bias) {
    const Vec3 offset_position = world_position + normal * bias.normal_offset_world;
    const LightFrame& frame = map.cascade.frame;
    const Vec3 light = frame.toLightSpace(offset_position);
    const float x_range = frame.x_max - frame.x_min;
    const float y_range = frame.y_max - frame.y_min;
    const float z_range = frame.z_max - frame.z_min;
    if (x_range <= 0.0f || y_range <= 0.0f || z_range <= 0.0f) {
        return 1.0f;
    }
    const float u = (light.x - frame.x_min) / x_range;
    const float v = 1.0f - (light.y - frame.y_min) / y_range;
    if (u < 0.0f || u > 1.0f || v < 0.0f || v > 1.0f) {
        return 1.0f;
    }
    const float receiver_depth = clamp01((light.z - frame.z_min) / z_range);
    const int center_x =
        std::min(static_cast<int>(map.cascade.resolution_x) - 1,
                 std::max(0, static_cast<int>(u *
                                              static_cast<float>(
                                                  map.cascade.resolution_x))));
    const int center_y =
        std::min(static_cast<int>(map.cascade.resolution_y) - 1,
                 std::max(0, static_cast<int>(v *
                                              static_cast<float>(
                                                  map.cascade.resolution_y))));
    std::uint32_t lit = 0;
    std::uint32_t samples = 0;
    for (int oy = -1; oy <= 1; ++oy) {
        for (int ox = -1; ox <= 1; ++ox) {
            ++samples;
            const int sx = center_x + ox;
            const int sy = center_y + oy;
            if (sx < 0 || sy < 0 ||
                sx >= static_cast<int>(map.cascade.resolution_x) ||
                sy >= static_cast<int>(map.cascade.resolution_y)) {
                ++lit;
                continue;
            }
            const float stored =
                map.depth[static_cast<std::size_t>(sy) *
                              map.cascade.resolution_x +
                          static_cast<std::size_t>(sx)];
            if (receiver_depth <=
                stored + bias.depth_bias_normalized + 1.0e-6f) {
                ++lit;
            }
        }
    }
    return static_cast<float>(lit) / static_cast<float>(samples);
}

bool isFinite(float value) {
    return std::isfinite(value);
}

void appendF32LE(std::vector<std::uint8_t>& bytes, float value) {
    std::uint32_t bits = 0;
    std::memcpy(&bits, &value, sizeof(bits));
    for (int i = 0; i < 4; ++i) {
        bytes.push_back(static_cast<std::uint8_t>((bits >> (i * 8)) & 0xffu));
    }
}

std::vector<std::uint8_t> makePgm(std::uint32_t width, std::uint32_t height,
                                  const std::vector<std::uint8_t>& pixels) {
    std::ostringstream header;
    header.imbue(std::locale::classic());
    header << "P5\n" << width << " " << height << "\n255\n";
    const std::string header_text = header.str();
    std::vector<std::uint8_t> bytes(header_text.begin(), header_text.end());
    bytes.insert(bytes.end(), pixels.begin(), pixels.end());
    return bytes;
}

std::vector<std::uint8_t> makePpm(std::uint32_t width, std::uint32_t height,
                                  const std::vector<std::uint8_t>& pixels) {
    std::ostringstream header;
    header.imbue(std::locale::classic());
    header << "P6\n" << width << " " << height << "\n255\n";
    const std::string header_text = header.str();
    std::vector<std::uint8_t> bytes(header_text.begin(), header_text.end());
    bytes.insert(bytes.end(), pixels.begin(), pixels.end());
    return bytes;
}

Status writeBytesAtomic(const std::string& path,
                        const std::vector<std::uint8_t>& bytes) {
    const std::string temporary = path + ".tmp";
    {
        std::ofstream out(temporary, std::ios::binary | std::ios::trunc);
        if (!out) {
            return Status::error(ErrorCode::IoError,
                                 "cannot create output file: " + path);
        }
        out.write(reinterpret_cast<const char*>(bytes.data()),
                  static_cast<std::streamsize>(bytes.size()));
        out.flush();
        if (!out) {
            return Status::error(ErrorCode::IoError,
                                 "failed while writing output file: " + path);
        }
    }
    std::error_code ec;
    std::filesystem::rename(temporary, path, ec);
    if (ec) {
        std::filesystem::remove(path, ec);
        ec.clear();
        std::filesystem::rename(temporary, path, ec);
    }
    if (ec) {
        std::filesystem::remove(temporary, ec);
        return Status::error(ErrorCode::IoError,
                             "cannot commit output file: " + path);
    }
    return Status::success();
}

std::string jsonEscape(const std::string& input) {
    std::ostringstream out;
    for (const unsigned char c : input) {
        switch (c) {
            case '"':
                out << "\\\"";
                break;
            case '\\':
                out << "\\\\";
                break;
            case '\n':
                out << "\\n";
                break;
            case '\r':
                out << "\\r";
                break;
            case '\t':
                out << "\\t";
                break;
            default:
                if (c < 0x20) {
                    out << "\\u" << std::hex << std::setfill('0') << std::setw(4)
                        << static_cast<int>(c) << std::dec;
                } else {
                    out << static_cast<char>(c);
                }
                break;
        }
    }
    return out.str();
}

void writeFloatJson(std::ostringstream& out, float value) {
    if (!std::isfinite(value)) {
        out << "null";
    } else {
        out << formatFloat(value, 9);
    }
}

void writeVec3Json(std::ostringstream& out, const Vec3& value) {
    out << "[";
    writeFloatJson(out, value.x);
    out << ",";
    writeFloatJson(out, value.y);
    out << ",";
    writeFloatJson(out, value.z);
    out << "]";
}

void writeMat4Json(std::ostringstream& out, const Mat4& value) {
    out << "[";
    for (int column = 0; column < 4; ++column) {
        for (int row = 0; row < 4; ++row) {
            if (column != 0 || row != 0) {
                out << ",";
            }
            writeFloatJson(out, value[column][row]);
        }
    }
    out << "]";
}

void writeFrameJson(std::ostringstream& out, const LightFrame& frame) {
    out << "{\"axis_x\":";
    writeVec3Json(out, frame.axis_x);
    out << ",\"axis_y\":";
    writeVec3Json(out, frame.axis_y);
    out << ",\"axis_z\":";
    writeVec3Json(out, frame.axis_z);
    out << ",\"x_min\":";
    writeFloatJson(out, frame.x_min);
    out << ",\"x_max\":";
    writeFloatJson(out, frame.x_max);
    out << ",\"y_min\":";
    writeFloatJson(out, frame.y_min);
    out << ",\"y_max\":";
    writeFloatJson(out, frame.y_max);
    out << ",\"z_min\":";
    writeFloatJson(out, frame.z_min);
    out << ",\"z_max\":";
    writeFloatJson(out, frame.z_max);
    out << "}";
}

void writeStatusJson(std::ostringstream& out, const Status& status) {
    out << "{\"code\":\"" << errorCodeName(status.code) << "\",\"message\":\""
        << jsonEscape(status.message) << "\"";
    if (status.node_index >= 0) {
        out << ",\"node_index\":" << status.node_index;
    }
    if (!status.node_name.empty()) {
        out << ",\"node_name\":\"" << jsonEscape(status.node_name) << "\"";
    }
    out << "}";
}

std::string makeSummaryJson(const RenderSummary& summary) {
    std::ostringstream out;
    out.imbue(std::locale::classic());
    out << "{\n";
    out << "  \"pipeline\":\"shadowpipe-cpu-reference-v1\",\n";
    out << "  \"status\":\"" << errorCodeName(summary.status.code) << "\",\n";
    out << "  \"status_detail\":";
    writeStatusJson(out, summary.status);
    out << ",\n";
    out << "  \"scene_hash\":\"" << hashHex(summary.scene_hash) << "\",\n";
    out << "  \"frame_hash\":\"" << hashHex(summary.frame_hash) << "\",\n";
    out << "  \"frame\":{\"width\":" << summary.width
        << ",\"height\":" << summary.height << "},\n";
    out << "  \"camera\":{\"eye\":";
    writeVec3Json(out, summary.camera.eye);
    out << ",\"target\":";
    writeVec3Json(out, summary.camera.target);
    out << ",\"up\":";
    writeVec3Json(out, summary.camera.up);
    out << ",\"fov_y_deg\":";
    writeFloatJson(out, summary.camera.fov_y_deg);
    out << ",\"near\":";
    writeFloatJson(out, summary.camera.near_plane);
    out << ",\"far\":";
    writeFloatJson(out, summary.camera.far_plane);
    out << ",\"view_matrix\":";
    writeMat4Json(out, summary.cascade_table.view);
    out << ",\"projection_matrix\":";
    writeMat4Json(out, summary.cascade_table.projection);
    out << "},\n";
    out << "  \"light\":{\"direction\":";
    writeVec3Json(out, summary.light.direction);
    out << "},\n";
    out << "  \"bias_parameters\":{\"depth_bias\":";
    writeFloatJson(out, summary.bias.depth_bias);
    out << ",\"normal_bias\":";
    writeFloatJson(out, summary.bias.normal_bias);
    out << ",\"slope_scale\":";
    writeFloatJson(out, summary.bias.slope_scale);
    out << ",\"max_bias_texels\":";
    writeFloatJson(out, summary.bias.max_bias_texels);
    out << ",\"switch_tolerance\":";
    writeFloatJson(out, summary.switch_tolerance);
    out << "},\n";
    out << "  \"frustum_culled_count\":" << summary.frustum_culled_count
        << ",\n";
    out << "  \"occluded_count\":" << summary.occluded_object_count << ",\n";
    out << "  \"occlusion_ambiguous_count\":" << summary.occlusion_ambiguous_count
        << ",\n";
    out << "  \"visible_object_count\":" << summary.visible_object_count << ",\n";
    out << "  \"mesh_node_count\":" << summary.mesh_node_count << ",\n";
    out << "  \"cascade_switch_count\":" << summary.cascade_switch_count << ",\n";
    out << "  \"cascade_switch_factor_delta_upper_bound\":";
    writeFloatJson(out, summary.cascade_switch_factor_delta_upper_bound);
    out << ",\n";
    out << "  \"shadow_factor_delta_upper_bound\":";
    writeFloatJson(out, summary.shadow_factor_delta_upper_bound);
    out << ",\n";

    out << "  \"cascades\":[";
    for (std::size_t i = 0; i < summary.cascade_table.cascades.size(); ++i) {
        const CascadeInfo& cascade = summary.cascade_table.cascades[i];
        if (i != 0) {
            out << ",";
        }
        out << "\n    {\"index\":" << cascade.index
            << ",\"near\":";
        writeFloatJson(out, cascade.near_distance);
        out << ",\"far\":";
        writeFloatJson(out, cascade.far_distance);
        out << ",\"resolution_x\":" << cascade.resolution_x
            << ",\"resolution_y\":" << cascade.resolution_y
            << ",\"texel_world_size\":";
        writeFloatJson(out, cascade.texel_world_size);
        out << ",\"depth_range\":";
        writeFloatJson(out, cascade.depth_range);
        out << ",\"depth_quantization_step\":";
        writeFloatJson(out, cascade.depth_quantization_step);
        out << ",\"max_depth_quantization_error\":";
        writeFloatJson(out, cascade.max_depth_quantization_error);
        out << ",\"frame\":";
        writeFrameJson(out, cascade.frame);
        out << ",\"light_view_projection\":";
        writeMat4Json(out, cascade.light_view_projection);
        out << "}";
    }
    out << "\n  ],\n";

    out << "  \"cascade_boundaries\":[";
    for (std::size_t i = 0; i < summary.cascade_table.boundaries.size(); ++i) {
        const CascadeBoundaryStats& boundary =
            summary.cascade_table.boundaries[i];
        if (i != 0) {
            out << ",";
        }
        out << "\n    {\"near_cascade\":" << boundary.near_cascade
            << ",\"far_cascade\":" << boundary.far_cascade
            << ",\"near_texel_world_size\":";
        writeFloatJson(out, boundary.near_texel_world_size);
        out << ",\"far_texel_world_size\":";
        writeFloatJson(out, boundary.far_texel_world_size);
        out << ",\"texel_coverage_difference\":";
        writeFloatJson(out, boundary.texel_coverage_difference);
        out << ",\"relative_texel_coverage_difference\":";
        writeFloatJson(out, boundary.relative_texel_coverage_difference);
        out << "}";
    }
    out << "\n  ],\n";

    out << "  \"pyramid\":{\"rebuilt\":"
        << (summary.pyramid_rebuild.rebuilt ? "true" : "false")
        << ",\"reason\":\"" << jsonEscape(summary.pyramid_rebuild.reason)
        << "\",\"old_new_level_range_difference_max\":";
    writeFloatJson(out, summary.pyramid_rebuild.old_new_level_range_difference_max);
    out << ",\"max_quantization_error\":";
    writeFloatJson(out, summary.pyramid_rebuild.max_quantization_error);
    out << ",\"levels\":[";
    for (std::size_t i = 0; i < summary.pyramid_levels.size(); ++i) {
        const PyramidLevelMeta& level = summary.pyramid_levels[i];
        if (i != 0) {
            out << ",";
        }
        out << "{\"index\":" << level.index << ",\"width\":" << level.width
            << ",\"height\":" << level.height
            << ",\"quantization_bits\":" << level.quantization_bits
            << ",\"depth_min\":";
        writeFloatJson(out, level.depth_min);
        out << ",\"depth_max\":";
        writeFloatJson(out, level.depth_max);
        out << ",\"quantization_step\":";
        writeFloatJson(out, level.quantization_step);
        out << ",\"max_quantization_error\":";
        writeFloatJson(out, level.max_quantization_error);
        out << "}";
    }
    out << "],\"level_deltas\":[";
    for (std::size_t i = 0; i < summary.pyramid_rebuild.level_deltas.size(); ++i) {
        const LevelRangeDelta& delta = summary.pyramid_rebuild.level_deltas[i];
        if (i != 0) {
            out << ",";
        }
        out << "{\"level\":" << delta.level << ",\"old_min\":";
        writeFloatJson(out, delta.old_min);
        out << ",\"new_min\":";
        writeFloatJson(out, delta.new_min);
        out << ",\"old_max\":";
        writeFloatJson(out, delta.old_max);
        out << ",\"new_max\":";
        writeFloatJson(out, delta.new_max);
        out << ",\"min_difference\":";
        writeFloatJson(out, delta.min_difference);
        out << ",\"max_difference\":";
        writeFloatJson(out, delta.max_difference);
        out << ",\"old_quantization_step\":";
        writeFloatJson(out, delta.old_quantization_step);
        out << ",\"new_quantization_step\":";
        writeFloatJson(out, delta.new_quantization_step);
        out << ",\"quantization_step_changed\":"
            << (delta.quantization_step_changed ? "true" : "false") << "}";
    }
    out << "]},\n";

    out << "  \"occlusion_queries\":[";
    for (std::size_t i = 0; i < summary.occlusion_nodes.size(); ++i) {
        const OcclusionNodeReport& node = summary.occlusion_nodes[i];
        if (i != 0) {
            out << ",";
        }
        out << "\n    {\"node_index\":" << node.node_index
            << ",\"node\":\"" << jsonEscape(node.node_name)
            << "\",\"frustum_culled\":"
            << (node.frustum_culled ? "true" : "false")
            << ",\"verdict\":\"" << occlusionVerdictName(node.verdict)
            << "\",\"ambiguous\":" << (node.ambiguous ? "true" : "false")
            << ",\"status\":\"" << errorCodeName(node.status.code)
            << "\",\"margin_lower\":";
        writeFloatJson(out, node.margin_lower);
        out << ",\"margin_upper\":";
        writeFloatJson(out, node.margin_upper);
        out << ",\"quantization_error\":";
        writeFloatJson(out, node.quantization_error);
        out << ",\"world_bounds\":{\"min\":";
        writeVec3Json(out, node.world_bounds.min);
        out << ",\"max\":";
        writeVec3Json(out, node.world_bounds.max);
        out << "},\"levels\":[";
        for (std::size_t j = 0; j < node.levels.size(); ++j) {
            const OcclusionLevelResult& level = node.levels[j];
            if (j != 0) {
                out << ",";
            }
            out << "{\"level\":" << level.level << ",\"verdict\":\""
                << occlusionVerdictName(level.verdict) << "\",\"margin_lower\":";
            writeFloatJson(out, level.margin_lower);
            out << ",\"margin_upper\":";
            writeFloatJson(out, level.margin_upper);
            out << ",\"quantization_error\":";
            writeFloatJson(out, level.quantization_error);
            out << ",\"sampled_texels\":" << level.sampled_texels
                << ",\"covered_texels\":" << level.covered_texels << "}";
        }
        out << "]}";
    }
    out << "\n  ],\n";

    out << "  \"bias_error_metrics\":{";
    float max_self = 0.0f;
    float max_detach = 0.0f;
    for (const BiasErrorMetric& metric : summary.bias_metrics) {
        max_self = std::max(max_self, metric.max_self_shadow_probability);
        max_detach = std::max(max_detach, metric.max_shadow_detachment_distance);
    }
    out << "\"applied_once\":true,\"max_self_shadow_probability\":";
    writeFloatJson(out, max_self);
    out << ",\"max_shadow_detachment_distance\":";
    writeFloatJson(out, max_detach);
    out << ",\"cascades\":[";
    for (std::size_t i = 0; i < summary.bias_metrics.size(); ++i) {
        const BiasErrorMetric& metric = summary.bias_metrics[i];
        if (i != 0) {
            out << ",";
        }
        out << "\n    {\"cascade\":" << metric.cascade
            << ",\"texel_world_size\":";
        writeFloatJson(out, metric.texel_world_size);
        out << ",\"depth_offset_world\":";
        writeFloatJson(out, metric.depth_offset_world);
        out << ",\"normal_offset_world\":";
        writeFloatJson(out, metric.normal_offset_world);
        out << ",\"depth_bias_normalized\":";
        writeFloatJson(out, metric.depth_bias_normalized);
        out << ",\"estimated_self_shadow_probability\":";
        writeFloatJson(out, metric.self_shadow_probability);
        out << ",\"estimated_shadow_detachment_distance\":";
        writeFloatJson(out, metric.shadow_detachment_distance);
        out << ",\"minimum_observed_depth_offset_world\":";
        writeFloatJson(out, metric.min_depth_offset_world);
        out << ",\"maximum_observed_depth_offset_world\":";
        writeFloatJson(out, metric.max_depth_offset_world);
        out << ",\"maximum_self_shadow_probability\":";
        writeFloatJson(out, metric.max_self_shadow_probability);
        out << ",\"maximum_shadow_detachment_distance\":";
        writeFloatJson(out, metric.max_shadow_detachment_distance);
        out << ",\"sample_count\":" << metric.sample_count
            << ",\"applied_once\":true}";
    }
    out << "]},\n";

    out << "  \"switch_events\":[";
    for (std::size_t i = 0; i < summary.switch_events.size(); ++i) {
        const CascadeSwitchEvent& event = summary.switch_events[i];
        if (i != 0) {
            out << ",";
        }
        out << "\n    {\"pixel\":[" << event.pixel_x << "," << event.pixel_y
            << "],\"node_index\":" << event.node_index
            << ",\"selected_cascade\":" << event.selected_cascade
            << ",\"alternate_cascade\":" << event.alternate_cascade
            << ",\"view_depth\":";
        writeFloatJson(out, event.view_depth);
        out << ",\"selected_shadow_factor\":";
        writeFloatJson(out, event.selected_shadow_factor);
        out << ",\"alternate_shadow_factor\":";
        writeFloatJson(out, event.alternate_shadow_factor);
        out << ",\"factor_delta\":";
        writeFloatJson(out, event.factor_delta);
        out << ",\"boundary_distance\":";
        writeFloatJson(out, event.boundary_distance);
        out << "}";
    }
    out << "],\n";

    out << "  \"snapshot_version\":{\"changed\":"
        << (summary.snapshot_diff.rerenderRequired() ? "true" : "false")
        << ",\"scene_changed\":"
        << (summary.snapshot_diff.scene_changed ? "true" : "false")
        << ",\"view_changed\":"
        << (summary.snapshot_diff.view_changed ? "true" : "false")
        << ",\"light_changed\":"
        << (summary.snapshot_diff.light_changed ? "true" : "false")
        << ",\"cascade_table_changed\":"
        << (summary.snapshot_diff.cascade_table_changed ? "true" : "false")
        << ",\"bias_changed\":"
        << (summary.snapshot_diff.bias_changed ? "true" : "false") << "},\n";

    out << "  \"artifact_hashes\":{\"shadow_factor.pgm\":\""
        << hashHex(summary.artifact_hashes.shadow_factor_pgm)
        << "\",\"shadow_factor.f32\":\""
        << hashHex(summary.artifact_hashes.shadow_factor_f32)
        << "\",\"visibility_mask.pgm\":\""
        << hashHex(summary.artifact_hashes.visibility_mask_pgm)
        << "\",\"error_heatmap.ppm\":\""
        << hashHex(summary.artifact_hashes.error_heatmap_ppm)
        << "\",\"cascade_depth_maps\":\""
        << hashHex(summary.artifact_hashes.cascade_depth_maps)
        << "\"}\n";
    out << "}\n";
    return out.str();
}

Status saveRenderResult(const OutputPaths& paths, RenderResult& result) {
    std::error_code ec;
    std::filesystem::create_directories(paths.directory, ec);
    if (ec) {
        return Status::error(ErrorCode::IoError,
                             "cannot create output directory: " + paths.directory);
    }

    std::vector<std::uint8_t> shadow_pgm(result.shadow_factor.size());
    for (std::size_t i = 0; i < result.shadow_factor.size(); ++i) {
        shadow_pgm[i] = static_cast<std::uint8_t>(
            std::lround(clamp01(result.shadow_factor[i]) * 255.0f));
    }
    std::vector<std::uint8_t> shadow_pgm_bytes =
        makePgm(result.summary.width, result.summary.height, shadow_pgm);
    result.summary.artifact_hashes.shadow_factor_pgm =
        hashBytes(shadow_pgm_bytes.data(), shadow_pgm_bytes.size());
    Status status = writeBytesAtomic(joinPath(paths.directory, "shadow_factor.pgm"),
                                     shadow_pgm_bytes);
    if (!status.ok()) {
        return status;
    }

    std::vector<std::uint8_t> shadow_f32_bytes;
    shadow_f32_bytes.reserve(result.shadow_factor.size() * 4);
    for (const float value : result.shadow_factor) {
        appendF32LE(shadow_f32_bytes, value);
    }
    result.summary.artifact_hashes.shadow_factor_f32 =
        hashBytes(shadow_f32_bytes.data(), shadow_f32_bytes.size());
    status =
        writeBytesAtomic(joinPath(paths.directory, "shadow_factor.f32"),
                         shadow_f32_bytes);
    if (!status.ok()) {
        return status;
    }

    std::vector<std::uint8_t> mask_pgm_bytes =
        makePgm(result.summary.width, result.summary.height,
                result.visibility_mask);
    result.summary.artifact_hashes.visibility_mask_pgm =
        hashBytes(mask_pgm_bytes.data(), mask_pgm_bytes.size());
    status = writeBytesAtomic(
        joinPath(paths.directory, "visibility_mask.pgm"), mask_pgm_bytes);
    if (!status.ok()) {
        return status;
    }

    if (result.write_heatmap) {
        std::vector<std::uint8_t> heatmap(result.error_map.size() * 3, 0);
        float max_error = 0.0f;
        for (const float value : result.error_map) {
            max_error = std::max(max_error, value);
        }
        const float normalizer = std::max(max_error, 1.0e-6f);
        for (std::size_t i = 0; i < result.error_map.size(); ++i) {
            const float intensity = clamp01(result.error_map[i] / normalizer);
            heatmap[i * 3] =
                static_cast<std::uint8_t>(std::lround(255.0f * intensity));
            heatmap[i * 3 + 1] = static_cast<std::uint8_t>(
                std::lround(96.0f * intensity * intensity));
            heatmap[i * 3 + 2] = 0;
        }
        std::vector<std::uint8_t> heatmap_bytes =
            makePpm(result.summary.width, result.summary.height, heatmap);
        result.summary.artifact_hashes.error_heatmap_ppm =
            hashBytes(heatmap_bytes.data(), heatmap_bytes.size());
        status =
            writeBytesAtomic(joinPath(paths.directory, "error_heatmap.ppm"),
                             heatmap_bytes);
        if (!status.ok()) {
            return status;
        }
    }

    Hasher cascade_hasher;
    for (const CascadeShadowMap& map : result.shadow_maps) {
        std::vector<std::uint8_t> f32;
        f32.reserve(map.depth.size() * 4);
        std::vector<std::uint8_t> pgm_pixels(map.depth.size());
        for (std::size_t i = 0; i < map.depth.size(); ++i) {
            appendF32LE(f32, map.depth[i]);
            pgm_pixels[i] = static_cast<std::uint8_t>(
                std::lround(clamp01(map.depth[i]) * 255.0f));
        }
        std::vector<std::uint8_t> pgm =
            makePgm(map.cascade.resolution_x, map.cascade.resolution_y,
                    pgm_pixels);
        const std::string prefix = "cascade_" +
                                   std::to_string(map.cascade.index) +
                                   "_shadow_depth";
        status = writeBytesAtomic(joinPath(paths.directory, prefix + ".f32"), f32);
        if (!status.ok()) {
            return status;
        }
        status = writeBytesAtomic(joinPath(paths.directory, prefix + ".pgm"), pgm);
        if (!status.ok()) {
            return status;
        }
        cascade_hasher.string(prefix);
        cascade_hasher.u64(hashBytes(f32.data(), f32.size()));
        cascade_hasher.u64(hashBytes(pgm.data(), pgm.size()));
    }
    result.summary.artifact_hashes.cascade_depth_maps = cascade_hasher.value();

    Hasher frame_hasher;
    frame_hasher.u32(result.summary.width);
    frame_hasher.u32(result.summary.height);
    for (const float value : result.shadow_factor) {
        frame_hasher.f32(value);
    }
    for (const std::uint8_t value : result.visibility_mask) {
        frame_hasher.u8(value);
    }
    for (const float value : result.error_map) {
        frame_hasher.f32(value);
    }
    result.summary.frame_hash = frame_hasher.value();

    result.snapshot.image_width = result.summary.width;
    result.snapshot.image_height = result.summary.height;

    const std::string summary_json = makeSummaryJson(result.summary);
    const std::vector<std::uint8_t> summary_bytes(summary_json.begin(),
                                                  summary_json.end());
    status = writeBytesAtomic(joinPath(paths.directory, "summary.json"),
                              summary_bytes);
    if (!status.ok()) {
        return status;
    }
    result.summary.artifact_hashes.summary_json =
        hashBytes(summary_bytes.data(), summary_bytes.size());
    result.snapshot.hashes = result.summary.artifact_hashes;

    std::string snapshot_path = paths.snapshot_file;
    if (snapshot_path.empty()) {
        snapshot_path = joinPath(paths.directory, "snapshot.bin");
    }
    status = writeSnapshotFile(snapshot_path, result.snapshot);
    if (!status.ok()) {
        return status;
    }
    result.outputs_written = true;
    return Status::success();
}

}  // namespace

RenderResult RenderService::render(const Scene& scene, const RenderConfig& config,
                                   const OutputPaths& paths,
                                   const SnapshotData* previous_snapshot) {
    RenderResult result;
    result.write_heatmap = config.write_heatmap;
    result.summary.camera = config.camera;
    result.summary.light = config.light;
    result.summary.bias = {config.depth_bias, config.normal_bias,
                           config.slope_scale, config.max_bias_texels};
    result.summary.switch_tolerance = config.switch_tolerance;
    result.summary.width = config.width;
    result.summary.height = config.height;
    result.summary.scene_hash =
        scene.content_hash != 0 ? scene.content_hash : computeSceneHash(scene);
    result.summary.status = Status::success();
    result.status = Status::success();

    {
        std::lock_guard<std::mutex> lock(mutex_);
        if (active_scenes_.count(result.summary.scene_hash) != 0) {
            result.status = Status::error(
                ErrorCode::Busy,
                "a render task for this scene content hash is already running");
            result.summary.status = result.status;
            return result;
        }
        active_scenes_.insert(result.summary.scene_hash);
    }

    struct ActiveGuard {
        RenderService& service;
        std::uint64_t hash;
        ~ActiveGuard() {
            std::lock_guard<std::mutex> lock(service.mutex_);
            service.active_scenes_.erase(hash);
        }
    } active_guard{*this, result.summary.scene_hash};

    try {
    Status status = validateRenderConfig(config);
    if (!status.ok()) {
        result.status = status;
        result.summary.status = status;
        if (status.code == ErrorCode::BiasInvalid) {
            writePipelineErrorSummary(paths.directory, status);
        }
        return result;
    }

    status = checkpoint(config, Checkpoint::BeforeCascade, 0);
    if (!status.ok()) {
        result.status = status;
        result.summary.status = status;
        return result;
    }

    CascadeTable cascade_table;
    status = buildCascadeTable(scene, config, cascade_table);
    if (!status.ok()) {
        result.status = status;
        result.summary.status = status;
        return result;
    }
    result.summary.cascade_table = cascade_table;
    if (previous_snapshot != nullptr) {
        result.summary.snapshot_loaded = true;
        result.summary.snapshot_diff =
            compareSnapshotToFrame(*previous_snapshot, result.summary.scene_hash,
                                   config, cascade_table);
    }

    constexpr float k_max_supported_cosine_slope = 4.8989795f;  // tan(acos(0.2))
    for (const CascadeInfo& cascade : cascade_table.cascades) {
        (void)cascade;
        const float required_texels =
            config.depth_bias + config.slope_scale * k_max_supported_cosine_slope +
            config.normal_bias;
        if (!isFinite(required_texels) || required_texels < 0.0f ||
            required_texels > config.max_bias_texels) {
            status = Status::error(
                ErrorCode::BiasInvalid,
                "computed cascade bias exceeds max_bias_texels; rendering aborted");
            result.status = status;
            result.summary.status = status;
            writePipelineErrorSummary(paths.directory, status);
            return result;
        }
    }

    OccluderPyramid pyramid;
    const PyramidMetadata* previous_pyramid =
        previous_snapshot != nullptr ? &previous_snapshot->pyramid : nullptr;
    status = buildOccluderPyramid(scene, config, previous_pyramid, pyramid);
    if (!status.ok()) {
        result.status = status;
        result.summary.status = status;
        return result;
    }
    result.summary.pyramid_rebuild = pyramid.rebuild_report;
    for (const PyramidLevel& level : pyramid.levels) {
        result.summary.pyramid_levels.push_back(level.meta);
    }

    for (std::size_t i = 0; i < scene.nodes.size(); ++i) {
        const Node& node = scene.nodes[i];
        if (node.mesh_index == kNoMesh) {
            continue;
        }
        OcclusionNodeReport report;
        report.node_index = static_cast<std::uint32_t>(i);
        report.node_name = node.name;
        report.world_bounds =
            node.content_bounds.valid() ? node.content_bounds : node.declared_bounds;
        report.frustum_culled =
            frustumCulled(report.world_bounds, cascade_table.view_projection);
        if (report.frustum_culled) {
            report.verdict = OcclusionVerdict::Visible;
            report.status = Status::success();
            ++result.summary.frustum_culled_count;
        } else {
            const OcclusionQueryResult query =
                queryOcclusion(pyramid, report.world_bounds,
                               static_cast<std::uint32_t>(i));
            report.verdict = query.verdict;
            report.ambiguous = query.ambiguous;
            report.status = query.status;
            report.margin_lower = query.margin_lower;
            report.margin_upper = query.margin_upper;
            report.quantization_error = query.quantization_error;
            report.levels = query.levels;
            if (query.verdict == OcclusionVerdict::Occluded) {
                ++result.summary.occluded_object_count;
            } else if (query.verdict == OcclusionVerdict::Ambiguous) {
                ++result.summary.occlusion_ambiguous_count;
            } else {
                ++result.summary.visible_object_count;
            }
        }
        result.summary.occlusion_nodes.push_back(report);
    }
    result.summary.mesh_node_count =
        static_cast<std::uint32_t>(result.summary.occlusion_nodes.size());

    std::vector<std::uint8_t> camera_allowed(scene.nodes.size(), 0);
    std::vector<std::uint8_t> caster_allowed(scene.nodes.size(), 0);
    for (const OcclusionNodeReport& report : result.summary.occlusion_nodes) {
        const bool frustum_visible = !report.frustum_culled;
        camera_allowed[report.node_index] =
            frustum_visible &&
                    (report.verdict == OcclusionVerdict::Visible ||
                     report.verdict == OcclusionVerdict::Ambiguous)
                ? 1
                : 0;
        caster_allowed[report.node_index] =
            report.verdict == OcclusionVerdict::Occluded ? 0 : 1;
    }

    const std::vector<WorldTriangle> triangles = buildWorldTriangles(scene);
    for (const CascadeInfo& cascade : cascade_table.cascades) {
        status = checkpoint(config, Checkpoint::BeforeCascade, cascade.index);
        if (!status.ok()) {
            result.status = status;
            result.summary.status = status;
            return result;
        }
        CascadeShadowMap map;
        map.cascade = cascade;
        rasterizeDepth(triangles, cascade.frame, cascade.resolution_x,
                       cascade.resolution_y, map.depth, &caster_allowed);
        Hasher map_hasher;
        for (const float value : map.depth) {
            map_hasher.f32(value);
        }
        map.content_hash = map_hasher.value();
        result.shadow_maps.push_back(std::move(map));
        status = checkpoint(config, Checkpoint::AfterCascade, cascade.index);
        if (!status.ok()) {
            result.status = status;
            result.summary.status = status;
            return result;
        }
    }
    status = checkpoint(config, Checkpoint::BeforeRaster, 0);
    if (!status.ok()) {
        result.status = status;
        result.summary.status = status;
        return result;
    }

    result.shadow_factor.assign(
        static_cast<std::size_t>(config.width) * config.height, 1.0f);
    result.visibility_mask.assign(
        static_cast<std::size_t>(config.width) * config.height, 0);
    result.error_map.assign(
        static_cast<std::size_t>(config.width) * config.height, 0.0f);

    for (const CascadeInfo& cascade : cascade_table.cascades) {
        const BiasSample representative =
            computeBias(cascade, config, 0.8f);
        BiasErrorMetric metric;
        metric.cascade = cascade.index;
        metric.texel_world_size = cascade.texel_world_size;
        metric.depth_offset_world = representative.depth_offset_world;
        metric.normal_offset_world = representative.normal_offset_world;
        metric.depth_bias_normalized =
            representative.depth_bias_normalized;
        metric.self_shadow_probability =
            representative.self_shadow_probability;
        metric.shadow_detachment_distance =
            representative.shadow_detachment_distance;
        metric.min_depth_offset_world =
            representative.depth_offset_world;
        metric.max_depth_offset_world =
            representative.depth_offset_world;
        metric.max_self_shadow_probability =
            representative.self_shadow_probability;
        metric.max_shadow_detachment_distance =
            representative.shadow_detachment_distance;
        result.summary.bias_metrics.push_back(metric);
    }

    const float aspect =
        static_cast<float>(config.width) / static_cast<float>(config.height);
    const float tan_half_fov =
        std::tan(glm::radians(config.camera.fov_y_deg) * 0.5f);
    const Vec3 to_light = -glm::normalize(config.light.direction);

    for (std::uint32_t y = 0; y < config.height; ++y) {
        for (std::uint32_t x = 0; x < config.width; ++x) {
            const float ndc_x =
                2.0f * (static_cast<float>(x) + 0.5f) /
                    static_cast<float>(config.width) -
                1.0f;
            const float ndc_y =
                1.0f - 2.0f * (static_cast<float>(y) + 0.5f) /
                           static_cast<float>(config.height);
            const Vec3 ray = glm::normalize(
                cascade_table.camera_forward +
                cascade_table.camera_right *
                    (ndc_x * tan_half_fov * aspect) +
                cascade_table.camera_up * (ndc_y * tan_half_fov));
            SurfaceHit hit = tracePixel(triangles, camera_allowed,
                                        config.camera.eye, ray);
            if (!hit.hit) {
                continue;
            }
            const std::size_t pixel =
                static_cast<std::size_t>(y) * config.width + x;
            if (glm::dot(hit.normal, ray) > 0.0f) {
                hit.normal = -hit.normal;
            }
            const auto report_it = std::find_if(
                result.summary.occlusion_nodes.begin(),
                result.summary.occlusion_nodes.end(),
                [&hit](const OcclusionNodeReport& report) {
                    return report.node_index == hit.node_index;
                });
            if (report_it != result.summary.occlusion_nodes.end() &&
                report_it->ambiguous) {
                result.visibility_mask[pixel] = 128;
            } else {
                result.visibility_mask[pixel] = 255;
            }

            const float view_depth = glm::dot(
                hit.position - config.camera.eye,
                cascade_table.camera_forward);
            const float cosine =
                std::max(0.0f, glm::dot(hit.normal, to_light));
            if (cosine <= 1.0e-5f) {
                result.shadow_factor[pixel] = 1.0f;
                continue;
            }
            const float depth_jitter =
                config.normal_bias *
                cascade_table.cascades.front().texel_world_size *
                (1.0f - std::min(1.0f, cosine));
            const std::uint32_t selected =
                selectCascadeForDepth(cascade_table, view_depth + depth_jitter);
            const CascadeShadowMap& selected_map =
                result.shadow_maps[selected];
            const BiasSample selected_bias =
                computeBias(selected_map.cascade, config, cosine);
            const float selected_factor =
                sampleShadowMap(selected_map, hit.position, hit.normal,
                                selected_bias);
            const BiasSample no_bias;
            const float reference_factor =
                sampleShadowMap(selected_map, hit.position, hit.normal, no_bias);
            float pixel_error =
                std::abs(selected_factor - reference_factor);
            result.shadow_factor[pixel] = selected_factor;

            BiasErrorMetric& metric = result.summary.bias_metrics[selected];
            if (metric.sample_count == 0) {
                metric.min_depth_offset_world =
                    selected_bias.depth_offset_world;
                metric.max_depth_offset_world =
                    selected_bias.depth_offset_world;
            } else {
                metric.min_depth_offset_world =
                    std::min(metric.min_depth_offset_world,
                             selected_bias.depth_offset_world);
                metric.max_depth_offset_world =
                    std::max(metric.max_depth_offset_world,
                             selected_bias.depth_offset_world);
            }
            ++metric.sample_count;
            metric.max_self_shadow_probability =
                std::max(metric.max_self_shadow_probability,
                         selected_bias.self_shadow_probability);
            metric.max_shadow_detachment_distance =
                std::max(metric.max_shadow_detachment_distance,
                         selected_bias.shadow_detachment_distance);

            const CascadeInfo& cascade = selected_map.cascade;
            int alternate = -1;
            float boundary_distance = kInfinity;
            const float band =
                std::max(2.0f * cascade.texel_world_size,
                         (cascade.far_distance - cascade.near_distance) * 0.05f);
            if (selected > 0 &&
                std::abs(view_depth - cascade.near_distance) <= band) {
                alternate = static_cast<int>(selected - 1);
                boundary_distance = std::abs(view_depth - cascade.near_distance);
            } else if (selected + 1 < cascade_table.cascades.size() &&
                       std::abs(view_depth - cascade.far_distance) <= band) {
                alternate = static_cast<int>(selected + 1);
                boundary_distance = std::abs(view_depth - cascade.far_distance);
            }
            if (alternate >= 0) {
                const CascadeShadowMap& alternate_map =
                    result.shadow_maps[static_cast<std::size_t>(alternate)];
                const BiasSample alternate_bias =
                    computeBias(alternate_map.cascade, config, cosine);
                const float alternate_factor =
                    sampleShadowMap(alternate_map, hit.position, hit.normal,
                                    alternate_bias);
                const float delta =
                    std::abs(selected_factor - alternate_factor);
                pixel_error = std::max(pixel_error, delta);
                if (delta > config.switch_tolerance) {
                    ++result.summary.cascade_switch_count;
                    result.summary.cascade_switch_factor_delta_upper_bound =
                        std::max(result.summary
                                     .cascade_switch_factor_delta_upper_bound,
                                 delta);
                    if (result.summary.switch_events.size() < 64) {
                        CascadeSwitchEvent event;
                        event.pixel_x = x;
                        event.pixel_y = y;
                        event.node_index = hit.node_index;
                        event.selected_cascade = selected;
                        event.alternate_cascade =
                            static_cast<std::uint32_t>(alternate);
                        event.view_depth = view_depth;
                        event.selected_shadow_factor = selected_factor;
                        event.alternate_shadow_factor = alternate_factor;
                        event.factor_delta = delta;
                        event.boundary_distance = boundary_distance;
                        result.summary.switch_events.push_back(event);
                    }
                }
            }
            result.error_map[pixel] = pixel_error;
            result.summary.shadow_factor_delta_upper_bound =
                std::max(result.summary.shadow_factor_delta_upper_bound,
                         pixel_error);
        }
    }

    status = checkpoint(config, Checkpoint::BeforeCommit, 0);
    if (!status.ok()) {
        result.status = status;
        result.summary.status = status;
        return result;
    }

    result.snapshot.format_version = kSnapshotFormatVersion;
    result.snapshot.scene_hash = result.summary.scene_hash;
    result.snapshot.camera = config.camera;
    result.snapshot.light = config.light;
    result.snapshot.cascade_table = cascade_table;
    result.snapshot.bias = {config.depth_bias, config.normal_bias,
                            config.slope_scale, config.max_bias_texels};
    result.snapshot.switch_tolerance = config.switch_tolerance;
    result.snapshot.pyramid = pyramid.metadata();
    result.snapshot.image_width = config.width;
    result.snapshot.image_height = config.height;
    result.snapshot.hashes = {};

    status = saveRenderResult(paths, result);
    if (!status.ok()) {
        result.status = status;
        result.summary.status = status;
        writePipelineErrorSummary(paths.directory, status);
        return result;
    }
    result.snapshot.hashes = result.summary.artifact_hashes;
    result.status = Status::success();
    result.summary.status = Status::success();
    return result;
    } catch (const std::exception& exception) {
        result.status = Status::error(
            ErrorCode::InternalError,
            std::string("render aborted by exception: ") + exception.what());
        result.summary.status = result.status;
        return result;
    } catch (...) {
        result.status = Status::error(
            ErrorCode::InternalError,
            "render aborted by an unknown exception");
        result.summary.status = result.status;
        return result;
    }
}

bool RenderService::isBusy(std::uint64_t scene_hash) const {
    std::lock_guard<std::mutex> lock(mutex_);
    return active_scenes_.count(scene_hash) != 0;
}

Status clearRenderOutputs(const std::string& directory,
                          const std::string& preserve_snapshot_path) {
    std::error_code ec;
    if (!std::filesystem::exists(directory, ec)) {
        return Status::success();
    }
    if (ec || !std::filesystem::is_directory(directory, ec)) {
        return Status::error(ErrorCode::IoError,
                             "output path is not a directory: " + directory);
    }
    const std::string preserved =
        preserve_snapshot_path.empty()
            ? std::string()
            : std::filesystem::absolute(preserve_snapshot_path, ec).lexically_normal().string();
    for (const auto& entry : std::filesystem::directory_iterator(directory, ec)) {
        if (ec) {
            return Status::error(ErrorCode::IoError,
                                 "cannot enumerate output directory: " + directory);
        }
        if (!entry.is_regular_file(ec)) {
            continue;
        }
        const std::string filename = entry.path().filename().string();
        const bool generated =
            filename == "summary.json" || filename == "shadow_factor.pgm" ||
            filename == "shadow_factor.f32" ||
            filename == "visibility_mask.pgm" ||
            filename == "error_heatmap.ppm" ||
            filename.rfind("cascade_", 0) == 0 ||
            (filename.size() > 4 &&
             filename.substr(filename.size() - 4) == ".tmp");
        if (!generated) {
            continue;
        }
        if (!preserved.empty()) {
            const std::string candidate =
                std::filesystem::absolute(entry.path(), ec).lexically_normal().string();
            if (ec || candidate == preserved) {
                continue;
            }
        }
        std::filesystem::remove(entry.path(), ec);
        if (ec) {
            return Status::error(ErrorCode::IoError,
                                 "cannot remove old output file: " +
                                     entry.path().string());
        }
    }
    return Status::success();
}

Status writePipelineErrorSummary(const std::string& directory,
                                 const Status& status) {
    std::error_code ec;
    std::filesystem::create_directories(directory, ec);
    if (ec) {
        return Status::error(ErrorCode::IoError,
                             "cannot create output directory: " + directory);
    }
    std::ostringstream out;
    out.imbue(std::locale::classic());
    out << "{\n  \"pipeline\":\"shadowpipe-cpu-reference-v1\",\n";
    out << "  \"status\":\"" << errorCodeName(status.code) << "\",\n";
    out << "  \"render_started\":false,\n";
    out << "  \"error\":";
    writeStatusJson(out, status);
    out << "\n}\n";
    const std::string text = out.str();
    std::vector<std::uint8_t> bytes(text.begin(), text.end());
    return writeBytesAtomic(joinPath(directory, "summary.json"), bytes);
}

}  // namespace shadowpipe

