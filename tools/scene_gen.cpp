#include "preview/hash.hpp"
#include "preview/renderer.hpp"
#include "preview/sample_scene.hpp"
#include "preview/scene_io.hpp"
#include "preview/streaming.hpp"

#include <filesystem>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <locale>
#include <map>
#include <sstream>
#include <stdexcept>
#include <string>

namespace {

std::string escapeJson(const std::string& value) {
    std::string output;
    for (char ch : value) {
        switch (ch) {
            case '"': output += "\\\""; break;
            case '\\': output += "\\\\"; break;
            case '\n': output += "\\n"; break;
            default: output += ch; break;
        }
    }
    return output;
}

void printUsage() {
    std::cout
        << "Usage: scene_gen --out DIR [options]\n"
        << "  --grid-x N              chunks along X (default 6)\n"
        << "  --grid-y N              chunks along Y (default 4)\n"
        << "  --layers N              depth layers (default 3)\n"
        << "  --cell SIZE             chunk cell size (default 1.2)\n"
        << "  --spacing SIZE          depth spacing (default 1.5)\n"
        << "  --occluder-density F    fraction in [0,1] (default 0.65)\n"
        << "  --seed N                deterministic seed (default 1584411188)\n"
        << "  --views N               view sequence length (default 8)\n"
        << "  --width N --height N    offscreen size (default 320x180)\n"
        << "  --no-baseline           do not render baseline hashes\n";
}

std::map<std::string, std::string> parseOptions(int argc, char** argv) {
    std::map<std::string, std::string> options;
    for (int i = 1; i < argc; ++i) {
        std::string key = argv[i];
        if (key.rfind("--", 0) != 0) {
            throw std::runtime_error("unexpected argument: " + key);
        }
        if (key == "--no-baseline" || key == "--help") {
            options[key] = "1";
            continue;
        }
        if (i + 1 >= argc) {
            throw std::runtime_error("missing value for " + key);
        }
        options[key] = argv[++i];
    }
    return options;
}

int intOption(const std::map<std::string, std::string>& options,
              const std::string& key, int fallback) {
    const auto found = options.find(key);
    return found == options.end() ? fallback : std::stoi(found->second);
}

double doubleOption(const std::map<std::string, std::string>& options,
                    const std::string& key, double fallback) {
    const auto found = options.find(key);
    return found == options.end() ? fallback : std::stod(found->second);
}

std::uint64_t uintOption(const std::map<std::string, std::string>& options,
                         const std::string& key, std::uint64_t fallback) {
    const auto found = options.find(key);
    return found == options.end() ? fallback : std::stoull(found->second);
}

}  // namespace

