#include <algorithm>
#include <atomic>
#include <chrono>
#include <csignal>
#include <cstdint>
#include <filesystem>
#include <iostream>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

#include "png_io.h"
#include "renderer.h"
#include "report.h"
#include "scene.h"
#include "sha256.h"
#include "shadow.h"
#include "snapshot.h"
#include "texture.h"
#include "util.h"

namespace {

using caisson::Scene;
using caisson::SnapshotBlock;
using caisson::StructuredError;
using caisson::sha256Hex;

volatile std::sig_atomic_t gInterrupted = 0;

void signalHandler(int) {
    gInterrupted = 1;
}

struct Options {
    std::filesystem::path scenePath;
    std::filesystem::path outputDirectory = "out";
    std::filesystem::path verifyDirectory;
    int stopAfterBlocks = -1;
    int blockDelayMs = 0;
    bool discardSnapshot = false;
    bool showHelp = false;
};

void printHelp() {
    std::cout
        << "caisson_render --scene <scene.json> [--output <directory>]\n"
        << "               [--stop-after-blocks <n>] [--block-delay-ms <n>]\n"
        << "               [--discard-snapshot] [--verify-against <directory>]\n";
}

bool parseInteger(const std::string& text, int& value) {
    try {
        std::size_t consumed = 0;
        const long parsed = std::stol(text, &consumed, 10);
        if (consumed != text.size()) return false;
        if (parsed < 0 || parsed > 1000000000L) return false;
        value = static_cast<int>(parsed);
        return true;
    } catch (...) {
        return false;
    }
}

bool parseOptions(int argc, char** argv, Options& options, std::string& error) {
    for (int i = 1; i < argc; ++i) {
        const std::string argument = argv[i];
        const auto requireValue = [&](const char* name) -> std::string {
            if (i + 1 >= argc) {
                throw std::runtime_error(std::string("missing value for ") + name);
            }
            return argv[++i];
        };
        try {
            if (argument == "--scene") {
                options.scenePath = requireValue("--scene");
            } else if (argument == "--output") {
                options.outputDirectory = requireValue("--output");
            } else if (argument == "--stop-after-blocks") {
                const std::string value = requireValue("--stop-after-blocks");
                if (!parseInteger(value, options.stopAfterBlocks)) {
                    throw std::runtime_error("invalid --stop-after-blocks");
                }
            } else if (argument == "--block-delay-ms") {
                const std::string value = requireValue("--block-delay-ms");
                if (!parseInteger(value, options.blockDelayMs)) {
                    throw std::runtime_error("invalid --block-delay-ms");
                }
            } else if (argument == "--verify-against") {
                options.verifyDirectory = requireValue("--verify-against");
            } else if (argument == "--discard-snapshot") {
                options.discardSnapshot = true;
            } else if (argument == "--help" || argument == "-h") {
                options.showHelp = true;
            } else if (!argument.empty() && argument[0] != '-' &&
                       options.scenePath.empty()) {
                options.scenePath = argument;
            } else {
                throw std::runtime_error("unknown argument: " + argument);
            }
        } catch (const std::exception& exception) {
            error = exception.what();
            return false;
        }
    }
    if (!options.showHelp && options.scenePath.empty()) {
        error = "--scene is required";
        return false;
    }
    return true;
}

int failWith(const StructuredError& error, int exitCode) {
    std::cerr << error.toJson() << '\n';
    return exitCode;
}

void removeFinalArtifacts(const std::filesystem::path& outputDirectory) {
    std::error_code ec;
    for (const char* file : {
             "image.png",
             "premultiplied.png",
             "ownership.png",
             "shadow_landings.csv",
             "render_report.json",
             "recovery_report.json"}) {
        std::filesystem::remove(outputDirectory / file, ec);
        ec.clear();
    }
}

void copyBlockToImage(
    const Scene& scene,
    const SnapshotBlock& block,
    std::vector<std::uint8_t>& premultiplied,
    std::vector<std::uint8_t>& ownership) {
    for (int row = 0; row < block.rect.height; ++row) {
        for (int column = 0; column < block.rect.width; ++column) {
            const std::size_t sourcePixel =
                static_cast<std::size_t>(row) *
                    static_cast<std::size_t>(block.rect.width) +
                static_cast<std::size_t>(column);
            const std::size_t destinationPixel =
                static_cast<std::size_t>(block.rect.y + row) *
                    static_cast<std::size_t>(scene.image.width) +
                static_cast<std::size_t>(block.rect.x + column);
            std::copy_n(
                block.bytes.begin() +
                    static_cast<std::ptrdiff_t>(sourcePixel * 8U),
                4U,
                premultiplied.begin() +
                    static_cast<std::ptrdiff_t>(destinationPixel * 4U));
            std::copy_n(
                block.bytes.begin() +
                    static_cast<std::ptrdiff_t>(sourcePixel * 8U + 4U),
                4U,
                ownership.begin() +
                    static_cast<std::ptrdiff_t>(destinationPixel * 4U));
        }
    }
}

bool writeRecoveryReport(
    const Scene& scene,
    const std::filesystem::path& outputDirectory,
    int completedBlocks,
    std::string& error) {
    std::ostringstream out;
    out << "{\n";
    out << "  \"recovery_version\": 1,\n";
    out << "  \"status\": \"interrupted\",\n";
    out << "  \"scene_digest\": " << caisson::quote(scene.digest) << ",\n";
    out << "  \"image_width\": " << scene.image.width << ",\n";
    out << "  \"image_height\": " << scene.image.height << ",\n";
    out << "  \"tile_size\": " << scene.image.tileSize << ",\n";
    out << "  \"completed_blocks\": " << completedBlocks << ",\n";
    out << "  \"total_blocks\": " << scene.blockCount() << ",\n";
    out << "  \"next_block\": " << completedBlocks << ",\n";
    out << "  \"snapshot_file\": \"snapshot.json\",\n";
    out << "  \"outputs_committed\": false\n";
    out << "}\n";
    return caisson::atomicWriteText(
        outputDirectory / "recovery_report.json", out.str(), error);
}

bool writeFinalOutputs(
    const Scene& scene,
    const std::filesystem::path& outputDirectory,
    const std::vector<std::uint8_t>& premultiplied,
    const std::vector<std::uint8_t>& ownership,
    const std::vector<std::uint8_t>& display,
    const caisson::ShadowReportData& shadowReport,
    caisson::AggregateStats& aggregate,
    const std::string& textureManifestRelative,
    std::string& error) {
    if (!caisson::writePngRgbaAtomic(
            outputDirectory / "image.png",
            scene.image.width,
            scene.image.height,
            display,
            error)) {
        return false;
    }
    if (!caisson::writePngRgbaAtomic(
            outputDirectory / "premultiplied.png",
            scene.image.width,
            scene.image.height,
            premultiplied,
            error)) {
        return false;
    }
    if (!caisson::writePngRgbaAtomic(
            outputDirectory / "ownership.png",
            scene.image.width,
            scene.image.height,
            ownership,
            error)) {
        return false;
    }

    const std::string landingCsv = caisson::shadowLandingsCsv(shadowReport);
    const std::vector<std::uint8_t> landingBytes(landingCsv.begin(), landingCsv.end());
    if (!caisson::atomicWriteBinary(
            outputDirectory / "shadow_landings.csv", landingBytes, error)) {
        return false;
    }
    const std::string landingSha = caisson::sha256Hex(landingBytes);
    const std::string report = caisson::buildRenderReport(
        scene,
        aggregate,
        shadowReport,
        outputDirectory,
        textureManifestRelative,
        display,
        premultiplied,
        ownership,
        landingSha);
    if (report.empty()) {
        error = "failed to build render report";
        return false;
    }
    return caisson::atomicWriteText(
        outputDirectory / "render_report.json", report, error);
}

}  // namespace

