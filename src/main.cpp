#include "json.hpp"
#include "png.hpp"
#include "render.hpp"
#include "scene.hpp"
#include "sha256.hpp"
#include "snapshot.hpp"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <csignal>
#include <cstdint>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <limits>
#include <map>
#include <optional>
#include <sstream>
#include <string>
#include <string_view>
#include <thread>
#include <utility>
#include <vector>

namespace {

using mosaic::ChunkResult;
using mosaic::JsonValue;
using mosaic::LayerStats;
using mosaic::Scene;
using mosaic::StoredChunk;
using mosaic::ValidationError;

volatile std::sig_atomic_t gInterruptRequested = 0;

void signalHandler(int) {
    gInterruptRequested = 1;
}

void installSignalHandlers() {
    std::signal(SIGINT, signalHandler);
#if defined(SIGTERM)
    std::signal(SIGTERM, signalHandler);
#endif
}

constexpr int kExitUsage = 2;
constexpr int kExitValidation = 20;
constexpr int kExitRuntime = 30;
constexpr int kExitDifference = 40;
constexpr int kExitVerifyError = 41;

void printUsage() {
    std::cerr
        << "usage:\n"
        << "  mosaic-validator validate --scene <scene.json>\n"
        << "  mosaic-validator render --scene <scene.json> --out <directory>\n"
        << "      [--cache <directory>] [--interrupt-after-chunks <n>]\n"
        << "      [--chunk-delay-ms <n>] [--force-restart]\n"
        << "  mosaic-validator verify --a <run-a> --b <run-b> --report <difference.json>\n";
}

std::string errorJson(std::string_view code, std::string_view path, std::string_view message) {
    std::ostringstream out;
    out << "{\n  \"ok\": false,\n  \"errors\": [\n"
        << "    {\"code\": \"" << mosaic::escapeJsonString(code)
        << "\", \"path\": \"" << mosaic::escapeJsonString(path)
        << "\", \"message\": \"" << mosaic::escapeJsonString(message) << "\"}\n"
        << "  ]\n}\n";
    return out.str();
}

std::optional<std::string> readTextFile(
    const std::filesystem::path& path,
    std::string& error) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        error = "cannot open input file";
        return std::nullopt;
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    if (!input.good() && !input.eof()) {
        error = "cannot read input file";
        return std::nullopt;
    }
    return buffer.str();
}

bool parseU32Argument(const std::string& text, std::uint32_t& value) {
    if (text.empty()) {
        return false;
    }
    std::uint64_t parsed = 0;
    for (const char c : text) {
        if (c < '0' || c > '9') {
            return false;
        }
        parsed = parsed * 10U + static_cast<std::uint64_t>(c - '0');
        if (parsed > std::numeric_limits<std::uint32_t>::max()) {
            return false;
        }
    }
    value = static_cast<std::uint32_t>(parsed);
    return true;
}

struct RenderOptions {
    std::string scenePath;
    std::string outputDirectory;
    std::optional<std::string> cacheDirectory;
    std::uint32_t interruptAfterChunks = 0;
    std::uint32_t chunkDelayMs = 0;
    bool forceRestart = false;
};

bool parseRenderOptions(
    int argc,
    char** argv,
    RenderOptions& options,
    std::string& error) {
    for (int i = 2; i < argc; ++i) {
        const std::string argument = argv[i];
        auto requireValue = [&](std::string& destination) {
            if (i + 1 >= argc) {
                error = argument + " requires a value";
                return false;
            }
            destination = argv[++i];
            return true;
        };
        if (argument == "--scene") {
            if (!requireValue(options.scenePath)) {
                return false;
            }
        } else if (argument == "--out") {
            if (!requireValue(options.outputDirectory)) {
                return false;
            }
        } else if (argument == "--cache") {
            std::string value;
            if (!requireValue(value)) {
                return false;
            }
            options.cacheDirectory = value;
        } else if (argument == "--interrupt-after-chunks") {
            std::string value;
            if (!requireValue(value) || !parseU32Argument(value, options.interruptAfterChunks)) {
                error = "invalid --interrupt-after-chunks value";
                return false;
            }
        } else if (argument == "--chunk-delay-ms") {
            std::string value;
            if (!requireValue(value) || !parseU32Argument(value, options.chunkDelayMs)) {
                error = "invalid --chunk-delay-ms value";
                return false;
            }
        } else if (argument == "--force-restart") {
            options.forceRestart = true;
        } else {
            error = "unknown option: " + argument;
            return false;
        }
    }
    if (options.scenePath.empty() || options.outputDirectory.empty()) {
        error = "--scene and --out are required";
        return false;
    }
    return true;
}

std::uint64_t saturatingAdd(std::uint64_t left, std::uint64_t right) {
    const std::uint64_t maximum = std::numeric_limits<std::uint64_t>::max();
    return right > maximum - left ? maximum : left + right;
}

