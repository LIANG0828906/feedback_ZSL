#pragma once

#include <atomic>
#include <cstdint>
#include <functional>
#include <memory>
#include <string>
#include <vector>

#include "shadowpipe/error.hpp"
#include "shadowpipe/math.hpp"

namespace shadowpipe {

enum class Checkpoint {
    BeforePyramidLevel,
    AfterPyramidLevel,
    BeforeCascade,
    AfterCascade,
    BeforeRaster,
    BeforeCommit,
};

struct CameraConfig {
    Vec3 eye{0.0f, 6.0f, 16.0f};
    Vec3 target{0.0f, 2.0f, 0.0f};
    Vec3 up{0.0f, 1.0f, 0.0f};
    float fov_y_deg = 55.0f;
    float near_plane = 0.5f;
    float far_plane = 45.0f;
};

struct LightConfig {
    // Direction in which light rays travel.
    Vec3 direction{-0.42f, -1.0f, -0.31f};
};

using CheckpointHook = std::function<void(Checkpoint, std::uint32_t)>;

struct RenderConfig {
    CameraConfig camera;
    LightConfig light;

    std::uint32_t width = 192;
    std::uint32_t height = 144;
    std::uint32_t cascade_count = 4;
    float cascade_lambda = 0.70f;
    std::uint32_t shadow_resolution = 512;
    std::uint32_t pyramid_resolution = 256;

    // Parameters are expressed in cascade texels.
    float depth_bias = 1.50f;
    float normal_bias = 1.25f;
    float slope_scale = 1.00f;
    float max_bias_texels = 64.0f;
    float switch_tolerance = 0.05f;

    bool write_heatmap = true;
    std::shared_ptr<std::atomic_bool> cancel;
    CheckpointHook checkpoint_hook;
};

struct BiasParameters {
    float depth_bias = 1.5f;
    float normal_bias = 1.25f;
    float slope_scale = 1.0f;
    float max_bias_texels = 64.0f;
};

struct PatchOptions {
    CameraConfig camera;
    LightConfig light;
    std::uint32_t width = 192;
    std::uint32_t height = 144;
    std::uint32_t cascade_count = 4;
    float cascade_lambda = 0.70f;
    std::uint32_t shadow_resolution = 512;
    std::uint32_t pyramid_resolution = 256;
    float depth_bias = 1.50f;
    float normal_bias = 1.25f;
    float slope_scale = 1.00f;
    float max_bias_texels = 64.0f;
    float switch_tolerance = 0.05f;
};

Status validateRenderConfig(const RenderConfig& config);
Status loadConfigScript(const std::string& path, RenderConfig& config);
void applyPatchOptions(RenderConfig& config, const PatchOptions& patch);

}  // namespace shadowpipe

