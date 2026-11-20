#include "shadowpipe/cascades.hpp"

#include <algorithm>
#include <cmath>

#include <glm/gtc/matrix_transform.hpp>

namespace shadowpipe {
namespace {

Mat4 lightViewProjectionMatrix(const LightFrame& frame) {
    Mat4 view(1.0f);
    view[0][0] = frame.axis_x.x;
    view[1][0] = frame.axis_x.y;
    view[2][0] = frame.axis_x.z;
    view[3][0] = 0.0f;
    view[0][1] = frame.axis_y.x;
    view[1][1] = frame.axis_y.y;
    view[2][1] = frame.axis_y.z;
    view[3][1] = 0.0f;
    view[0][2] = frame.axis_z.x;
    view[1][2] = frame.axis_z.y;
    view[2][2] = frame.axis_z.z;
    view[3][2] = 0.0f;

    const float x_range = frame.x_max - frame.x_min;
    const float y_range = frame.y_max - frame.y_min;
    const float z_range = frame.z_max - frame.z_min;
    Mat4 projection(1.0f);
    projection[0][0] = 2.0f / x_range;
    projection[3][0] = -(frame.x_max + frame.x_min) / x_range;
    projection[1][1] = 2.0f / y_range;
    projection[3][1] = -(frame.y_max + frame.y_min) / y_range;
    projection[2][2] = 2.0f / z_range;
    projection[3][2] = -(frame.z_max + frame.z_min) / z_range;
    return projection * view;
}

}  // namespace

ProjectedBounds projectAABB(const AABB& bounds, const LightFrame& frame) {
    ProjectedBounds projected;
    if (!bounds.valid()) {
        return projected;
    }
    projected.x_min = kInfinity;
    projected.y_min = kInfinity;
    projected.z_min = kInfinity;
    projected.x_max = -kInfinity;
    projected.y_max = -kInfinity;
    projected.z_max = -kInfinity;
    for (const Vec3& corner : bounds.corners()) {
        const Vec3 p = frame.toLightSpace(corner);
        projected.x_min = std::min(projected.x_min, p.x);
        projected.x_max = std::max(projected.x_max, p.x);
        projected.y_min = std::min(projected.y_min, p.y);
        projected.y_max = std::max(projected.y_max, p.y);
        projected.z_min = std::min(projected.z_min, p.z);
        projected.z_max = std::max(projected.z_max, p.z);
    }
    projected.valid = true;
    return projected;
}

Status buildCascadeTable(const Scene& scene, const RenderConfig& config,
                         CascadeTable& table) {
    table = {};
    if (scene.nodes.empty()) {
        return Status::error(ErrorCode::InternalError, "cannot build cascades for an empty scene");
    }

    const Vec3 camera_forward =
        glm::normalize(config.camera.target - config.camera.eye);
    Vec3 camera_right = glm::cross(camera_forward, config.camera.up);
    if (glm::length(camera_right) < 1.0e-6f) {
        return Status::error(ErrorCode::InvalidArgument,
                             "camera up vector is parallel to the view direction");
    }
    camera_right = glm::normalize(camera_right);
    const Vec3 camera_up = glm::normalize(glm::cross(camera_right, camera_forward));
    table.camera_eye = config.camera.eye;
    table.camera_forward = camera_forward;
    table.camera_right = camera_right;
    table.camera_up = camera_up;
    table.view = glm::lookAt(config.camera.eye, config.camera.target, camera_up);
    table.projection = glm::perspective(
        glm::radians(config.camera.fov_y_deg),
        static_cast<float>(config.width) / static_cast<float>(config.height),
        config.camera.near_plane, config.camera.far_plane);
    table.view_projection = table.projection * table.view;

    AABB scene_bounds;
    for (const Node& node : scene.nodes) {
        scene_bounds.expand(node.content_bounds.valid() ? node.content_bounds
                                                       : node.declared_bounds);
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

    LightFrame global_frame;
    global_frame.axis_x = light_right;
    global_frame.axis_y = light_up;
    global_frame.axis_z = light_forward;
    const ProjectedBounds scene_light = projectAABB(scene_bounds, global_frame);
    global_frame.x_min = scene_light.x_min;
    global_frame.x_max = scene_light.x_max;
    global_frame.y_min = scene_light.y_min;
    global_frame.y_max = scene_light.y_max;
    global_frame.z_min = scene_light.z_min;
    global_frame.z_max = scene_light.z_max;
    table.scene_light_frame = global_frame;

    const float near_plane = config.camera.near_plane;
    const float far_plane = config.camera.far_plane;
    const float tan_half_fov = std::tan(glm::radians(config.camera.fov_y_deg) * 0.5f);
    const float aspect =
        static_cast<float>(config.width) / static_cast<float>(config.height);

    std::vector<float> split_distances;
    split_distances.reserve(config.cascade_count + 1);
    split_distances.push_back(near_plane);
    for (std::uint32_t i = 1; i <= config.cascade_count; ++i) {
        const float ratio = static_cast<float>(i) /
                            static_cast<float>(config.cascade_count);
        const float uniform = near_plane + (far_plane - near_plane) * ratio;
        const float logarithmic =
            near_plane * std::pow(far_plane / near_plane, ratio);
        const float split = config.cascade_lambda * logarithmic +
                            (1.0f - config.cascade_lambda) * uniform;
        split_distances.push_back(std::min(far_plane, std::max(near_plane, split)));
    }
    split_distances.back() = far_plane;

    for (std::uint32_t cascade_index = 0; cascade_index < config.cascade_count;
         ++cascade_index) {
        const float near_distance = split_distances[cascade_index];
        const float far_distance = split_distances[cascade_index + 1];
        Vec3 corners[8];
        for (int i = 0; i < 4; ++i) {
            const float nx = (i == 0 || i == 3) ? -1.0f : 1.0f;
            const float ny = (i < 2) ? -1.0f : 1.0f;
            const Vec3 ray =
                camera_forward + camera_right * (nx * tan_half_fov * aspect) +
                camera_up * (ny * tan_half_fov);
            corners[i] = config.camera.eye + ray * near_distance;
            corners[i + 4] = config.camera.eye + ray * far_distance;
        }

        AABB slice_bounds;
        for (const Vec3& corner : corners) {
            slice_bounds.expand(corner);
        }
        ProjectedBounds projected = projectAABB(slice_bounds, global_frame);
        const float x_extent = std::max(projected.x_max - projected.x_min, 1.0e-4f);
        const float y_extent = std::max(projected.y_max - projected.y_min, 1.0e-4f);
        const float center_x = (projected.x_max + projected.x_min) * 0.5f;
        const float center_y = (projected.y_max + projected.y_min) * 0.5f;
        const float half_x = x_extent * 0.52f;
        const float half_y = y_extent * 0.52f;

        CascadeInfo cascade;
        cascade.index = cascade_index;
        cascade.near_distance = near_distance;
        cascade.far_distance = far_distance;
        const std::uint32_t shift = std::min(cascade_index, 3u);
        const std::uint32_t base =
            std::min(config.shadow_resolution, 8192u);
        cascade.resolution_x = std::max(64u, base >> shift);
        cascade.resolution_y = cascade.resolution_x;
        if ((cascade.resolution_x & (cascade.resolution_x - 1u)) != 0u) {
            std::uint32_t power = 64u;
            while ((power << 1u) <= cascade.resolution_x) {
                power <<= 1u;
            }
            cascade.resolution_x = power;
            cascade.resolution_y = power;
        }

        const float texel_x = (2.0f * half_x) / static_cast<float>(cascade.resolution_x);
        const float texel_y = (2.0f * half_y) / static_cast<float>(cascade.resolution_y);
        cascade.texel_world_size = std::max(texel_x, texel_y);
        const float snapped_center_x =
            std::round(center_x / texel_x) * texel_x;
        const float snapped_center_y =
            std::round(center_y / texel_y) * texel_y;

        cascade.frame = global_frame;
        cascade.frame.x_min = snapped_center_x - half_x;
        cascade.frame.x_max = snapped_center_x + half_x;
        cascade.frame.y_min = snapped_center_y - half_y;
        cascade.frame.y_max = snapped_center_y + half_y;
        cascade.frame.z_min = global_frame.z_min - 0.1f;
        cascade.frame.z_max = global_frame.z_max + 0.1f;
        cascade.depth_range = cascade.frame.z_max - cascade.frame.z_min;
        cascade.depth_quantization_step =
            cascade.depth_range / static_cast<float>((1u << 24u) - 1u);
        cascade.max_depth_quantization_error =
            cascade.depth_quantization_step * 0.5f;
        cascade.light_view_projection = lightViewProjectionMatrix(cascade.frame);
        table.cascades.push_back(cascade);
    }

    for (std::size_t i = 0; i + 1 < table.cascades.size(); ++i) {
        const CascadeInfo& a = table.cascades[i];
        const CascadeInfo& b = table.cascades[i + 1];
        CascadeBoundaryStats boundary;
        boundary.near_cascade = a.index;
        boundary.far_cascade = b.index;
        boundary.near_texel_world_size = a.texel_world_size;
        boundary.far_texel_world_size = b.texel_world_size;
        boundary.texel_coverage_difference =
            std::abs(a.texel_world_size - b.texel_world_size);
        boundary.relative_texel_coverage_difference =
            boundary.texel_coverage_difference /
            std::max(a.texel_world_size, b.texel_world_size);
        table.boundaries.push_back(boundary);
    }
    return Status::success();
}

std::uint32_t selectCascadeForDepth(const CascadeTable& table, float view_depth) {
    if (table.cascades.empty()) {
        return 0;
    }
    for (const CascadeInfo& cascade : table.cascades) {
        if (view_depth <= cascade.far_distance) {
            return cascade.index;
        }
    }
    return table.cascades.back().index;
}

}  // namespace shadowpipe