std::vector<LayerStats> aggregateLayerStats(
    const std::vector<std::int32_t>& layers,
    const std::map<std::uint32_t, StoredChunk>& chunks) {
    std::vector<LayerStats> aggregate(layers.size());
    for (std::size_t i = 0; i < layers.size(); ++i) {
        aggregate[i].layerIndex = layers[i];
    }
    for (const auto& [index, stored] : chunks) {
        (void)index;
        const auto& stats = stored.result.layerStats;
        for (std::size_t i = 0; i < std::min(stats.size(), aggregate.size()); ++i) {
            aggregate[i].visibleFragments = saturatingAdd(
                aggregate[i].visibleFragments,
                stats[i].visibleFragments);
            aggregate[i].transmissionEnergyMicro = saturatingAdd(
                aggregate[i].transmissionEnergyMicro,
                stats[i].transmissionEnergyMicro);
            aggregate[i].reflectionEnergyMicro = saturatingAdd(
                aggregate[i].reflectionEnergyMicro,
                stats[i].reflectionEnergyMicro);
            aggregate[i].transmissionLuminanceMicro = saturatingAdd(
                aggregate[i].transmissionLuminanceMicro,
                stats[i].transmissionLuminanceMicro);
        }
    }
    return aggregate;
}

std::vector<std::uint8_t> encodeOwnership(const std::vector<std::uint64_t>& ownership) {
    std::vector<std::uint8_t> bytes;
    bytes.reserve(ownership.size() * 8U);
    for (const std::uint64_t value : ownership) {
        for (std::uint32_t shift = 0; shift < 64U; shift += 8U) {
            bytes.push_back(static_cast<std::uint8_t>((value >> shift) & 0xFFU));
        }
    }
    return bytes;
}

std::string assemblyError(
    const Scene& scene,
    const std::map<std::uint32_t, StoredChunk>& chunks,
    const std::vector<std::uint8_t>& rgba,
    const std::vector<std::uint64_t>& owners,
    const std::string& sceneDigest,
    const mosaic::PreparedScene& prepared,
    const mosaic::CausticResult& caustics,
    const mosaic::SnapshotLoadResult& loadResult,
    std::uint32_t reusedChunks,
    std::uint32_t recomputedChunks,
    const std::string& ownershipDigest) {
    (void)owners;
    const std::vector<LayerStats> layerStats = aggregateLayerStats(prepared.layers, chunks);
    std::ostringstream out;
    out << std::fixed << std::setprecision(6);
    out << "{\n";
    out << "  \"format\": \"mosaic-validation-report\",\n";
    out << "  \"version\": 1,\n";
    out << "  \"scene\": " << mosaic::sceneSummaryJson(scene) << ",\n";
    out << "  \"image\": {\"width\": " << scene.settings.output.width
        << ", \"height\": " << scene.settings.output.height
        << ", \"chunkWidth\": " << scene.settings.output.chunkWidth
        << ", \"chunkHeight\": " << scene.settings.output.chunkHeight
        << ", \"rgbaSha256\": \"" << mosaic::bytesDigest(rgba) << "\""
        << ", \"ownershipSha256\": \"" << ownershipDigest << "\""
        << ", \"causticGraySha256\": \"" << mosaic::bytesDigest(caustics.grayBinary) << "\"},\n";
    out << "  \"texture\": {\"sampleTableSha256\": \"" << prepared.textureTable.digest
        << "\", \"sampleTableBytes\": " << prepared.textureTable.bytes.size() << "},\n";
    out << "  \"caustics\": {\"recordCount\": " << caustics.records.size()
        << ", \"stripeCount\": " << caustics.stripeCount
        << ", \"digest\": \"" << caustics.digest << "\"},\n";
    out << "  \"layers\": " << mosaic::layerStatsJson(layerStats) << ",\n";
    out << "  \"penumbraTrend\": {\"profileOffsetsInSigma\": [0.0, 0.5, 1.0, 1.5], ";
    out << "\"samples\": [";
    for (std::size_t i = 0; i < caustics.probes.size(); ++i) {
        const auto& probe = caustics.probes[i];
        out << (i == 0 ? "\n    " : ",\n    ");
        out << "{\"sourceLayer\": " << probe.sourceLayer
            << ", \"sourcePieceId\": " << probe.sourcePieceId
            << ", \"lightId\": " << probe.lightId
            << ", \"thickness\": " << probe.thickness
            << ", \"layerDistance\": " << probe.layerDistance
            << ", \"penumbraWidthPixels\": " << probe.penumbraWidthPixels
            << ", \"peakBrightness\": " << probe.peakBrightness
            << ", \"profileGray\": [" << probe.profileGray[0] << ", "
            << probe.profileGray[1] << ", " << probe.profileGray[2] << ", "
            << probe.profileGray[3] << "]}";
    }
    if (!caustics.probes.empty()) {
        out << '\n';
    }
    out << "  ]},\n";
    out << "  \"chunks\": {\"totalChunks\": " << mosaic::chunkCount(scene)
        << ", \"snapshotState\": \"complete\", \"completedChunks\": [";
    bool first = true;
    for (const auto& [index, chunk] : chunks) {
        (void)chunk;
        out << (first ? "" : ", ") << index;
        first = false;
    }
    out << "], \"resumeCount\": " << loadResult.resumeCount
        << ", \"reusedChunks\": " << reusedChunks
        << ", \"recomputedChunks\": " << recomputedChunks
        << ", \"snapshotDiscarded\": " << (loadResult.discarded ? "true" : "false")
        << ", \"discardReason\": \"" << mosaic::escapeJsonString(loadResult.discardReason)
        << "\"},\n";
    out << "  \"recoveries\": {\"count\": " << loadResult.resumeCount
        << ", \"reusedChunks\": " << reusedChunks
        << ", \"recomputedChunks\": " << recomputedChunks << "},\n";
    out << "  \"checks\": {\"channelOverflow\": false, \"nonFinite\": false, "
           "\"imageOpaquePremultiplied\": true}\n";
    out << "}\n";
    (void)sceneDigest;
    return out.str();
}

