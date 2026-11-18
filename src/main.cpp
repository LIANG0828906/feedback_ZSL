#include "hash.h"
#include "json.h"
#include "output.h"
#include "render.h"
#include "scene.h"
#include "snapshot.h"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cmath>
#include <csignal>
#include <cstdio>
#include <cstdlib>
#include <cstdint>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <limits>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

#ifdef _WIN32
#include <windows.h>
#endif

namespace {

namespace json = cr::json;

volatile std::sig_atomic_t g_interrupt = 0;

void handleSignal(int) {
    g_interrupt = 1;
}

#ifdef _WIN32
BOOL WINAPI consoleHandler(DWORD controlType) {
    if (controlType == CTRL_C_EVENT || controlType == CTRL_BREAK_EVENT) {
        g_interrupt = 1;
        return TRUE;
    }
    return FALSE;
}
#endif

struct Options {
    std::string scenePath;
    std::string outDir = "out";
    std::string baselineDir;
    bool fresh = false;
    bool dumpTextures = false;
    int tileDelayMs = 0;
    int testInterruptAfterBlock = -1;
};

void printUsage() {
    std::cout
        << "coffered_raster --scene <scene.json> [options]\n"
        << "  --out <dir>                    output directory (default: out)\n"
        << "  --fresh                        discard any existing snapshot\n"
        << "  --verify-baseline <dir>        compare final outputs with baseline\n"
        << "  --dump-textures                write deterministic wood sampling tables\n"
        << "  --tile-delay-ms <n>            deterministic pacing for interrupt tests\n"
        << "  --test-interrupt-after-block <n> simulate a processable interrupt\n";
}

bool parseOptions(int argc, char** argv, Options& options) {
    for (int i = 1; i < argc; ++i) {
        const std::string arg = argv[i];
        auto value = [&](const char* name) -> std::string {
            if (i + 1 >= argc) {
                std::cerr << "missing value for " << name << "\n";
                return {};
            }
            return argv[++i];
        };
        if (arg == "--help" || arg == "-h") {
            printUsage();
            std::exit(0);
        } else if (arg == "--scene") {
            options.scenePath = value("--scene");
        } else if (arg == "--out") {
            options.outDir = value("--out");
        } else if (arg == "--verify-baseline") {
            options.baselineDir = value("--verify-baseline");
        } else if (arg == "--fresh") {
            options.fresh = true;
        } else if (arg == "--dump-textures") {
            options.dumpTextures = true;
        } else if (arg == "--tile-delay-ms") {
            try {
                options.tileDelayMs = std::stoi(value("--tile-delay-ms"));
            } catch (...) {
                std::cerr << "invalid --tile-delay-ms\n";
                return false;
            }
        } else if (arg == "--test-interrupt-after-block") {
            try {
                options.testInterruptAfterBlock =
                    std::stoi(value("--test-interrupt-after-block"));
            } catch (...) {
                std::cerr << "invalid --test-interrupt-after-block\n";
                return false;
            }
        } else {
            std::cerr << "unknown option: " << arg << "\n";
            return false;
        }
    }
    if (options.scenePath.empty()) {
        std::cerr << "--scene is required\n";
        return false;
    }
    if (options.tileDelayMs < 0) {
        std::cerr << "--tile-delay-ms must be non-negative\n";
        return false;
    }
    return true;
}

bool writeJson(const std::string& path, const json::Value& value, std::string& error) {
    return cr::writeTextAtomic(path, value.pretty(2), error);
}

json::Value vec3Json(cr::Vec3 value) {
    json::Value result = json::Value::array();
    result.push(json::Value(value.x));
    result.push(json::Value(value.y));
    result.push(json::Value(value.z));
    return result;
}

json::Value ownerInfoJson(const cr::OwnerInfo& info) {
    json::Value result = json::Value::object();
    result.set("code", json::Value(info.code));
    if (info.layer >= 0) result.set("layer", json::Value(info.layer));
    result.set("kind", json::Value(info.type == cr::ComponentType::Board ? "board" :
                                                                          "bracket"));
    result.set("componentId", json::Value(info.componentId));
    result.set("color", vec3Json(info.color));
    return result;
}

json::Value buildReport(const cr::Scene& scene, const cr::RenderContext& context,
                        const std::vector<cr::TileData>& tiles, bool resumed,
                        int resumedBlocks, const std::string& resetReason,
                        const std::string& imageDigest, const std::string& ownerDigest,
                        const std::string& diffStatus) {
    json::Value report = json::Value::object();
    report.set("format", json::Value("coffered-report/v1"));
    report.set("ok", json::Value(true));
    report.set("scene", json::Value(scene.name));
    report.set("sceneDigest", json::Value(scene.digest));
    report.set("textureBankDigest", json::Value(context.textures.bankDigest));
    report.set("projectionDigest", json::Value(context.projectionDigest));
    report.set("imageDigest", json::Value(imageDigest));
    report.set("ownershipDigest", json::Value(ownerDigest));
    report.set("premultipliedAlpha", json::Value(true));
    report.set("resumedFromSnapshot", json::Value(resumed));
    report.set("resumedBlockCount", json::Value(resumedBlocks));
    report.set("snapshotResetReason", json::Value(resetReason));
    report.set("totalBlocks", json::Value(scene.totalBlocks()));
    report.set("completedBlocks", json::Value(static_cast<int>(tiles.size())));
    report.set("tileSize", json::Value(scene.render.tileSize));
    report.set("width", json::Value(scene.render.width));
    report.set("height", json::Value(scene.render.height));

    std::vector<cr::LayerEnergyStats> aggregate(scene.layers.size());
    for (std::size_t i = 0; i < scene.layers.size(); ++i) {
        aggregate[i].layerIndex = scene.layers[i].index;
    }
    for (const cr::TileData& tile : tiles) {
        for (std::size_t i = 0; i < tile.stats.layers.size() &&
                                i < aggregate.size();
             ++i) {
            aggregate[i].reflected += tile.stats.layers[i].reflected;
            aggregate[i].transmitted += tile.stats.layers[i].transmitted;
            aggregate[i].samples += tile.stats.layers[i].samples;
        }
    }
    json::Value layerStats = json::Value::array();
    for (std::size_t i = 0; i < aggregate.size(); ++i) {
        layerStats.push(aggregate[i].toJson(scene.layers[i].index));
    }
    report.set("layerTransmissionReflection", std::move(layerStats));

    json::Value textures = json::Value::array();
    for (const cr::Layer& layer : scene.layers) {
        for (const cr::Board& board : layer.boards) {
            const auto it = context.textures.byOwnerCode.find(board.ownerCode);
            if (it == context.textures.byOwnerCode.end()) continue;
            json::Value item = json::Value::object();
            item.set("layer", json::Value(layer.index));
            item.set("componentId", json::Value(board.id));
            item.set("seed", json::Value(static_cast<double>(board.seed)));
            item.set("resolution", json::Value(it->second.resolution));
            item.set("digest", json::Value(it->second.digest));
            textures.push(std::move(item));
        }
    }
    report.set("woodTextures", std::move(textures));

    json::Value validation = json::Value::object();
    validation.set("ok", json::Value(true));
    validation.set("warningCount", json::Value(0));
    report.set("validation", std::move(validation));

    json::Value ownerIndex = json::Value::array();
    for (const auto& [code, info] : context.ownerTable) {
        json::Value item = ownerInfoJson(info);
        item.set("code", json::Value(code));
        ownerIndex.push(std::move(item));
    }
    report.set("ownershipIndex", std::move(ownerIndex));

    cr::ImageRGBA currentImage;
    currentImage.width = scene.render.width;
    currentImage.height = scene.render.height;
    currentImage.pixels.assign(currentImage.byteSize(), 0u);
    std::vector<int> owners(static_cast<std::size_t>(scene.render.width) *
                                static_cast<std::size_t>(scene.render.height),
                            cr::kBackgroundOwnerCode);
    for (const cr::TileData& tile : tiles) {
        for (int row = 0; row < tile.height; ++row) {
            const int dst = ((tile.y + row) * scene.render.width + tile.x) * 4;
            const int src = row * tile.width * 4;
            std::copy(tile.rgba.begin() + src, tile.rgba.begin() + src + tile.width * 4,
                      currentImage.pixels.begin() + dst);
            const int dstOwner =
                (tile.y + row) * scene.render.width + tile.x;
            std::copy(tile.owners.begin() + row * tile.width,
                      tile.owners.begin() + (row + 1) * tile.width,
                      owners.begin() + dstOwner);
        }
    }
    json::Value queries = json::Value::array();
    const int queryXs[] = {scene.render.width / 2, scene.render.width / 4,
                           (scene.render.width * 3) / 4, scene.render.width / 2,
                           scene.render.width / 3};
    const int queryYs[] = {scene.render.height / 2, scene.render.height / 3,
                           (scene.render.height * 2) / 3, scene.render.height / 5,
                           (scene.render.height * 4) / 5};
    for (int i = 0; i < 5; ++i) {
        const int qx = std::min(scene.render.width - 1, std::max(0, queryXs[i]));
        const int qy = std::min(scene.render.height - 1, std::max(0, queryYs[i]));
        const int code = owners[static_cast<std::size_t>(qy) *
                                    static_cast<std::size_t>(scene.render.width) +
                                static_cast<std::size_t>(qx)];
        json::Value item = json::Value::object();
        item.set("x", json::Value(qx));
        item.set("y", json::Value(qy));
        item.set("ownerCode", json::Value(code));
        const auto it = context.ownerTable.find(code);
        if (it != context.ownerTable.end()) item.set("owner", ownerInfoJson(it->second));
        queries.push(std::move(item));
    }
    report.set("ownershipQueries", std::move(queries));
    report.set("diffStatus", json::Value(diffStatus));
    return report;
}

bool assembleOutputs(const cr::Scene& scene, const std::vector<cr::TileData>& tiles,
                     cr::ImageRGBA& image, std::vector<int>& owners,
                     std::string& error) {
    if (static_cast<int>(tiles.size()) != scene.totalBlocks()) {
        error = "not all blocks are available";
        return false;
    }
    image.width = scene.render.width;
    image.height = scene.render.height;
    image.pixels.assign(image.byteSize(), 0u);
    owners.assign(static_cast<std::size_t>(image.width) *
                      static_cast<std::size_t>(image.height),
                  cr::kBackgroundOwnerCode);
    for (const cr::TileData& tile : tiles) {
        if (tile.width <= 0 || tile.height <= 0) {
            error = "invalid tile dimensions";
            return false;
        }
        for (int row = 0; row < tile.height; ++row) {
            const std::size_t dst =
                (static_cast<std::size_t>(tile.y + row) *
                     static_cast<std::size_t>(image.width) +
                 static_cast<std::size_t>(tile.x)) *
                4u;
            const std::size_t src =
                static_cast<std::size_t>(row) *
                static_cast<std::size_t>(tile.width) * 4u;
            std::copy(tile.rgba.begin() + static_cast<std::ptrdiff_t>(src),
                      tile.rgba.begin() +
                          static_cast<std::ptrdiff_t>(src + tile.width * 4),
                      image.pixels.begin() + static_cast<std::ptrdiff_t>(dst));
            const std::size_t dstOwner =
                static_cast<std::size_t>(tile.y + row) *
                    static_cast<std::size_t>(image.width) +
                static_cast<std::size_t>(tile.x);
            std::copy(tile.owners.begin() +
                          static_cast<std::ptrdiff_t>(row * tile.width),
                      tile.owners.begin() +
                          static_cast<std::ptrdiff_t>((row + 1) * tile.width),
                      owners.begin() + static_cast<std::ptrdiff_t>(dstOwner));
        }
    }
    return true;
}

bool dumpTextures(const cr::Scene& scene, const cr::RenderContext& context,
                  const std::string& outDir, std::string& error) {
    const std::string textureDir = cr::pathJoin(outDir, "textures");
    std::error_code ec;
    std::filesystem::create_directories(textureDir, ec);
    if (ec) {
        error = "cannot create texture directory: " + ec.message();
        return false;
    }
    json::Value manifest = json::Value::object();
    manifest.set("format", json::Value("coffered-textures/v1"));
    manifest.set("bankDigest", json::Value(context.textures.bankDigest));
    json::Value items = json::Value::array();
    for (const cr::Layer& layer : scene.layers) {
        for (const cr::Board& board : layer.boards) {
            const auto it = context.textures.byOwnerCode.find(board.ownerCode);
            if (it == context.textures.byOwnerCode.end()) continue;
            const cr::WoodTexture& texture = it->second;
            const std::string stem = "wood_L" + std::to_string(layer.index) + "_" +
                                     board.id + "_seed" +
                                     std::to_string(board.seed) + "_res" +
                                     std::to_string(texture.resolution);
            cr::ImageRGBA colorImage;
            colorImage.width = texture.resolution;
            colorImage.height = texture.resolution;
            colorImage.pixels.resize(colorImage.byteSize(), 255u);
            for (std::size_t i = 0; i < texture.color.size(); ++i) {
                colorImage.pixels[i * 4] = static_cast<std::uint8_t>(std::lround(
                    std::pow(cr::clamp01(texture.color[i].x), 1.0 / 2.2) * 255.0));
                colorImage.pixels[i * 4 + 1] = static_cast<std::uint8_t>(std::lround(
                    std::pow(cr::clamp01(texture.color[i].y), 1.0 / 2.2) * 255.0));
                colorImage.pixels[i * 4 + 2] = static_cast<std::uint8_t>(std::lround(
                    std::pow(cr::clamp01(texture.color[i].z), 1.0 / 2.2) * 255.0));
                colorImage.pixels[i * 4 + 3] = 255u;
            }
            if (!cr::writePngAtomic(cr::pathJoin(textureDir, stem + ".png"), colorImage,
                                    error)) {
                return false;
            }
            if (!cr::writeFileAtomic(cr::pathJoin(textureDir, stem + ".bin"),
                                     cr::serializeTextureTable(texture), error)) {
                return false;
            }
            json::Value item = json::Value::object();
            item.set("layer", json::Value(layer.index));
            item.set("componentId", json::Value(board.id));
            item.set("seed", json::Value(static_cast<double>(board.seed)));
            item.set("resolution", json::Value(texture.resolution));
            item.set("digest", json::Value(texture.digest));
            item.set("colorPng", json::Value(stem + ".png"));
            item.set("tableBin", json::Value(stem + ".bin"));
            items.push(std::move(item));
        }
    }
    manifest.set("textures", std::move(items));
    return writeJson(cr::pathJoin(textureDir, "textures.json"), manifest, error);
}

bool parseOwnerBinary(const std::vector<std::uint8_t>& bytes, int& width, int& height,
                      std::size_t& dataOffset) {
    const std::string header(bytes.begin(), bytes.begin() + std::min<std::size_t>(bytes.size(), 64));
    const std::size_t first = header.find('\n');
    if (first == std::string::npos) return false;
    const std::size_t second = header.find('\n', first + 1);
    if (second == std::string::npos) return false;
    if (header.substr(0, first) != "COFFOWN1") return false;
    if (std::sscanf(header.c_str() + first + 1, "%d %d", &width, &height) != 2) {
        return false;
    }
    dataOffset = second + 1;
    return width >= 0 && height >= 0;
}

bool verifyBaseline(const cr::Scene& scene, const std::string& baselineDir,
                    const cr::ImageRGBA& currentImage,
                    const std::vector<int>& currentOwners,
                    const std::string& currentProjectionPath,
                    const std::string& diffPath, bool& matched,
                    std::string& error) {
    json::Value diff = json::Value::object();
    diff.set("format", json::Value("coffered-diff/v1"));
    int totalDifferences = 0;
    json::Value differences = json::Value::array();
    auto addDifference = [&](int block, int x, int y, const std::string& channel,
                             double expected, double actual) {
        ++totalDifferences;
        if (differences.asArray().size() >= 128) return;
        json::Value item = json::Value::object();
        item.set("block", json::Value(block));
        item.set("x", json::Value(x));
        item.set("y", json::Value(y));
        item.set("channel", json::Value(channel));
        item.set("expected", json::Value(expected));
        item.set("actual", json::Value(actual));
        differences.push(std::move(item));
    };

    cr::ImageRGBA baselineImage;
    if (!cr::readPng(cr::pathJoin(baselineDir, "rgba.png"), baselineImage, error)) {
        return false;
    }
    const int blocksX = scene.blocksX();
    if (baselineImage.width != currentImage.width ||
        baselineImage.height != currentImage.height) {
        addDifference(-1, -1, -1, "image-dimensions", baselineImage.width,
                      currentImage.width);
    } else {
        static const char* kChannels[] = {"R", "G", "B", "A"};
        for (int y = 0; y < currentImage.height; ++y) {
            for (int x = 0; x < currentImage.width; ++x) {
                const std::size_t i = (static_cast<std::size_t>(y) *
                                           static_cast<std::size_t>(currentImage.width) +
                                       static_cast<std::size_t>(x)) *
                                      4u;
                for (int channel = 0; channel < 4; ++channel) {
                    if (baselineImage.pixels[i + static_cast<std::size_t>(channel)] !=
                        currentImage.pixels[i + static_cast<std::size_t>(channel)]) {
                        const int block =
                            (y / scene.render.tileSize) * blocksX +
                            (x / scene.render.tileSize);
                        addDifference(
                            block, x, y, kChannels[channel],
                            baselineImage.pixels[i + static_cast<std::size_t>(channel)],
                            currentImage.pixels[i + static_cast<std::size_t>(channel)]);
                    }
                }
            }
        }
    }

    std::vector<std::uint8_t> baselineOwnerBytes;
    if (!cr::readFile(cr::pathJoin(baselineDir, "owner.bin"), baselineOwnerBytes, error)) {
        return false;
    }
    int baseWidth = 0;
    int baseHeight = 0;
    std::size_t offset = 0;
    if (!parseOwnerBinary(baselineOwnerBytes, baseWidth, baseHeight, offset)) {
        error = "baseline owner.bin header is invalid";
        return false;
    }
    if (baseWidth != currentImage.width || baseHeight != currentImage.height) {
        addDifference(-1, -1, -1, "owner-dimensions", baseWidth, currentImage.width);
    } else if (baselineOwnerBytes.size() <
               offset + static_cast<std::size_t>(baseWidth) *
                            static_cast<std::size_t>(baseHeight) * 4u) {
        error = "baseline owner.bin is truncated";
        return false;
    } else {
        for (int y = 0; y < baseHeight; ++y) {
            for (int x = 0; x < baseWidth; ++x) {
                const std::size_t i = static_cast<std::size_t>(y) *
                                          static_cast<std::size_t>(baseWidth) +
                                      static_cast<std::size_t>(x);
                const std::uint8_t* p = baselineOwnerBytes.data() + offset + i * 4;
                const std::int32_t expected =
                    static_cast<std::int32_t>(
                        static_cast<std::uint32_t>(p[0]) |
                        (static_cast<std::uint32_t>(p[1]) << 8u) |
                        (static_cast<std::uint32_t>(p[2]) << 16u) |
                        (static_cast<std::uint32_t>(p[3]) << 24u));
                if (expected != currentOwners[i]) {
                    const int block =
                        (y / scene.render.tileSize) * blocksX +
                        (x / scene.render.tileSize);
                    addDifference(block, x, y, "ownerCode", expected,
                                  currentOwners[i]);
                }
            }
        }
    }

    std::vector<std::uint8_t> currentProjection;
    std::vector<std::uint8_t> baselineProjection;
    if (!cr::readFile(currentProjectionPath, currentProjection, error)) return false;
    if (!cr::readFile(cr::pathJoin(baselineDir, "projection.json"),
                      baselineProjection, error)) {
        return false;
    }
    if (currentProjection != baselineProjection) {
        addDifference(-1, -1, -1, "projectionBytes", 0.0, 1.0);
    }
    matched = totalDifferences == 0;
    diff.set("ok", json::Value(matched));
    diff.set("totalDifferences", json::Value(totalDifferences));
    diff.set("differences", std::move(differences));
    if (!writeJson(diffPath, diff, error)) return false;
    return true;
}

}  // namespace

