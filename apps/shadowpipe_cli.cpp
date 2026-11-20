#include <cmath>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <optional>
#include <sstream>
#include <string>
#include <vector>

#include "shadowpipe/hash.hpp"
#include "shadowpipe/renderer.hpp"
#include "shadowpipe/scene.hpp"
#include "shadowpipe/snapshot.hpp"

namespace {

struct CommandLine {
    std::string scene_path = "data/synthetic.rscene";
    std::string config_path;
    std::string output_directory = "output";
    std::string snapshot_path;
    std::string load_snapshot_path;
    bool clear_output = true;
    bool show_help = false;
    std::vector<std::pair<std::string, std::string>> overrides;
};

void printUsage() {
    std::cout
        << "Usage: shadowpipe --scene <file.rscene> [options]\n"
        << "  --config <script>        key=value camera/light/pipeline parameters\n"
        << "  --output <dir>           artifact directory (default: output)\n"
        << "  --snapshot <file>        snapshot output path\n"
        << "  --load-snapshot <file>   validate/compare a previous snapshot\n"
        << "  --eye x,y,z --target x,y,z --up x,y,z\n"
        << "  --light x,y,z            direction in which light rays travel\n"
        << "  --width N --height N --cascades N --cascade-lambda F\n"
        << "  --shadow-resolution N --pyramid-resolution N\n"
        << "  --depth-bias F --normal-bias F --slope-scale F\n"
        << "  --max-bias-texels F --switch-tolerance F\n"
        << "  --no-heatmap --no-clear\n"
        << "Exit codes: 0 ok, 10 scene, 12 bias, 13 snapshot, 14 busy, 15 cancel.\n";
}

bool parseFloat(const std::string& text, float& value) {
    try {
        std::size_t consumed = 0;
        const float parsed = std::stof(text, &consumed);
        if (consumed != text.size() || !std::isfinite(parsed)) {
            return false;
        }
        value = parsed;
        return true;
    } catch (...) {
        return false;
    }
}

bool parseU32(const std::string& text, std::uint32_t& value) {
    try {
        std::size_t consumed = 0;
        const unsigned long parsed = std::stoul(text, &consumed);
        if (consumed != text.size() || parsed > 0xffffffffUL) {
            return false;
        }
        value = static_cast<std::uint32_t>(parsed);
        return true;
    } catch (...) {
        return false;
    }
}

bool parseVec3(const std::string& text, shadowpipe::Vec3& value) {
    std::string normalized = text;
    for (char& c : normalized) {
        if (c == ',' || c == '(' || c == ')') {
            c = ' ';
        }
    }
    std::istringstream in(normalized);
    in.imbue(std::locale::classic());
    float x = 0.0f;
    float y = 0.0f;
    float z = 0.0f;
    in >> x >> y >> z;
    if (!in) {
        return false;
    }
    std::string rest;
    std::getline(in, rest);
    if (!rest.empty() && rest.find_first_not_of(" \t\r\n") != std::string::npos) {
        return false;
    }
    if (!std::isfinite(x) || !std::isfinite(y) || !std::isfinite(z)) {
        return false;
    }
    value = shadowpipe::Vec3(x, y, z);
    return true;
}

bool parseCommandLine(int argc, char** argv, CommandLine& command_line) {
    for (int i = 1; i < argc; ++i) {
        const std::string argument = argv[i];
        if (argument == "--help" || argument == "-h") {
            printUsage();
            command_line.show_help = true;
            return false;
        }
        auto requireValue = [&](std::string& value) {
            if (i + 1 >= argc) {
                return false;
            }
            value = argv[++i];
            return true;
        };
        if (argument == "--scene") {
            if (!requireValue(command_line.scene_path)) {
                return false;
            }
        } else if (argument == "--config" || argument == "--script") {
            if (!requireValue(command_line.config_path)) {
                return false;
            }
        } else if (argument == "--output") {
            if (!requireValue(command_line.output_directory)) {
                return false;
            }
        } else if (argument == "--snapshot") {
            if (!requireValue(command_line.snapshot_path)) {
                return false;
            }
        } else if (argument == "--load-snapshot") {
            if (!requireValue(command_line.load_snapshot_path)) {
                return false;
            }
        } else if (argument == "--no-clear") {
            command_line.clear_output = false;
        } else if (argument == "--clear-output") {
            command_line.clear_output = true;
        } else if (argument == "--no-heatmap") {
            command_line.overrides.emplace_back("write_heatmap", "0");
        } else if (argument == "--eye" || argument == "--target" || argument == "--up" ||
                   argument == "--light") {
            std::string value;
            if (!requireValue(value)) {
                return false;
            }
            command_line.overrides.emplace_back(argument.substr(2), value);
        } else if (argument == "--width" || argument == "--height" ||
                   argument == "--cascades" || argument == "--shadow-resolution" ||
                   argument == "--pyramid-resolution" || argument == "--cascade-lambda" ||
                   argument == "--depth-bias" || argument == "--normal-bias" ||
                   argument == "--slope-scale" || argument == "--max-bias-texels" ||
                   argument == "--switch-tolerance") {
            std::string value;
            if (!requireValue(value)) {
                return false;
            }
            command_line.overrides.emplace_back(argument.substr(2), value);
        } else {
            std::cerr << "unknown argument: " << argument << "\n";
            return false;
        }
    }
    return true;
}

bool applyOverride(shadowpipe::RenderConfig& config, const std::string& key,
                   const std::string& value) {
    float float_value = 0.0f;
    std::uint32_t u32_value = 0;
    if (key == "eye" || key == "target" || key == "up" || key == "light") {
        shadowpipe::Vec3 vector;
        if (!parseVec3(value, vector)) {
            return false;
        }
        if (key == "eye") {
            config.camera.eye = vector;
        } else if (key == "target") {
            config.camera.target = vector;
        } else if (key == "up") {
            config.camera.up = vector;
        } else {
            config.light.direction = vector;
        }
        return true;
    }
    if (key == "width" || key == "height" || key == "cascades" ||
        key == "shadow-resolution" || key == "pyramid-resolution") {
        if (!parseU32(value, u32_value)) {
            return false;
        }
        if (key == "width") {
            config.width = u32_value;
        } else if (key == "height") {
            config.height = u32_value;
        } else if (key == "cascades") {
            config.cascade_count = u32_value;
        } else if (key == "shadow-resolution") {
            config.shadow_resolution = u32_value;
        } else {
            config.pyramid_resolution = u32_value;
        }
        return true;
    }
    if (!parseFloat(value, float_value)) {
        return false;
    }
    if (key == "cascade-lambda") {
        config.cascade_lambda = float_value;
    } else if (key == "depth-bias") {
        config.depth_bias = float_value;
    } else if (key == "normal-bias") {
        config.normal_bias = float_value;
    } else if (key == "slope-scale") {
        config.slope_scale = float_value;
    } else if (key == "max-bias-texels") {
        config.max_bias_texels = float_value;
    } else if (key == "switch-tolerance") {
        config.switch_tolerance = float_value;
    } else if (key == "write_heatmap") {
        config.write_heatmap = float_value != 0.0f;
    } else {
        return false;
    }
    return true;
}

int exitCode(shadowpipe::ErrorCode code) {
    switch (code) {
        case shadowpipe::ErrorCode::Ok:
            return 0;
        case shadowpipe::ErrorCode::SceneInvalid:
            return 10;
        case shadowpipe::ErrorCode::BiasInvalid:
            return 12;
        case shadowpipe::ErrorCode::SnapshotMismatch:
            return 13;
        case shadowpipe::ErrorCode::Busy:
            return 14;
        case shadowpipe::ErrorCode::Cancelled:
            return 15;
        case shadowpipe::ErrorCode::IoError:
            return 7;
        case shadowpipe::ErrorCode::InvalidArgument:
            return 2;
        default:
            return 9;
    }
}

void printStatus(const shadowpipe::Status& status) {
    std::cerr << shadowpipe::errorCodeName(status.code) << ": "
              << status.message;
    if (status.node_index >= 0) {
        std::cerr << " (node " << status.node_index;
        if (!status.node_name.empty()) {
            std::cerr << " '" << status.node_name << "'";
        }
        std::cerr << ")";
    }
    std::cerr << "\n";
}

}  // namespace