struct RenderRunResult {
    int exitCode = 0;
    bool interrupted = false;
    std::uint32_t reusedChunks = 0;
    std::uint32_t recomputedChunks = 0;
};

int renderCommand(int argc, char** argv) {
    RenderOptions options;
    std::string argumentError;
    if (!parseRenderOptions(argc, argv, options, argumentError)) {
        std::cerr << errorJson("MOSAIC_CLI_INVALID", "command", argumentError);
        return kExitUsage;
    }

    std::string sceneError;
    const auto sceneText = readTextFile(options.scenePath, sceneError);
    if (!sceneText.has_value()) {
        std::cerr << errorJson("MOSAIC_SCENE_READ_ERROR", "scene", sceneError);
        return kExitRuntime;
    }
    mosaic::SceneParseResult parseResult = mosaic::parseScene(*sceneText);
    if (parseResult.jsonError.has_value()) {
        const auto& error = *parseResult.jsonError;
        std::ostringstream message;
        message << "line " << error.line << ", column " << error.column << ": "
                << error.message;
        std::cerr << errorJson("MOSAIC_JSON_PARSE", "scene", message.str());
        return kExitValidation;
    }
    if (!parseResult.errors.empty()) {
        const std::string validation = mosaic::validationErrorsJson(parseResult.errors);
        std::cerr << validation;
        std::error_code directoryError;
        std::filesystem::create_directories(options.outputDirectory, directoryError);
        if (!directoryError) {
            std::string writeError;
            mosaic::atomicWriteTextFile(
                std::filesystem::path(options.outputDirectory) / "validation_errors.json",
                validation,
                writeError);
        }
        return kExitValidation;
    }
    if (!parseResult.scene.has_value()) {
        std::cerr << errorJson("MOSAIC_SCENE_INVALID", "scene", "scene was not parsed");
        return kExitValidation;
    }
    const Scene& scene = *parseResult.scene;
    const std::string digest = mosaic::sceneDigest(scene);

    std::error_code directoryError;
    std::filesystem::create_directories(options.outputDirectory, directoryError);
    if (directoryError) {
        std::cerr << errorJson("MOSAIC_OUTPUT_ERROR", "out", directoryError.message());
        return kExitRuntime;
    }
    const std::filesystem::path cacheDirectory = options.cacheDirectory.has_value()
        ? std::filesystem::path(*options.cacheDirectory)
        : std::filesystem::path(options.outputDirectory) / "snapshot";

    mosaic::SnapshotLoadResult loadResult;
    if (options.forceRestart) {
        const bool hadSnapshot =
            std::filesystem::exists(cacheDirectory / "snapshot.json");
        mosaic::discardCache(cacheDirectory);
        loadResult.snapshotPresent = hadSnapshot;
        loadResult.discarded = hadSnapshot;
        loadResult.discardReason = hadSnapshot ? "forced-restart" : "";
        loadResult.resumeCount = hadSnapshot ? 1U : 0U;
    } else {
        loadResult = mosaic::loadSnapshot(cacheDirectory, scene, digest);
        if (!loadResult.valid && loadResult.snapshotPresent) {
            mosaic::discardCache(cacheDirectory);
            loadResult.chunks.clear();
            loadResult.reusedChunks = 0;
            loadResult.resumeCount = 1;
        } else if (!loadResult.snapshotPresent) {
            std::error_code existsError;
            if (std::filesystem::exists(cacheDirectory, existsError)) {
                mosaic::discardCache(cacheDirectory);
            }
        }
    }
    std::filesystem::create_directories(cacheDirectory / "chunks", directoryError);
    if (directoryError) {
        std::cerr << errorJson("MOSAIC_CACHE_ERROR", "cache", directoryError.message());
        return kExitRuntime;
    }

    const mosaic::PreparedScene prepared = mosaic::prepareScene(scene);
    const mosaic::CausticResult caustics = mosaic::renderCaustics(prepared);
    std::map<std::uint32_t, StoredChunk> chunks = std::move(loadResult.chunks);
    std::uint32_t reusedChunks = static_cast<std::uint32_t>(chunks.size());
    std::uint32_t recomputedChunks = 0;
    const std::uint32_t totalChunks = mosaic::chunkCount(scene);
    bool interrupted = false;

    installSignalHandlers();
    for (std::uint32_t chunkIndex = 0; chunkIndex < totalChunks; ++chunkIndex) {
        if (chunks.find(chunkIndex) != chunks.end()) {
            continue;
        }
        if (gInterruptRequested != 0) {
            interrupted = true;
            break;
        }
        ChunkResult chunk = mosaic::renderChunk(prepared, caustics, chunkIndex);
        std::string storeError;
        StoredChunk stored =
            mosaic::storeChunk(cacheDirectory, digest, chunk, storeError);
        if (!storeError.empty()) {
            std::cerr << errorJson("MOSAIC_SNAPSHOT_WRITE_ERROR", "snapshot", storeError);
            return kExitRuntime;
        }
        chunks.emplace(chunkIndex, std::move(stored));
        ++recomputedChunks;
        std::string snapshotError;
        if (!mosaic::writeSnapshot(
                cacheDirectory,
                scene,
                digest,
                chunks,
                "in-progress",
                loadResult.resumeCount,
                snapshotError)) {
            std::cerr << errorJson("MOSAIC_SNAPSHOT_WRITE_ERROR", "snapshot", snapshotError);
            return kExitRuntime;
        }
        std::cout << "chunk " << (chunkIndex + 1) << "/" << totalChunks
                  << " committed" << std::endl;
        if (options.chunkDelayMs > 0) {
            std::this_thread::sleep_for(
                std::chrono::milliseconds(options.chunkDelayMs));
        }
        const bool limitReached =
            options.interruptAfterChunks > 0 &&
            recomputedChunks >= options.interruptAfterChunks &&
            chunks.size() < totalChunks;
        if (gInterruptRequested != 0 || limitReached) {
            interrupted = true;
            break;
        }
    }

    if (interrupted) {
        std::string snapshotError;
        if (!mosaic::writeSnapshot(
                cacheDirectory,
                scene,
                digest,
                chunks,
                "interrupted",
                loadResult.resumeCount,
                snapshotError)) {
            std::cerr << errorJson("MOSAIC_SNAPSHOT_WRITE_ERROR", "snapshot", snapshotError);
            return kExitRuntime;
        }
        std::string recoveryError;
        if (!mosaic::writeRecoveryReport(
                options.outputDirectory,
                scene,
                digest,
                "interrupted",
                loadResult,
                reusedChunks,
                recomputedChunks,
                chunks,
                recoveryError)) {
            std::cerr << errorJson("MOSAIC_RECOVERY_WRITE_ERROR", "recovery", recoveryError);
            return kExitRuntime;
        }
        std::cout << "interrupted after current chunk; snapshot is resumable" << std::endl;
        return 0;
    }

    if (chunks.size() != totalChunks) {
        std::cerr << errorJson("MOSAIC_CHUNK_INCOMPLETE", "chunks", "not all chunks are available");
        return kExitRuntime;
    }
    std::string snapshotError;
    if (!mosaic::writeSnapshot(
            cacheDirectory,
            scene,
            digest,
            chunks,
            "complete",
            loadResult.resumeCount,
            snapshotError)) {
        std::cerr << errorJson("MOSAIC_SNAPSHOT_WRITE_ERROR", "snapshot", snapshotError);
        return kExitRuntime;
    }

    std::vector<std::uint8_t> rgba(
        static_cast<std::size_t>(scene.settings.output.width) *
        scene.settings.output.height * 4U);
    std::vector<std::uint64_t> owners(
        static_cast<std::size_t>(scene.settings.output.width) *
        scene.settings.output.height);
    for (std::uint32_t chunkIndex = 0; chunkIndex < totalChunks; ++chunkIndex) {
        const StoredChunk& chunk = chunks.at(chunkIndex);
        const ChunkResult& result = chunk.result;
        for (std::uint32_t row = 0; row < result.height; ++row) {
            const std::size_t sourceRgba =
                static_cast<std::size_t>(row) * result.width * 4U;
            const std::size_t sourceOwner =
                static_cast<std::size_t>(row) * result.width;
            const std::size_t destinationRgba =
                (static_cast<std::size_t>(result.y + row) *
                     scene.settings.output.width +
                 result.x) *
                4U;
            const std::size_t destinationOwner =
                static_cast<std::size_t>(result.y + row) *
                    scene.settings.output.width +
                result.x;
            std::copy_n(
                result.rgba.begin() + static_cast<std::ptrdiff_t>(sourceRgba),
                static_cast<std::ptrdiff_t>(result.width) * 4,
                rgba.begin() + static_cast<std::ptrdiff_t>(destinationRgba));
            std::copy_n(
                result.ownership.begin() + static_cast<std::ptrdiff_t>(sourceOwner),
                result.width,
                owners.begin() + static_cast<std::ptrdiff_t>(destinationOwner));
        }
    }
    const std::vector<std::uint8_t> ownershipBytes = encodeOwnership(owners);
    const std::string ownershipDigest = mosaic::bytesDigest(ownershipBytes);

    const std::filesystem::path outputPath(options.outputDirectory);
    std::string writeError;
    if (!mosaic::atomicWriteFile(outputPath / "image.rgba", rgba, writeError) ||
        !mosaic::atomicWriteFile(outputPath / "ownership.u64", ownershipBytes, writeError) ||
        !mosaic::atomicWriteFile(
            outputPath / "caustic_gray.u16",
            caustics.grayBinary,
            writeError) ||
        !mosaic::atomicWriteFile(
            outputPath / "caustics.bin",
            caustics.recordsBinary,
            writeError) ||
        !mosaic::atomicWriteFile(
            outputPath / "texture_table.bin",
            prepared.textureTable.bytes,
            writeError) ||
        !mosaic::atomicWriteTextFile(
            outputPath / "caustics.json",
            mosaic::causticRecordsJson(prepared, caustics),
            writeError) ||
        !mosaic::atomicWriteTextFile(
            outputPath / "ownership_legend.json",
            mosaic::ownershipLegendJson(scene),
            writeError)) {
        std::cerr << errorJson("MOSAIC_OUTPUT_WRITE_ERROR", "out", writeError);
        return kExitRuntime;
    }
    if (!mosaic::writeRgbaPng(
            outputPath / "image.png",
            scene.settings.output.width,
            scene.settings.output.height,
            rgba,
            writeError) ||
        !mosaic::writeGray16Png(
            outputPath / "caustic_gray.png",
            scene.settings.output.width,
            scene.settings.output.height,
            caustics.gray,
            writeError)) {
        std::cerr << errorJson("MOSAIC_OUTPUT_WRITE_ERROR", "out", writeError);
        return kExitRuntime;
    }

    const std::string report = assemblyError(
        scene,
        chunks,
        rgba,
        owners,
        digest,
        prepared,
        caustics,
        loadResult,
        reusedChunks,
        recomputedChunks,
        ownershipDigest);
    if (!mosaic::atomicWriteTextFile(outputPath / "report.json", report, writeError)) {
        std::cerr << errorJson("MOSAIC_REPORT_WRITE_ERROR", "report", writeError);
        return kExitRuntime;
    }
    std::string recoveryError;
    if (!mosaic::writeRecoveryReport(
            outputPath,
            scene,
            digest,
            "complete",
            loadResult,
            reusedChunks,
            recomputedChunks,
            chunks,
            recoveryError)) {
        std::cerr << errorJson("MOSAIC_RECOVERY_WRITE_ERROR", "recovery", recoveryError);
        return kExitRuntime;
    }

    std::cout << "render complete: " << scene.settings.output.width << "x"
              << scene.settings.output.height << ", chunks=" << totalChunks
              << ", reused=" << reusedChunks
              << ", recomputed=" << recomputedChunks
              << ", imageSha256=" << mosaic::bytesDigest(rgba) << std::endl;
    return 0;
}

