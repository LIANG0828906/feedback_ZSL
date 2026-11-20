#pragma once

#include <cstdint>
#include <vector>

#include "shadowpipe/config.hpp"
#include "shadowpipe/math.hpp"
#include "shadowpipe/scene.hpp"

namespace shadowpipe {

struct LightFrame {
    Vec3 axis_x{1.0f, 0.0f, 0.0f};
    Vec3 axis_y{0.0f, 1.0f, 0.0f};
    Vec3 axis_z{0.0f, 0.0f, 1.0f};
    float x_min = -1.0f;
    float x_max = 1.0f;
    float y_min = -1.0f;
    float y_max = 1.0f;
    float z_min = 0.0f;
    float z_max = 1.0f;

    Vec3 toLightSpace(const Vec3& world) const {
        return Vec3(glm::dot(world, axis_x), glm::dot(world, axis_y),
                    glm::dot(world, axis_z));
    }
};

struct ProjectedBounds {
    float x_min = 0.0f;
    float x_max = 0.0f;
    float y_min = 0.0f;
    float y_max = 0.0f;
    float z_min = 0.0f;
    float z_max = 0.0f;
    bool valid = false;
};

ProjectedBounds projectAABB(const AABB& bounds, const LightFrame& frame);

struct CascadeInfo {
    std::uint32_t index = 0;
    float near_distance = 0.0f;
    float far_distance = 0.0f;
    std::uint32_t resolution_x = 0;
    std::uint32_t resolution_y = 0;
    float texel_world_size = 0.0f;
    float depth_range = 1.0f;
    float depth_quantization_step = 0.0f;
    float max_depth_quantization_error = 0.0f;
    LightFrame frame;
    Mat4 light_view_projection{1.0f};
};

struct CascadeBoundaryStats {
    std::uint32_t near_cascade = 0;
    std::uint32_t far_cascade = 1;
    float near_texel_world_size = 0.0f;
    float far_texel_world_size = 0.0f;
    float texel_coverage_difference = 0.0f;
    float relative_texel_coverage_difference = 0.0f;
};

struct CascadeTable {
    std::vector<CascadeInfo> cascades;
    std::vector<CascadeBoundaryStats> boundaries;
    LightFrame scene_light_frame;
    Vec3 camera_eye{0.0f};
    Vec3 camera_forward{0.0f, 0.0f, -1.0f};
    Vec3 camera_right{1.0f, 0.0f, 0.0f};
    Vec3 camera_up{0.0f, 1.0f, 0.0f};
    Mat4 view{1.0f};
    Mat4 projection{1.0f};
    Mat4 view_projection{1.0f};
};

Status buildCascadeTable(const Scene& scene, const RenderConfig& config,
                         CascadeTable& table);

std::uint32_t selectCascadeForDepth(const CascadeTable& table, float view_depth);

}  // namespace shadowpipe

