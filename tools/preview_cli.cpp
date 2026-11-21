#include "preview/hash.hpp"
#include "preview/renderer.hpp"
#include "preview/sample_scene.hpp"
#include "preview/scene_io.hpp"
#include "preview/snapshot.hpp"
#include "preview/streaming.hpp"

#include <algorithm>
#include <csignal>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <locale>
#include <map>
#include <stdexcept>
#include <string>
#include <vector>

namespace {

void printUsage() {
    std::cout
        << "Usage:\n"
        << "  preview_cli render --scene FILE --views FILE --out DIR"
           " [--snapshot FILE] [--batch N]\n"
        << "  preview_cli recover --scene FILE --views FILE --snapshot FILE"
           " [--view-index N]\n"
        << "  preview_cli crash-writer --scene FILE --views FILE --snapshot FILE"
           " [--view-index N]\n";
}

std::map<std::string, std::string> parseOptions(int argc, char** argv,
                                                std::string* command) {
    std::map<std::string, std::string> options;
    if (argc < 2) {
        throw std::runtime_error("missing command");
    }
    *command = argv[1];
    for (int i = 2; i < argc; ++i) {
        std::string key = argv[i];
        if (key.rfind("--", 0) != 0 || i + 1 >= argc) {
            throw std::runtime_error("invalid option or missing value: " + key);
        }
        options[key] = argv[++i];
    }
    return options;
}

std::string required(const std::map<std::string, std::string>& options,
                     const std::string& key) {
    const auto found = options.find(key);
    if (found == options.end()) {
        throw std::runtime_error("missing required option " + key);
    }
    return found->second;
}

std::size_t indexOption(const std::map<std::string, std::string>& options,
                        const std::string& key, std::size_t fallback) {
    const auto found = options.find(key);
    if (found == options.end()) {
        return fallback;
    }
    const int value = std::stoi(found->second);
    if (value < 0) {
        throw std::runtime_error(key + " must be non-negative");
    }
    return static_cast<std::size_t>(value);
}

void writeU32(std::ostream* stream, std::uint32_t value) {
    const char bytes[4] = {
        static_cast<char>(value & 0xffu),
        static_cast<char>((value >> 8u) & 0xffu),
        static_cast<char>((value >> 16u) & 0xffu),
        static_cast<char>((value >> 24u) & 0xffu)};
    stream->write(bytes, 4);
}

void writeU64(std::ostream* stream, std::uint64_t value) {
    writeU32(stream, static_cast<std::uint32_t>(value & 0xffffffffu));
    writeU32(stream, static_cast<std::uint32_t>(value >> 32u));
}

bool writeMaskFile(const std::filesystem::path& path,
                   const preview::VisibilityMask& mask) {
    std::ofstream stream(path, std::ios::binary | std::ios::trunc);
    if (!stream) {
        return false;
    }
    stream.write("ZSPVMASK", 8);
    writeU32(&stream, static_cast<std::uint32_t>(mask.width));
    writeU32(&stream, static_cast<std::uint32_t>(mask.height));
    writeU64(&stream, mask.snapshotVersion);
    writeU64(&stream, mask.pyramidVersion);
    writeU64(&stream, mask.loadContentHash);
    writeU64(&stream, mask.contentHash);
    if (!mask.pixels.empty()) {
        stream.write(reinterpret_cast<const char*>(mask.pixels.data()),
                     static_cast<std::streamsize>(
                         mask.pixels.size() * sizeof(std::uint32_t)));
    }
    stream.flush();
    return static_cast<bool>(stream);
}

struct LoadedInputs {
    std::shared_ptr<const preview::Scene> scene;
    std::vector<preview::SampleView> views;
};

LoadedInputs loadInputs(const std::map<std::string, std::string>& options) {
    LoadedInputs inputs;
    const preview::SceneLoadResult loaded =
        preview::loadSceneFile(required(options, "--scene"));
    if (!loaded) {
        throw std::runtime_error("scene load failed: " +
                                 loaded.diagnostic.toJson());
    }
    preview::Diagnostic diagnostic;
    if (!preview::readViewSequenceFile(required(options, "--views"),
                                       &inputs.views, &diagnostic)) {
        throw std::runtime_error("view load failed: " + diagnostic.toJson());
    }
    if (inputs.views.empty()) {
        throw std::runtime_error("view sequence is empty");
    }
    inputs.scene = loaded.scene;
    return inputs;
}

int runRender(const std::map<std::string, std::string>& options) {
    const LoadedInputs inputs = loadInputs(options);
    const std::filesystem::path outputDirectory(required(options, "--out"));
    std::error_code error;
    std::filesystem::create_directories(outputDirectory, error);
    if (error) {
        throw std::runtime_error("failed to create output directory: " +
                                 error.message());
    }
    const std::size_t batch = indexOption(options, "--batch", 8);
    preview::StreamingLoader loader(
        inputs.scene, preview::StreamingConfig{std::max<std::size_t>(1, batch)});
    loader.start();
    preview::RenderConfig renderConfig;
    renderConfig.pyramid.width = inputs.views.front().camera.width;
    renderConfig.pyramid.height = inputs.views.front().camera.height;
    renderConfig.pyramid.quantizationBits = 20;
    renderConfig.cancellationCheckpointBlocks = 16;
    const auto snapshotIt = options.find("--snapshot");
    if (snapshotIt != options.end()) {
        renderConfig.snapshotPath = snapshotIt->second;
    }
    preview::Renderer renderer(loader, renderConfig);
    const auto trajectoryPath = outputDirectory / "trace.jsonl";
    std::ofstream trajectory(trajectoryPath, std::ios::trunc);
    if (!trajectory) {
        throw std::runtime_error("failed to create trace output");
    }
    trajectory.imbue(std::locale::classic());
    trajectory << std::setprecision(17);

    for (const preview::SampleView& view : inputs.views) {
        loader.scheduleForView(view.camera);
        if (!loader.waitUntilIdle()) {
            throw std::runtime_error("streaming loader timed out");
        }
        preview::RenderRequest request;
        request.camera = view.camera;
        const preview::RenderResult result = renderer.render(request);
        if (!result) {
            throw std::runtime_error("render failed for " + view.name + ": " +
                                     result.diagnostic.toJson());
        }
        const auto maskPath = outputDirectory / (view.name + ".mask");
        if (!writeMaskFile(maskPath, result.mask)) {
            throw std::runtime_error("failed to write mask " +
                                     maskPath.string());
        }
        trajectory
            << "{\"view\":\"" << view.name << "\",\"camera\":{\"position\":["
            << view.camera.position.x << ',' << view.camera.position.y << ','
            << view.camera.position.z << "],\"target\":["
            << view.camera.target.x << ',' << view.camera.target.y << ','
            << view.camera.target.z << "],\"fov_y\":"
            << view.camera.fovYDegrees << "},\"snapshot_version\":"
            << result.snapshotVersion << ",\"pyramid_version\":"
            << result.pyramidVersion << ",\"pyramid_rebuilt\":"
            << (result.pyramidRebuild.rebuilt ? "true" : "false")
            << ",\"layer_switches\":" << result.summary.layerSwitches
            << ",\"critical_blocks\":" << result.summary.criticalBlocks
            << ",\"load_content_hash\":\"0x"
            << preview::hex64(result.summary.loadContentHash)
            << "\",\"scene_content_hash\":\"0x"
            << preview::hex64(inputs.scene->contentHash)
            << "\",\"mask_hash\":\"0x"
            << preview::hex64(result.mask.contentHash)
            << "\",\"summary_hash\":\"0x"
            << preview::hex64(result.summary.summaryHash())
            << "\",\"summary\":" << result.summary.canonicalJson() << '\n';
    }
    trajectory.flush();
    if (!trajectory) {
        throw std::runtime_error("failed to write trace output");
    }
    std::cout << "rendered_views=" << inputs.views.size() << "\n"
              << "loaded_blocks=" << loader.loadedCount() << "\n"
              << "trace=" << trajectoryPath.string() << "\n";
    return 0;
}

int runRecover(const std::map<std::string, std::string>& options) {
    const LoadedInputs inputs = loadInputs(options);
    const std::size_t viewIndex =
        indexOption(options, "--view-index", 0);
    if (viewIndex >= inputs.views.size()) {
        throw std::runtime_error("view index is out of range");
    }
    preview::StreamingLoader loader(inputs.scene,
                                    preview::StreamingConfig{16});
    loader.start();
    loader.enqueueAll();
    if (!loader.waitUntilIdle()) {
        throw std::runtime_error("streaming loader timed out");
    }
    const auto snapshot = loader.snapshot();
    const std::string snapshotPath = required(options, "--snapshot");
    const preview::SnapshotRecoveryCheck check =
        preview::checkSnapshotRecovery(snapshotPath, *snapshot,
                                       inputs.views[viewIndex].camera);
    if (!check) {
        throw std::runtime_error("snapshot recovery failed: " +
                                 check.diagnostic.toJson());
    }
    std::cout << "{\"snapshot_version\":" << check.recoveredSnapshotVersion
              << ",\"mask_hash\":\"0x" << preview::hex64(check.recoveredMaskHash)
              << "\",\"requires_rerender\":"
              << (check.requiresRerender ? "true" : "false")
              << ",\"changes\":[";
    for (std::size_t i = 0; i < check.changes.size(); ++i) {
        if (i != 0) {
            std::cout << ',';
        }
        std::cout << '"' << check.changes[i] << '"';
    }
    std::cout << "]}\n";
    return 0;
}

int runCrashWriter(const std::map<std::string, std::string>& options) {
    const LoadedInputs inputs = loadInputs(options);
    const std::size_t viewIndex =
        indexOption(options, "--view-index", 0);
    if (viewIndex >= inputs.views.size()) {
        throw std::runtime_error("view index is out of range");
    }
    preview::StreamingLoader loader(inputs.scene,
                                    preview::StreamingConfig{16});
    loader.start();
    loader.enqueueAll();
    if (!loader.waitUntilIdle()) {
        throw std::runtime_error("streaming loader timed out");
    }
    preview::RenderConfig renderConfig;
    renderConfig.pyramid.width = inputs.views[viewIndex].camera.width;
    renderConfig.pyramid.height = inputs.views[viewIndex].camera.height;
    renderConfig.snapshotPath = required(options, "--snapshot");
    preview::Renderer renderer(loader, renderConfig);
    preview::RenderRequest request;
    request.camera = inputs.views[viewIndex].camera;
    const preview::RenderResult result = renderer.render(request);
    if (!result) {
        throw std::runtime_error("crash-writer render failed: " +
                                 result.diagnostic.toJson());
    }
    std::cout << "snapshot_committed=0x"
              << preview::hex64(result.mask.contentHash) << std::endl;
#if defined(_WIN32)
    return 0;
#else
    std::raise(SIGKILL);
    return 1;
#endif
}

}  // namespace

int main(int argc, char** argv) {
    try {
        std::string command;
        if (argc == 1) {
            printUsage();
            return 1;
        }
        const auto options = parseOptions(argc, argv, &command);
        if (command == "render") {
            return runRender(options);
        }
        if (command == "recover") {
            return runRecover(options);
        }
        if (command == "crash-writer") {
            return runCrashWriter(options);
        }
        printUsage();
        throw std::runtime_error("unknown command: " + command);
    } catch (const std::exception& exception) {
        std::cerr << "preview_cli: " << exception.what() << "\n";
        return 1;
    }
}