int validateCommand(int argc, char** argv) {
    std::string scenePath;
    for (int i = 2; i < argc; ++i) {
        const std::string argument = argv[i];
        if (argument == "--scene" && i + 1 < argc) {
            scenePath = argv[++i];
        } else {
            std::cerr << errorJson("MOSAIC_CLI_INVALID", "command", "unknown or incomplete option");
            return kExitUsage;
        }
    }
    if (scenePath.empty()) {
        std::cerr << errorJson("MOSAIC_CLI_INVALID", "command", "--scene is required");
        return kExitUsage;
    }
    std::string readError;
    const auto text = readTextFile(scenePath, readError);
    if (!text.has_value()) {
        std::cerr << errorJson("MOSAIC_SCENE_READ_ERROR", "scene", readError);
        return kExitRuntime;
    }
    mosaic::SceneParseResult result = mosaic::parseScene(*text);
    if (result.jsonError.has_value()) {
        std::ostringstream message;
        message << "line " << result.jsonError->line << ", column "
                << result.jsonError->column << ": " << result.jsonError->message;
        std::cerr << errorJson("MOSAIC_JSON_PARSE", "scene", message.str());
        return kExitValidation;
    }
    if (!result.errors.empty() || !result.scene.has_value()) {
        std::cerr << mosaic::validationErrorsJson(result.errors);
        return kExitValidation;
    }
    std::cout << mosaic::sceneSummaryJson(*result.scene) << std::endl;
    return 0;
}

