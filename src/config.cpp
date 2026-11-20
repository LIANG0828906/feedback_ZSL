#include "shadowpipe/config.hpp"

#include <algorithm>
#include <cctype>
#include <cmath>
#include <fstream>
#include <sstream>
#include <string_view>

#include "shadowpipe/error.hpp"

namespace shadowpipe {

const char* errorCodeName(ErrorCode code) {
    switch (code) {
        case ErrorCode::Ok:
            return "E_OK";
        case ErrorCode::SceneInvalid:
            return "E_SCENE_INVALID";
        case ErrorCode::OcclusionAmbiguous:
            return "E_OCCLUSION_AMBIGUOUS";
        case ErrorCode::BiasInvalid:
            return "E_BIAS_INVALID";
        case ErrorCode::SnapshotMismatch:
            return "E_SNAPSHOT_MISMATCH";
        case ErrorCode::Busy:
            return "E_BUSY";
        case ErrorCode::Cancelled:
            return "E_CANCELLED";
        case ErrorCode::IoError:
            return "E_IO_ERROR";
        case ErrorCode::InvalidArgument:
            return "E_INVALID_ARGUMENT";
        case ErrorCode::InternalError:
            return "E_INTERNAL_ERROR";
    }
    return "E_INTERNAL_ERROR";
}

namespace {

std::string trim(std::string_view input) {
    std::size_t first = 0;
    while (first < input.size() &&
           std::isspace(static_cast<unsigned char>(input[first])) != 0) {
        ++first;
    }
    std::size_t last = input.size();
    while (last > first &&
           std::isspace(static_cast<unsigned char>(input[last - 1])) != 0) {
        --last;
    }
    return std::string(input.substr(first, last - first));
}

bool parseFloat(const std::string& text, float& value) {
    std::istringstream in(text);
    in.imbue(std::locale::classic());
    float parsed = 0.0f;
    in >> parsed;
    if (!in || !std::isfinite(parsed)) {
        return false;
    }
    std::string rest;
    std::getline(in, rest);
    if (!trim(rest).empty()) {
        return false;
    }
    value = parsed;
    return true;
}

bool parseU32(const std::string& text, std::uint32_t& value) {
    std::istringstream in(text);
    in.imbue(std::locale::classic());
    std::uint64_t parsed = 0;
    in >> parsed;
    if (!in || parsed > 0xffffffffULL) {
        return false;
    }
    std::string rest;
    std::getline(in, rest);
    if (!trim(rest).empty()) {
        return false;
    }
    value = static_cast<std::uint32_t>(parsed);
    return true;
}

bool parseVec3(const std::string& text, Vec3& value) {
    std::string normalized = text;
    std::replace(normalized.begin(), normalized.end(), ',', ' ');
    std::replace(normalized.begin(), normalized.end(), '(', ' ');
    std::replace(normalized.begin(), normalized.end(), ')', ' ');
    std::istringstream in(normalized);
    in.imbue(std::locale::classic());
    float x = 0.0f;
    float y = 0.0f;
    float z = 0.0f;
    in >> x >> y >> z;
    if (!in || !std::isfinite(x) || !std::isfinite(y) || !std::isfinite(z)) {
        return false;
    }
    std::string rest;
    std::getline(in, rest);
    if (!trim(rest).empty()) {
        return false;
    }
    value = Vec3(x, y, z);
    return true;
}

}  // namespace

Status validateRenderConfig(const RenderConfig& config) {
    const BiasParameters bias{config.depth_bias, config.normal_bias, config.slope_scale,
                              config.max_bias_texels};
    if (!std::isfinite(bias.depth_bias) || !std::isfinite(bias.normal_bias) ||
        !std::isfinite(bias.slope_scale) || !std::isfinite(bias.max_bias_texels)) {
        return Status::error(ErrorCode::BiasInvalid,
                             "bias parameters must be finite");
    }
    if (bias.depth_bias < 0.0f || bias.normal_bias < 0.0f || bias.slope_scale < 0.0f) {
        return Status::error(ErrorCode::BiasInvalid,
                             "depth, normal and slope bias must be non-negative");
    }
    if (bias.depth_bias > 16.0f || bias.normal_bias > 16.0f ||
        bias.slope_scale > 32.0f || bias.max_bias_texels <= 0.0f ||
        bias.max_bias_texels > 256.0f) {
        return Status::error(ErrorCode::BiasInvalid,
                             "bias parameter is outside the supported range");
    }
    if (!std::isfinite(config.switch_tolerance) || config.switch_tolerance < 0.0f ||
        config.switch_tolerance > 4.0f) {
        return Status::error(ErrorCode::InvalidArgument,
                             "switch tolerance must be in [0, 4]");
    }
    if (config.width == 0 || config.height == 0 || config.width > 4096 ||
        config.height > 4096 || config.shadow_resolution < 16 ||
        config.shadow_resolution > 8192 || config.pyramid_resolution < 16 ||
        config.pyramid_resolution > 2048) {
        return Status::error(ErrorCode::InvalidArgument,
                             "image or shadow resolution is outside the supported range");
    }
    if (config.cascade_count == 0 || config.cascade_count > 4) {
        return Status::error(ErrorCode::InvalidArgument,
                             "cascade count must be in [1, 4]");
    }
    if (!std::isfinite(config.cascade_lambda) || config.cascade_lambda < 0.0f ||
        config.cascade_lambda > 1.0f) {
        return Status::error(ErrorCode::InvalidArgument,
                             "cascade lambda must be in [0, 1]");
    }
    if (!finiteVec(config.camera.eye) || !finiteVec(config.camera.target) ||
        !finiteVec(config.camera.up) || !finiteVec(config.light.direction)) {
        return Status::error(ErrorCode::InvalidArgument,
                             "camera and light vectors must be finite");
    }
    if (!std::isfinite(config.camera.fov_y_deg) || config.camera.fov_y_deg <= 1.0f ||
        config.camera.fov_y_deg >= 179.0f) {
        return Status::error(ErrorCode::InvalidArgument,
                             "vertical field of view must be in (1, 179) degrees");
    }
    if (!std::isfinite(config.camera.near_plane) ||
        !std::isfinite(config.camera.far_plane) || config.camera.near_plane <= 0.0f ||
        config.camera.far_plane <= config.camera.near_plane) {
        return Status::error(ErrorCode::InvalidArgument,
                             "camera near/far planes are invalid");
    }
    if (glm::length(config.camera.target - config.camera.eye) < 1.0e-5f ||
        glm::length(config.camera.up) < 1.0e-5f ||
        glm::length(config.light.direction) < 1.0e-5f) {
        return Status::error(ErrorCode::InvalidArgument,
                             "camera direction, up vector and light direction must be non-zero");
    }
    return Status::success();
}

Status loadConfigScript(const std::string& path, RenderConfig& config) {
    std::ifstream in(path);
    if (!in) {
        return Status::error(ErrorCode::IoError,
                             "cannot open configuration script: " + path);
    }

    std::string line;
    std::uint64_t line_number = 0;
    while (std::getline(in, line)) {
        ++line_number;
        const std::size_t comment = line.find('#');
        if (comment != std::string::npos) {
            line.resize(comment);
        }
        line = trim(line);
        if (line.empty()) {
            continue;
        }
        const std::size_t equal = line.find('=');
        if (equal == std::string::npos) {
            return Status::error(ErrorCode::InvalidArgument,
                                 "config line " + std::to_string(line_number) +
                                     " must be key=value");
        }
        const std::string key = trim(std::string_view(line).substr(0, equal));
        const std::string value = trim(std::string_view(line).substr(equal + 1));
        const std::string where = " (line " + std::to_string(line_number) + ")";
        bool ok = false;

        if (key == "eye") {
            ok = parseVec3(value, config.camera.eye);
        } else if (key == "target") {
            ok = parseVec3(value, config.camera.target);
        } else if (key == "up") {
            ok = parseVec3(value, config.camera.up);
        } else if (key == "light_dir" || key == "light") {
            ok = parseVec3(value, config.light.direction);
        } else if (key == "fov_y_deg") {
            ok = parseFloat(value, config.camera.fov_y_deg);
        } else if (key == "near") {
            ok = parseFloat(value, config.camera.near_plane);
        } else if (key == "far") {
            ok = parseFloat(value, config.camera.far_plane);
        } else if (key == "width") {
            ok = parseU32(value, config.width);
        } else if (key == "height") {
            ok = parseU32(value, config.height);
        } else if (key == "cascades") {
            ok = parseU32(value, config.cascade_count);
        } else if (key == "cascade_lambda") {
            ok = parseFloat(value, config.cascade_lambda);
        } else if (key == "shadow_resolution") {
            ok = parseU32(value, config.shadow_resolution);
        } else if (key == "pyramid_resolution") {
            ok = parseU32(value, config.pyramid_resolution);
        } else if (key == "depth_bias") {
            ok = parseFloat(value, config.depth_bias);
        } else if (key == "normal_bias") {
            ok = parseFloat(value, config.normal_bias);
        } else if (key == "slope_scale") {
            ok = parseFloat(value, config.slope_scale);
        } else if (key == "max_bias_texels") {
            ok = parseFloat(value, config.max_bias_texels);
        } else if (key == "switch_tolerance") {
            ok = parseFloat(value, config.switch_tolerance);
        } else {
            return Status::error(ErrorCode::InvalidArgument,
                                 "unknown config key '" + key + "'" + where);
        }
        if (!ok) {
            return Status::error(ErrorCode::InvalidArgument,
                                 "invalid value for '" + key + "'" + where);
        }
    }
    return Status::success();
}

void applyPatchOptions(RenderConfig& config, const PatchOptions& patch) {
    config.camera = patch.camera;
    config.light = patch.light;
    config.width = patch.width;
    config.height = patch.height;
    config.cascade_count = patch.cascade_count;
    config.cascade_lambda = patch.cascade_lambda;
    config.shadow_resolution = patch.shadow_resolution;
    config.pyramid_resolution = patch.pyramid_resolution;
    config.depth_bias = patch.depth_bias;
    config.normal_bias = patch.normal_bias;
    config.slope_scale = patch.slope_scale;
    config.max_bias_texels = patch.max_bias_texels;
    config.switch_tolerance = patch.switch_tolerance;
}

}  // namespace shadowpipe