int main(int argc, char** argv) {
    CommandLine command_line;
    if (!parseCommandLine(argc, argv, command_line)) {
        return command_line.show_help ? 0 : 2;
    }

    shadowpipe::RenderConfig config;
    if (!command_line.config_path.empty()) {
        const shadowpipe::Status status =
            shadowpipe::loadConfigScript(command_line.config_path, config);
        if (!status.ok()) {
            printStatus(status);
            return exitCode(status.code);
        }
    }
    for (const auto& [key, value] : command_line.overrides) {
        if (!applyOverride(config, key, value)) {
            std::cerr << "invalid override --" << key << " " << value << "\n";
            return 2;
        }
    }
    shadowpipe::Status status = shadowpipe::validateRenderConfig(config);
    if (!status.ok()) {
        printStatus(status);
        shadowpipe::writePipelineErrorSummary(command_line.output_directory,
                                              status);
        return exitCode(status.code);
    }

    if (command_line.snapshot_path.empty()) {
        command_line.snapshot_path =
            (std::filesystem::path(command_line.output_directory) / "snapshot.bin")
                .string();
    }

    std::optional<shadowpipe::SnapshotData> previous_snapshot;
    if (!command_line.load_snapshot_path.empty()) {
        const shadowpipe::SnapshotLoadResult loaded =
            shadowpipe::loadSnapshotFile(command_line.load_snapshot_path);
        if (!loaded.status.ok()) {
            printStatus(loaded.status);
            return exitCode(loaded.status.code);
        }
        previous_snapshot = loaded.snapshot;
    }

    if (command_line.clear_output) {
        status = shadowpipe::clearRenderOutputs(
            command_line.output_directory, command_line.snapshot_path);
        if (!status.ok()) {
            printStatus(status);
            return exitCode(status.code);
        }
    }

    const shadowpipe::SceneLoadResult loaded_scene =
        shadowpipe::loadSceneFromFile(command_line.scene_path);
    if (!loaded_scene.status.ok()) {
        printStatus(loaded_scene.status);
        shadowpipe::writePipelineErrorSummary(command_line.output_directory,
                                              loaded_scene.status);
        return exitCode(loaded_scene.status.code);
    }

    shadowpipe::RenderService service;
    shadowpipe::OutputPaths paths;
    paths.directory = command_line.output_directory;
    paths.snapshot_file = command_line.snapshot_path;
    shadowpipe::RenderResult result =
        service.render(loaded_scene.scene, config, paths,
                       previous_snapshot ? &*previous_snapshot : nullptr);
    if (!result.status.ok()) {
        printStatus(result.status);
        return exitCode(result.status.code);
    }

    std::cout << "E_OK scene=" << shadowpipe::hashHex(result.summary.scene_hash)
              << " frame=" << shadowpipe::hashHex(result.summary.frame_hash)
              << " visible=" << result.summary.visible_object_count
              << " occluded=" << result.summary.occluded_object_count
              << " ambiguous=" << result.summary.occlusion_ambiguous_count
              << " culled=" << result.summary.frustum_culled_count
              << " switches=" << result.summary.cascade_switch_count << "\n";
    if (result.summary.snapshot_loaded) {
        std::cout << "snapshot_changed="
                  << (result.summary.snapshot_diff.rerenderRequired() ? "true"
                                                                      : "false")
                  << " view_changed="
                  << (result.summary.snapshot_diff.view_changed ? "true" : "false")
                  << " cascade_changed="
                  << (result.summary.snapshot_diff.cascade_table_changed ? "true"
                                                                         : "false")
                  << "\n";
    }
    std::cout << "artifacts=" << command_line.output_directory
              << " snapshot=" << command_line.snapshot_path << "\n";
    return 0;
}