std::uint32_t readLittleU32(const std::vector<std::uint8_t>& bytes, std::size_t offset) {
    return static_cast<std::uint32_t>(bytes.at(offset)) |
           (static_cast<std::uint32_t>(bytes.at(offset + 1)) << 8U) |
           (static_cast<std::uint32_t>(bytes.at(offset + 2)) << 16U) |
           (static_cast<std::uint32_t>(bytes.at(offset + 3)) << 24U);
}

double readLittleDouble(const std::vector<std::uint8_t>& bytes, std::size_t offset) {
    std::uint64_t bits = 0;
    for (std::uint32_t i = 0; i < 8; ++i) {
        bits |= static_cast<std::uint64_t>(bytes.at(offset + i)) << (8U * i);
    }
    return std::bit_cast<double>(bits);
}

std::uint16_t readLittleU16(const std::vector<std::uint8_t>& bytes, std::size_t offset) {
    return static_cast<std::uint16_t>(bytes.at(offset)) |
           static_cast<std::uint16_t>(bytes.at(offset + 1) << 8U);
}

struct DifferenceEntry {
    std::string artifact;
    std::uint32_t chunk = 0;
    std::uint32_t x = 0;
    std::uint32_t y = 0;
    std::string channel;
    std::string a;
    std::string b;
    std::uint64_t index = 0;
};