int main(int argc, char** argv) {
    try {
        const auto options = parseOptions(argc, argv);
        if (options.find("--help") != options.end()) {
            printUsage();
            return 0;
        }
        const auto outIt = options.find("--out");
        if (outIt == options.end()) {
            printUsage();
            throw std::runtime_error("--out is required");
        }
        const std::filesystem::path outputDirectory(outIt->second);
        std::error_code directoryError;
        std::filesystem::create_directories(outputDirectory, directoryError);
        if (directoryError) {
            throw std::runtime_error("failed to create output directory: " +
                                     directoryError.message());
        }

        preview::SampleSceneConfig sceneConfig;
        sceneConfig.gridX = intOption(options, "--grid-x", 6);
        sceneConfig.gridY = intOption(options, "--grid-y", 4);
        sceneConfig.depthLayers = intOption(options, "--layers", 3);
        sceneConfig.cellSize = doubleOption(options, "--cell", 1.2);
        sceneConfig.layerSpacing = doubleOption(options, "--spacing", 1.5);
        sceneConfig.occluderDensity =
            doubleOption(options, "--occluder-density", 0.65);
        sceneConfig.seed = uintOption(options, "--seed", 0x5eed1234ull);
        const int viewCount = intOption(options, "--views", 8);
        const int width = intOption(options, "--width", 320);
        const int height = intOption(options, "--height", 180);
        if (viewCount <= 0 || width <= 0 || height <= 0 ||
            sceneConfig.gridX <= 0 || sceneConfig.gridY <= 0 ||
            sceneConfig.depthLayers <= 0) {
            throw std::runtime_error("sample dimensions must be positive");
        }
        if (sceneConfig.occluderDensity < 0.0 ||
            sceneConfig.occluderDensity > 1.0) {
            throw std::runtime_error("occluder density must be in [0,1]");
        }

        const preview::SampleSceneData sceneData =
            preview::generateSampleScene(sceneConfig);
        const auto scenePath = outputDirectory / "scene.bin";
        const auto viewsPath = outputDirectory / "views.txt";
        preview::Diagnostic diagnostic;
        if (!preview::writeSceneFile(scenePath.string(), sceneData.roots,
                                     sceneData.blocks, 0, &diagnostic)) {
            throw std::runtime_error("failed to write scene: " +
                                     diagnostic.toJson());
        }
        const auto views =
            preview::generateSampleViews(sceneConfig, viewCount, width, height);
        if (!preview::writeViewSequenceFile(viewsPath.string(), views,
                                            &diagnostic)) {
            throw std::runtime_error("failed to write views: " +
                                     diagnostic.toJson());
        }
        const preview::SceneLoadResult loaded =
            preview::loadSceneFile(scenePath.string());
        if (!loaded) {
            throw std::runtime_error("generated scene did not validate: " +
                                     loaded.diagnostic.toJson());
        }

        std::cout << "scene=" << scenePath.string() << "\n"
                  << "views=" << viewsPath.string() << "\n"
                  << "blocks=" << loaded.scene->blocks.size() << "\n"
                  << "scene_hash=0x" << preview::hex64(loaded.scene->contentHash)
                  << "\n";

        if (options.find("--no-baseline") != options.end()) {
            return 0;
        }

        preview::StreamingLoader loader(loaded.scene,
                                        preview::StreamingConfig{16});
        loader.start();
        loader.enqueueAll();
        if (!loader.waitUntilIdle()) {
            throw std::runtime_error("streaming loader timed out");
        }
        const auto loadSnapshot = loader.snapshot();
        preview::RenderConfig renderConfig;
        renderConfig.pyramid.width = width;
        renderConfig.pyramid.height = height;
        renderConfig.pyramid.quantizationBits = 20;
        renderConfig.cancellationCheckpointBlocks = 16;
        preview::Renderer renderer(loader, renderConfig);
        const auto baselinePath = outputDirectory / "baseline.json";
        const auto trajectoryPath = outputDirectory / "trajectory.jsonl";
        std::ofstream baseline(baselinePath, std::ios::trunc);
        std::ofstream trajectory(trajectoryPath, std::ios::trunc);
        if (!baseline || !trajectory) {
            throw std::runtime_error("failed to create baseline output files");
        }
        baseline.imbue(std::locale::classic());
        trajectory.imbue(std::locale::classic());
        baseline << std::setprecision(17);
        trajectory << std::setprecision(17);
        baseline << "{\"baseline_version\":1"
                 << ",\"scene\":\"scene.bin\""
                 << ",\"views\":\"views.txt\""
                 << ",\"scene_hash\":\"0x"
                 << preview::hex64(loaded.scene->contentHash)
                 << "\",\"load_hash\":\"0x"
                 << preview::hex64(loadSnapshot->contentHash)
                 << "\",\"entries\":[";

        for (std::size_t i = 0; i < views.size(); ++i) {
            preview::RenderRequest request;
            request.camera = views[i].camera;
            request.expectedSnapshotVersion = loadSnapshot->version;
            const preview::RenderResult result = renderer.render(request);
            if (!result) {
                throw std::runtime_error("baseline render failed for " +
                                         views[i].name + ": " +
                                         result.diagnostic.toJson());
            }
            if (i != 0) {
                baseline << ',';
            }
            baseline << "{\"view\":\"" << escapeJson(views[i].name)
                     << "\",\"mask_hash\":\"0x"
                     << preview::hex64(result.mask.contentHash)
                     << "\",\"summary_hash\":\"0x"
                     << preview::hex64(result.summary.summaryHash())
                     << "\",\"snapshot_version\":" << result.snapshotVersion
                     << ",\"pyramid_version\":" << result.pyramidVersion << "}";
            trajectory
                << "{\"view\":\"" << escapeJson(views[i].name) << "\""
                << ",\"camera\":{\"position\":[" << views[i].camera.position.x
                << ',' << views[i].camera.position.y << ','
                << views[i].camera.position.z << "],\"target\":["
                << views[i].camera.target.x << ','
                << views[i].camera.target.y << ','
                << views[i].camera.target.z << "],\"fov_y\":"
                << views[i].camera.fovYDegrees << "}"
                << ",\"snapshot_version\":" << result.snapshotVersion
                << ",\"pyramid_version\":" << result.pyramidVersion
                << ",\"load_content_hash\":\"0x"
                << preview::hex64(result.summary.loadContentHash)
                << "\",\"scene_content_hash\":\"0x"
                << preview::hex64(loaded.scene->contentHash)
                << "\",\"mask_hash\":\"0x"
                << preview::hex64(result.mask.contentHash)
                << "\",\"summary_hash\":\"0x"
                << preview::hex64(result.summary.summaryHash())
                << "\",\"summary\":" << result.summary.canonicalJson() << '\n';
        }
        baseline << "]}";
        baseline.flush();
        trajectory.flush();
        if (!baseline || !trajectory) {
            throw std::runtime_error("failed to write baseline outputs");
        }
        std::cout << "baseline=" << baselinePath.string() << "\n"
                  << "trajectory=" << trajectoryPath.string() << "\n";
        return 0;
    } catch (const std::exception& exception) {
        std::cerr << "scene_gen: " << exception.what() << "\n";
        return 1;
    }
}