int main(int argc, char** argv) {
    Options options;
    std::string optionError;
    if (!parseOptions(argc, argv, options, optionError)) {
        caisson::StructuredError error("E_CLI");
        error.message = optionError;
        return failWith(error, 2);
    }
    if (options.showHelp) {
        printHelp();
        return 0;
    }

    std::signal(SIGINT, signalHandler);
#ifdef SIGTERM
    std::signal(SIGTERM, signalHandler);
#endif

    caisson::Scene scene;
    caisson::StructuredError sceneError;
    if (!caisson::loadScene(options.scenePath, scene, sceneError)) {
        return failWith(sceneError, 2);
    }

    std::string error;
    if (!caisson::ensureDirectory(options.outputDirectory, error)) {
        caisson::StructuredError ioError("E_IO");
        ioError.message = error;
        return failWith(ioError, 3);
    }
    removeFinalArtifacts(options.outputDirectory);
    if (options.discardSnapshot &&
        !caisson::clearSnapshot(options.outputDirectory, error)) {
        caisson::StructuredError ioError("E_IO");
        ioError.message = error;
        return failWith(ioError, 3);
    }

    caisson::TextureSet textures = caisson::generateTextures(scene);
    const std::string textureManifestRelative = textures.manifestRelativePath;
    if (!caisson::writeTextures(scene, textures, options.outputDirectory, error)) {
        caisson::StructuredError ioError("E_IO");
        ioError.message = error;
        return failWith(ioError, 3);
    }

    caisson::ShadowSampler shadowSampler(scene);
    const caisson::ShadowReportData shadowReport =
        caisson::buildShadowReport(scene, shadowSampler);
    caisson::Renderer renderer(scene, textures, shadowSampler);

    caisson::SnapshotState snapshot;
    const caisson::SnapshotLoadStatus snapshotStatus =
        caisson::loadSnapshot(scene, options.outputDirectory, snapshot, error);
    if (snapshotStatus == caisson::SnapshotLoadStatus::DiscardedCorrupt) {
        std::cout << "snapshot_discarded=1\n";
    } else if (snapshotStatus == caisson::SnapshotLoadStatus::Resumed) {
        std::cout << "snapshot_resumed_blocks=" << snapshot.blocks.size() << '\n';
    }

    const std::size_t pixelCount =
        static_cast<std::size_t>(scene.image.width) *
        static_cast<std::size_t>(scene.image.height);
    std::vector<std::uint8_t> premultiplied(pixelCount * 4U, 0U);
    std::vector<std::uint8_t> ownership(pixelCount * 4U, 0U);
    caisson::AggregateStats aggregate;
    aggregate.resize(scene.layers.size());

    for (const caisson::SnapshotBlock& block : snapshot.blocks) {
        copyBlockToImage(scene, block, premultiplied, ownership);
        aggregate.add(block.stats);
    }

    int completed = static_cast<int>(snapshot.blocks.size());
    const int totalBlocks = scene.blockCount();
    while (completed < totalBlocks) {
        if (options.stopAfterBlocks >= 0 &&
            completed >= options.stopAfterBlocks) {
            if (!writeRecoveryReport(scene, options.outputDirectory, completed, error)) {
                caisson::StructuredError ioError("E_IO");
                ioError.message = error;
                return failWith(ioError, 3);
            }
            std::cout << "interrupted_blocks=" << completed << '\n';
            return 0;
        }
        if (gInterrupted != 0) {
            if (!writeRecoveryReport(scene, options.outputDirectory, completed, error)) {
                caisson::StructuredError ioError("E_IO");
                ioError.message = error;
                return failWith(ioError, 3);
            }
            std::cout << "interrupted_blocks=" << completed << '\n';
            return 0;
        }

        caisson::BlockResult block = renderer.renderBlock(completed);
        caisson::SnapshotBlock committedBlock;
        committedBlock.rect = block.rect;
        committedBlock.relativeFile = caisson::blockRelativePath(completed);
        committedBlock.bytes = block.pixels;
        committedBlock.stats = block.stats;
        copyBlockToImage(scene, committedBlock, premultiplied, ownership);
        aggregate.add(block.stats);
        if (!caisson::commitBlockSnapshot(
                scene,
                options.outputDirectory,
                snapshot.blocks,
                std::move(block),
                error)) {
            caisson::StructuredError ioError("E_IO");
            ioError.message = error;
            return failWith(ioError, 3);
        }
        ++completed;
        if (options.blockDelayMs > 0) {
            std::this_thread::sleep_for(
                std::chrono::milliseconds(options.blockDelayMs));
        }
    }

    const std::vector<std::uint8_t> display =
        caisson::makeDisplayImage(scene, premultiplied);
    if (!writeFinalOutputs(
            scene,
            options.outputDirectory,
            premultiplied,
            ownership,
            display,
            shadowReport,
            aggregate,
            textureManifestRelative,
            error)) {
        caisson::StructuredError ioError("E_IO");
        ioError.message = error;
        return failWith(ioError, 3);
    }

    if (!options.verifyDirectory.empty()) {
        std::string differenceJson;
        if (!caisson::verifyAgainstReference(
                scene,
                options.outputDirectory,
                options.verifyDirectory,
                differenceJson)) {
            std::cerr << differenceJson << '\n';
            return 4;
        }
    }

    if (!caisson::clearSnapshot(options.outputDirectory, error)) {
        caisson::StructuredError ioError("E_IO");
        ioError.message = error;
        return failWith(ioError, 3);
    }
    std::error_code ec;
    std::filesystem::remove(
        options.outputDirectory / "recovery_report.json", ec);
    std::cout << "complete_blocks=" << totalBlocks << '\n';
    return 0;
}