int main(int argc, char** argv) {
    Options options;
    if (!parseOptions(argc, argv, options)) {
        printUsage();
        return 1;
    }

    std::signal(SIGINT, handleSignal);
#ifdef _WIN32
    SetConsoleCtrlHandler(consoleHandler, TRUE);
#else
    std::signal(SIGTERM, handleSignal);
#endif

    std::error_code ec;
    std::filesystem::create_directories(options.outDir, ec);
    if (ec) {
        std::cerr << "cannot create output directory: " << ec.message() << "\n";
        return 1;
    }

    const cr::LoadResult loaded = cr::loadScene(options.scenePath);
    if (!loaded.parsed) {
        std::cerr << "{\"code\":\"scene_parse_error\",\"message\":"
                  << json::Value(loaded.fatalError).canonical() << "}\n";
        json::Value failure = json::Value::object();
        failure.set("format", json::Value("coffered-validation/v1"));
        failure.set("ok", json::Value(false));
        failure.set("fatalError", json::Value(loaded.fatalError));
        std::string error;
        writeJson(cr::pathJoin(options.outDir, "validation.json"), failure, error);
        return 1;
    }
    const cr::Scene& scene = loaded.scene;
    if (!scene.valid()) {
        const json::Value validation = cr::warningsToJson(scene.warnings);
        std::string error;
        writeJson(cr::pathJoin(options.outDir, "validation.json"), validation, error);
        for (const cr::Warning& warning : scene.warnings) {
            std::cerr << warning.toJson().canonical() << "\n";
        }
        return 2;
    }

    const std::string snapshotPath =
        cr::pathJoin(options.outDir, "progress.snapshot.json");
    if (options.fresh) cr::discardSnapshot(snapshotPath);

    cr::RenderContext context(scene);
    cr::SnapshotLoadResult snapshot = cr::loadSnapshot(snapshotPath, scene);
    std::vector<cr::TileData> tiles;
    bool resumed = false;
    int resumedBlocks = 0;
    std::string resetReason;
    if (snapshot.valid) {
        tiles = std::move(snapshot.tiles);
        resumed = !tiles.empty();
        resumedBlocks = static_cast<int>(tiles.size());
    } else if (snapshot.discarded) {
        resetReason = snapshot.reason;
    }
    const bool snapshotComplete =
        snapshot.valid && static_cast<int>(tiles.size()) == scene.totalBlocks();
    if (!snapshotComplete) {
        for (const char* name : {"rgba.png", "ownership.png", "owner.bin",
                                 "projection.json", "diff_report.json"}) {
            cr::removeFileIfExists(cr::pathJoin(options.outDir, name));
        }
    }

    if (resumed) {
        std::cerr << "{\"code\":\"snapshot_resume\",\"completedBlocks\":"
                  << resumedBlocks << ",\"totalBlocks\":" << scene.totalBlocks()
                  << "}\n";
    } else if (snapshot.discarded) {
        std::cerr << "{\"code\":\"snapshot_discarded\",\"reason\":"
                  << json::Value(resetReason).canonical() << "}\n";
    }

    const bool wasInterruptedAtStart =
        options.testInterruptAfterBlock < 0 && g_interrupt != 0;
    (void)wasInterruptedAtStart;

    int completed = static_cast<int>(tiles.size());
    while (completed < scene.totalBlocks() && g_interrupt == 0) {
        const int x = (completed % scene.blocksX()) * scene.render.tileSize;
        const int y = (completed / scene.blocksX()) * scene.render.tileSize;
        const int width = std::min(scene.render.tileSize, scene.render.width - x);
        const int height = std::min(scene.render.tileSize, scene.render.height - y);
        cr::TileData tile = context.renderTile(completed, x, y, width, height);
        tiles.push_back(std::move(tile));
        ++completed;
        std::string error;
        if (!cr::saveSnapshot(snapshotPath, scene, tiles, error)) {
            std::cerr << "snapshot write failed: " << error << "\n";
            return 1;
        }
        if (options.tileDelayMs > 0) {
            std::this_thread::sleep_for(
                std::chrono::milliseconds(options.tileDelayMs));
        }
        if (options.testInterruptAfterBlock >= 0 &&
            completed > options.testInterruptAfterBlock) {
            g_interrupt = 1;
        }
    }

    if (g_interrupt != 0 && completed < scene.totalBlocks()) {
        json::Value recovery = json::Value::object();
        recovery.set("format", json::Value("coffered-recovery/v1"));
        recovery.set("interrupted", json::Value(true));
        recovery.set("exitCode", json::Value(0));
        recovery.set("completedBlocks", json::Value(completed));
        recovery.set("totalBlocks", json::Value(scene.totalBlocks()));
        recovery.set("sceneDigest", json::Value(scene.digest));
        recovery.set("snapshot", json::Value("progress.snapshot.json"));
        recovery.set("message",
                     json::Value("current block committed; rerun the same command to resume"));
        std::string error;
        if (!writeJson(cr::pathJoin(options.outDir, "recovery.json"), recovery, error)) {
            std::cerr << "recovery report write failed: " << error << "\n";
            return 1;
        }
        std::cerr << recovery.canonical() << "\n";
        return 0;
    }

    if (completed != scene.totalBlocks()) {
        std::cerr << "internal error: incomplete render\n";
        return 1;
    }

    cr::ImageRGBA image;
    std::vector<int> owners;
    std::string error;
    if (!assembleOutputs(scene, tiles, image, owners, error)) {
        std::cerr << "output assembly failed: " << error << "\n";
        return 1;
    }
    const std::string rgbaPath = cr::pathJoin(options.outDir, "rgba.png");
    if (!cr::writePngAtomic(rgbaPath, image, error)) {
        std::cerr << "RGBA write failed: " << error << "\n";
        return 1;
    }
    const cr::ImageRGBA ownershipImage =
        cr::ownerImage(owners, image.width, image.height, context.ownerTable);
    if (!cr::writePngAtomic(cr::pathJoin(options.outDir, "ownership.png"),
                            ownershipImage, error)) {
        std::cerr << "ownership image write failed: " << error << "\n";
        return 1;
    }
    if (!cr::writeFileAtomic(cr::pathJoin(options.outDir, "owner.bin"),
                             cr::ownerBinary(owners, image.width, image.height),
                             error)) {
        std::cerr << "owner.bin write failed: " << error << "\n";
        return 1;
    }
    const std::string projectionPath = cr::pathJoin(options.outDir, "projection.json");
    if (!writeJson(projectionPath, context.projectionReport(), error)) {
        std::cerr << "projection report write failed: " << error << "\n";
        return 1;
    }
    if (options.dumpTextures &&
        !dumpTextures(scene, context, options.outDir, error)) {
        std::cerr << "texture dump failed: " << error << "\n";
        return 1;
    }

    const std::string imageDigest =
        cr::sha256Hex(image.pixels.data(), image.pixels.size());
    const std::vector<std::uint8_t> ownerBytes =
        cr::ownerBinary(owners, image.width, image.height);
    const std::string ownerDigest =
        cr::sha256Hex(ownerBytes.data(), ownerBytes.size());
    const std::string diffPath = cr::pathJoin(options.outDir, "diff_report.json");
    std::string diffStatus = "not_requested";
    bool diffMatched = true;
    if (!options.baselineDir.empty()) {
        if (!verifyBaseline(scene, options.baselineDir, image, owners, projectionPath,
                            diffPath, diffMatched, error)) {
            std::cerr << "baseline verification failed: " << error << "\n";
            return 1;
        }
        diffStatus = diffMatched ? "match" : "mismatch";
    }
    const json::Value report =
        buildReport(scene, context, tiles, resumed, resumedBlocks, resetReason,
                    imageDigest, ownerDigest, diffStatus);
    if (!writeJson(cr::pathJoin(options.outDir, "report.json"), report, error)) {
        std::cerr << "report write failed: " << error << "\n";
        return 1;
    }
    if (!options.baselineDir.empty() && !diffMatched) {
        std::cerr << "{\"code\":\"resume_diff\",\"diffReport\":\"diff_report.json\","
                     "\"exitCode\":3}\n";
        return 3;
    }
    if (g_interrupt != 0) {
        json::Value recovery = json::Value::object();
        recovery.set("format", json::Value("coffered-recovery/v1"));
        recovery.set("interrupted", json::Value(true));
        recovery.set("exitCode", json::Value(0));
        recovery.set("completedBlocks", json::Value(completed));
        recovery.set("totalBlocks", json::Value(scene.totalBlocks()));
        recovery.set("sceneDigest", json::Value(scene.digest));
        recovery.set("snapshot", json::Value("progress.snapshot.json"));
        recovery.set("message",
                     json::Value("interrupt observed after final block; all outputs written"));
        writeJson(cr::pathJoin(options.outDir, "recovery.json"), recovery, error);
    }
    std::cout << "{\"ok\":true,\"image\":\"rgba.png\",\"ownership\":\"owner.bin\","
                 "\"projection\":\"projection.json\",\"report\":\"report.json\","
                 "\"completedBlocks\":"
              << completed << ",\"diff\":\"" << diffStatus << "\"}\n";
    return 0;
}