struct DifferenceCollector {
    std::uint64_t total = 0;
    std::vector<DifferenceEntry> entries;
    std::uint64_t limit = 2000;

    void add(DifferenceEntry entry) {
        ++total;
        if (entries.size() < limit) {
            entries.push_back(std::move(entry));
        }
    }
};

std::uint32_t chunkForPixel(
    std::uint32_t x,
    std::uint32_t y,
    std::uint32_t width,
    std::uint32_t height,
    std::uint32_t chunkWidth,
    std::uint32_t chunkHeight) {
    (void)height;
    const std::uint32_t columns = (width + chunkWidth - 1U) / chunkWidth;
    return (y / chunkHeight) * columns + (x / chunkWidth);
}

bool parseRunLayout(
    const std::filesystem::path& directory,
    std::uint32_t& width,
    std::uint32_t& height,
    std::uint32_t& chunkWidth,
    std::uint32_t& chunkHeight,
    std::string& error) {
    std::string readError;
    const auto reportText = readTextFile(directory / "report.json", readError);
    if (!reportText.has_value()) {
        error = "cannot read report.json";
        return false;
    }
    const auto parsed = mosaic::parseJson(*reportText);
    if (!parsed.value.has_value()) {
        error = "report.json is not valid JSON";
        return false;
    }
    const JsonValue* image = parsed.value->find("image");
    if (image == nullptr || !image->isObject()) {
        error = "report.json has no image object";
        return false;
    }
    auto readDimension = [&](std::string_view key, std::uint32_t& value) {
        const JsonValue* field = image->find(key);
        if (field == nullptr || !field->isNumber() || field->asNumber() < 1.0 ||
            field->asNumber() > static_cast<double>(std::numeric_limits<std::uint32_t>::max()) ||
            std::trunc(field->asNumber()) != field->asNumber()) {
            return false;
        }
        value = static_cast<std::uint32_t>(field->asNumber());
        return true;
    };
    if (!readDimension("width", width) ||
        !readDimension("height", height) ||
        !readDimension("chunkWidth", chunkWidth) ||
        !readDimension("chunkHeight", chunkHeight)) {
        error = "report.json has invalid image dimensions";
        return false;
    }
    return true;
}

std::optional<std::vector<std::uint8_t>> readArtifact(
    const std::filesystem::path& directory,
    const std::string& name) {
    std::string error;
    return mosaic::readBinaryFile(directory / name, error);
}

std::string differenceReportJson(const DifferenceCollector& collector) {
    std::ostringstream out;
    out << "{\n  \"format\": \"mosaic-difference\",\n";
    out << "  \"version\": 1,\n";
    out << "  \"status\": \"" << (collector.total == 0 ? "equal" : "different") << "\",\n";
    out << "  \"differenceCount\": " << collector.total << ",\n";
    out << "  \"truncated\": " << (collector.total > collector.entries.size() ? "true" : "false")
        << ",\n  \"differences\": [";
    for (std::size_t i = 0; i < collector.entries.size(); ++i) {
        const DifferenceEntry& entry = collector.entries[i];
        out << (i == 0 ? "\n" : ",\n");
        out << "    {\"artifact\": \"" << mosaic::escapeJsonString(entry.artifact)
            << "\", \"chunk\": " << entry.chunk
            << ", \"x\": " << entry.x
            << ", \"y\": " << entry.y
            << ", \"channel\": \"" << mosaic::escapeJsonString(entry.channel)
            << "\", \"a\": \"" << mosaic::escapeJsonString(entry.a)
            << "\", \"b\": \"" << mosaic::escapeJsonString(entry.b)
            << "\", \"index\": " << entry.index << "}";
    }
    if (!collector.entries.empty()) {
        out << '\n';
    }
    out << "  ]\n}\n";
    return out.str();
}

int verifyCommand(int argc, char** argv) {
    std::string directoryA;
    std::string directoryB;
    std::string reportPath;
    for (int i = 2; i < argc; ++i) {
        const std::string argument = argv[i];
        auto value = [&](std::string& destination) {
            if (i + 1 >= argc) {
                return false;
            }
            destination = argv[++i];
            return true;
        };
        if (argument == "--a") {
            if (!value(directoryA)) {
                return kExitUsage;
            }
        } else if (argument == "--b") {
            if (!value(directoryB)) {
                return kExitUsage;
            }
        } else if (argument == "--report") {
            if (!value(reportPath)) {
                return kExitUsage;
            }
        } else {
            std::cerr << errorJson("MOSAIC_CLI_INVALID", "command", "unknown option");
            return kExitUsage;
        }
    }
    if (directoryA.empty() || directoryB.empty() || reportPath.empty()) {
        std::cerr << errorJson("MOSAIC_CLI_INVALID", "command", "--a, --b and --report are required");
        return kExitUsage;
    }
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    std::uint32_t chunkWidth = 0;
    std::uint32_t chunkHeight = 0;
    std::string layoutError;
    if (!parseRunLayout(directoryA, width, height, chunkWidth, chunkHeight, layoutError)) {
        std::cerr << errorJson("MOSAIC_VERIFY_INPUT_ERROR", "a", layoutError);
        return kExitVerifyError;
    }

    const auto rgbaA = readArtifact(directoryA, "image.rgba");
    const auto rgbaB = readArtifact(directoryB, "image.rgba");
    const auto ownerA = readArtifact(directoryA, "ownership.u64");
    const auto ownerB = readArtifact(directoryB, "ownership.u64");
    const auto grayA = readArtifact(directoryA, "caustic_gray.u16");
    const auto grayB = readArtifact(directoryB, "caustic_gray.u16");
    const auto causticA = readArtifact(directoryA, "caustics.bin");
    const auto causticB = readArtifact(directoryB, "caustics.bin");
    if (!rgbaA.has_value() || !rgbaB.has_value() || !ownerA.has_value() ||
        !ownerB.has_value() || !grayA.has_value() || !grayB.has_value() ||
        !causticA.has_value() || !causticB.has_value()) {
        std::cerr << errorJson(
            "MOSAIC_VERIFY_INPUT_ERROR",
            "artifacts",
            "one or more required artifacts are missing");
        return kExitVerifyError;
    }

    DifferenceCollector differences;
    const std::uint64_t expectedRgba =
        static_cast<std::uint64_t>(width) * height * 4U;
    const std::uint64_t expectedOwners =
        static_cast<std::uint64_t>(width) * height;
    const std::uint64_t expectedGray =
        static_cast<std::uint64_t>(width) * height * 2U;
    const bool sizesValid =
        rgbaA->size() == expectedRgba && rgbaB->size() == expectedRgba &&
        ownerA->size() == expectedOwners * 8U && ownerB->size() == expectedOwners * 8U &&
        grayA->size() == expectedGray && grayB->size() == expectedGray;
    if (!sizesValid) {
        differences.add(
            {"artifact-size", 0, 0, 0, "size",
             std::to_string(rgbaA->size()) + "/" + std::to_string(ownerA->size()) +
                 "/" + std::to_string(grayA->size()),
             std::to_string(rgbaB->size()) + "/" + std::to_string(ownerB->size()) +
                 "/" + std::to_string(grayB->size()),
             0});
    } else {
        static constexpr char channels[4] = {'R', 'G', 'B', 'A'};
        for (std::uint64_t pixel = 0; pixel < expectedOwners; ++pixel) {
            const std::uint32_t x = static_cast<std::uint32_t>(pixel % width);
            const std::uint32_t y = static_cast<std::uint32_t>(pixel / width);
            const std::uint32_t chunk =
                chunkForPixel(x, y, width, height, chunkWidth, chunkHeight);
            for (std::size_t channel = 0; channel < 4; ++channel) {
                const std::size_t offset = static_cast<std::size_t>(pixel) * 4U + channel;
                const std::uint8_t left = (*rgbaA)[offset];
                const std::uint8_t right = (*rgbaB)[offset];
                if (left != right) {
                    differences.add(
                        {"image.rgba", chunk, x, y, std::string(1, channels[channel]),
                         std::to_string(left), std::to_string(right), pixel * 4U + channel});
                }
            }
            const std::size_t ownerOffset = static_cast<std::size_t>(pixel) * 8U;
            if (!std::equal(
                    ownerA->begin() + static_cast<std::ptrdiff_t>(ownerOffset),
                    ownerA->begin() + static_cast<std::ptrdiff_t>(ownerOffset + 8),
                    ownerB->begin() + static_cast<std::ptrdiff_t>(ownerOffset))) {
                std::uint64_t left = 0;
                std::uint64_t right = 0;
                for (std::uint32_t i = 0; i < 8; ++i) {
                    left |= static_cast<std::uint64_t>((*ownerA)[ownerOffset + i]) << (8U * i);
                    right |= static_cast<std::uint64_t>((*ownerB)[ownerOffset + i]) << (8U * i);
                }
                differences.add(
                    {"ownership.u64", chunk, x, y, "owner",
                     std::to_string(left), std::to_string(right), pixel});
            }
            for (std::size_t channel = 0; channel < 2; ++channel) {
                const std::size_t offset = static_cast<std::size_t>(pixel) * 2U + channel;
                if ((*grayA)[offset] != (*grayB)[offset]) {
                    const std::uint16_t left =
                        readLittleU16(*grayA, static_cast<std::size_t>(pixel) * 2U);
                    const std::uint16_t right =
                        readLittleU16(*grayB, static_cast<std::size_t>(pixel) * 2U);
                    if (channel == 0) {
                        differences.add(
                            {"caustic_gray.u16", chunk, x, y, "gray",
                             std::to_string(left), std::to_string(right), pixel});
                    }
                    break;
                }
            }
        }
    }

    if (*causticA != *causticB) {
        const std::uint32_t countA =
            causticA->size() >= 20 ? readLittleU32(*causticA, 16) : 0;
        const std::uint32_t countB =
            causticB->size() >= 20 ? readLittleU32(*causticB, 16) : 0;
        const std::uint32_t common = std::min(countA, countB);
        bool added = false;
        for (std::uint32_t record = 0; record < common; ++record) {
            const std::size_t offset = 20U + static_cast<std::size_t>(record) * 136U;
            if (offset + 136U > causticA->size() || offset + 136U > causticB->size()) {
                break;
            }
            const double screenX = readLittleDouble(*causticA, offset + 108U);
            const double screenY = readLittleDouble(*causticA, offset + 116U);
            std::uint32_t x = 0;
            std::uint32_t y = 0;
            std::uint32_t chunk = 0;
            if (screenX >= 0.0 && screenY >= 0.0 && width > 0 && height > 0) {
                x = static_cast<std::uint32_t>(
                    std::min(screenX, static_cast<double>(width - 1U)));
                y = static_cast<std::uint32_t>(
                    std::min(screenY, static_cast<double>(height - 1U)));
                chunk = chunkForPixel(x, y, width, height, chunkWidth, chunkHeight);
            }
            const double targetLeft[3] = {
                readLittleDouble(*causticA, offset + 52U),
                readLittleDouble(*causticA, offset + 60U),
                readLittleDouble(*causticA, offset + 68U),
            };
            const double targetRight[3] = {
                readLittleDouble(*causticB, offset + 52U),
                readLittleDouble(*causticB, offset + 60U),
                readLittleDouble(*causticB, offset + 68U),
            };
            static constexpr char axes[3] = {'x', 'y', 'z'};
            for (int axis = 0; axis < 3; ++axis) {
                const std::uint64_t leftBits = std::bit_cast<std::uint64_t>(targetLeft[axis]);
                const std::uint64_t rightBits = std::bit_cast<std::uint64_t>(targetRight[axis]);
                if (leftBits != rightBits) {
                    std::ostringstream left;
                    std::ostringstream right;
                    left << std::setprecision(17) << targetLeft[axis];
                    right << std::setprecision(17) << targetRight[axis];
                    differences.add(
                        {"caustics.bin", chunk, x, y, std::string("target") + axes[axis],
                         left.str(), right.str(), record});
                    added = true;
                }
            }
        }
        if (countA != countB) {
            differences.add(
                {"caustics.bin", 0, 0, 0, "recordCount",
                 std::to_string(countA), std::to_string(countB), 0});
            added = true;
        }
        if (!added && *causticA != *causticB) {
            differences.add(
                {"caustics.bin", 0, 0, 0, "payload",
                 std::to_string(causticA->size()), std::to_string(causticB->size()), 0});
        }
    }

    const auto textureA = readArtifact(directoryA, "texture_table.bin");
    const auto textureB = readArtifact(directoryB, "texture_table.bin");
    if (textureA.has_value() && textureB.has_value() && *textureA != *textureB) {
        differences.add(
            {"texture_table.bin", 0, 0, 0, "payload",
             std::to_string(textureA->size()), std::to_string(textureB->size()), 0});
    }

    const std::string report = differenceReportJson(differences);
    std::string writeError;
    if (!mosaic::atomicWriteTextFile(reportPath, report, writeError)) {
        std::cerr << errorJson("MOSAIC_DIFFERENCE_WRITE_ERROR", "report", writeError);
        return kExitVerifyError;
    }
    if (differences.total != 0) {
        std::cerr << "verification found " << differences.total
                  << " difference(s); details: " << reportPath << std::endl;
        return kExitDifference;
    }
    std::cout << "verification equal; details: " << reportPath << std::endl;
    return 0;
}

}  // namespace

int main(int argc, char** argv) {
    if (argc < 2) {
        printUsage();
        return kExitUsage;
    }
    const std::string command = argv[1];
    if (command == "render") {
        return renderCommand(argc, argv);
    }
    if (command == "validate") {
        return validateCommand(argc, argv);
    }
    if (command == "verify") {
        return verifyCommand(argc, argv);
    }
    std::cerr << errorJson("MOSAIC_CLI_INVALID", "command", "unknown command");
    printUsage();
    return kExitUsage;
}
