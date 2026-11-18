#include "json.hpp"
#include "png.hpp"
#include "render.hpp"
#include "scene.hpp"
#include "snapshot.hpp"

#include <algorithm>
#include <chrono>
#include <csignal>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <functional>
#include <iostream>
#include <map>
#include <set>
#include <sstream>
#include <stdexcept>
#include <string>
#include <thread>
#include <vector>

namespace {

volatile std::sig_atomic_t g_interrupted = 0;

void signalHandler(int) {
    g_interrupted = 1;
}

struct Cli {
    std::string command;
    std::map<std::string, std::string> values;
    std::set<std::string> switches;

    bool has(const std::string& name) const {
        return values.find(name) != values.end() || switches.find(name) != switches.end();
    }
    std::string get(const std::string& name, const std::string& fallback = "") const {
        auto it = values.find(name);
        return it == values.end() ? fallback : it->second;
    }
};

Cli parseCli(int argc, char** argv) {
    Cli cli;
    if (argc >= 2) cli.command = argv[1];
    for (int i = 2; i < argc; ++i) {
        std::string arg = argv[i];
        if (arg.find("--") != 0) {
            throw std::runtime_error("unexpected argument: " + arg);
        }
        std::string key = arg.substr(2);
        const std::size_t equals = key.find('=');
        if (equals != std::string::npos) {
            cli.values[key.substr(0, equals)] = key.substr(equals + 1);
            continue;
        }
        if (i + 1 < argc && std::string(argv[i + 1]).find("--") != 0) {
            cli.values[key] = argv[++i];
        } else {
            cli.switches.insert(key);
        }
    }
    return cli;
}

int parseIntOption(const Cli& cli, const std::string& name, int fallback = 0) {
    if (!cli.has(name)) return fallback;
    const std::string value = cli.get(name);
    std::size_t consumed = 0;
    int result = 0;
    try {
        result = std::stoi(value, &consumed);
    } catch (...) {
        throw std::runtime_error("--" + name + " requires an integer");
    }
    if (consumed != value.size()) {
        throw std::runtime_error("--" + name + " requires an integer");
    }
    return result;
}

std::string required(const Cli& cli, const std::string& name) {
    if (!cli.has(name) || cli.get(name).empty()) {
        throw std::runtime_error("missing required option --" + name);
    }
    return cli.get(name);
}

std::string optionalOutput(const Cli& cli, const std::string& fallback = "") {
    if (cli.has("out")) return cli.get("out");
    if (cli.has("output")) return cli.get("output");
    return fallback;
}

std::string readText(const std::string& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) throw std::runtime_error("cannot read " + path);
    std::ostringstream buffer;
    buffer << input.rdbuf();
    return buffer.str();
}

std::vector<std::uint8_t> readBinary(const std::string& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) throw std::runtime_error("cannot read " + path);
    input.seekg(0, std::ios::end);
    const std::streamoff size = input.tellg();
    input.seekg(0, std::ios::beg);
    std::vector<std::uint8_t> bytes(static_cast<std::size_t>(size));
    input.read(reinterpret_cast<char*>(bytes.data()), size);
    if (!input && size != 0) throw std::runtime_error("failed reading " + path);
    return bytes;
}

void writeBytesAtomic(const std::string& path, const std::vector<std::uint8_t>& bytes) {
    std::string contents;
    if (!bytes.empty()) {
        contents.assign(reinterpret_cast<const char*>(bytes.data()), bytes.size());
    }
    sl::writeFileAtomic(path, contents);
}

void removeIfExists(const std::filesystem::path& path) {
    std::error_code error;
    std::filesystem::remove(path, error);
}

std::string outputPath(const std::string& dir, const std::string& name) {
    return (std::filesystem::path(dir) / name).string();
}

void printUsage() {
    std::cout
        << "silklight - deterministic offline silkscreen lightbox validator\n\n"
        << "Commands:\n"
        << "  sample   --out scene.json [--seed N]\n"
        << "  validate --scene scene.json\n"
        << "  halftone --scene scene.json --out-dir DIR\n"
        << "  render   --scene scene.json --out-dir DIR [--fresh]\n"
        << "           [--stop-after-blocks N] [--crash-after-blocks N]\n"
        << "           [--tile-delay-ms N]\n"
        << "  compare  --expected DIR --actual DIR [--out diff.json]\n"
        << "  self-test [--work-dir DIR]\n";
}

sj::Value okJson() {
    sj::Value value = sj::Value::object();
    value["ok"] = sj::Value(true);
    return value;
}

struct RenderOptions {
    bool fresh = false;
    bool quiet = false;
    int stopAfterBlocks = 0;
    int crashAfterBlocks = 0;
    int tileDelayMs = 0;
};

enum class RenderStatus { Completed, InterruptedResumable, InvalidScene, Failed };

struct RenderOutcome {
    RenderStatus status = RenderStatus::Failed;
    int exitCode = 1;
    sj::Value summary;
};

sj::Value recoveryJson(const sl::Renderer& renderer,
                       const std::string& snapshotPath,
                       const std::string& snapshotDigest,
                       const std::map<int, sl::SnapshotBlockRecord>& completed,
                       int lastBlock) {
    sj::Value recovery = sj::Value::object();
    recovery["ok"] = sj::Value(true);
    recovery["code"] = sj::Value("E_INTERRUPTED_RESUMABLE");
    recovery["graceful"] = sj::Value(true);
    recovery["exitCode"] = sj::Value(0);
    recovery["message"] =
        sj::Value("current tile completed and committed; rerun render without --fresh to resume");
    recovery["sceneDigest"] = sj::Value(renderer.scene().sceneDigest);
    recovery["snapshot"] = sj::Value::object();
    recovery["snapshot"]["path"] = sj::Value(snapshotPath);
    recovery["snapshot"]["documentDigest"] = sj::Value(snapshotDigest);
    recovery["snapshot"]["completedBlockCount"] = sj::Value(static_cast<double>(completed.size()));
    recovery["lastCompletedBlock"] = sj::Value(lastBlock);
    recovery["completedBlocks"] = sj::Value::array();
    for (const auto& item : completed) {
        recovery["completedBlocks"].push(sj::Value(item.first));
    }
    return recovery;
}

RenderOutcome runRender(const std::string& scenePath,
                        const std::string& outDir,
                        const RenderOptions& options) {
    RenderOutcome outcome;
    sl::Scene scene;
    std::vector<sl::Diagnostic> diagnostics;
    if (!sl::loadScene(scenePath, scene, diagnostics)) {
        outcome.status = RenderStatus::InvalidScene;
        outcome.exitCode = 2;
        outcome.summary = sl::diagnosticsJson(diagnostics);
        return outcome;
    }

    std::filesystem::create_directories(outDir);
    const std::string snapshotPath = outputPath(outDir, "snapshot.json");
    const std::string rawPath = outputPath(outDir, "render_rgba.raw");
    const std::string pngPath = outputPath(outDir, "render_rgba.png");
    const std::string ownerRawPath = outputPath(outDir, "owner_map.raw");
    const std::string ownerPngPath = outputPath(outDir, "owner_map.png");
    const std::string reportPath = outputPath(outDir, "report.json");
    const std::string recoveryPath = outputPath(outDir, "recovery.json");
    const std::string resumePath = outputPath(outDir, "resume_report.json");

    for (const std::string& path :
         {rawPath, pngPath, ownerRawPath, ownerPngPath, reportPath, resumePath}) {
        removeIfExists(path);
    }
    if (options.fresh) {
        removeIfExists(recoveryPath);
    }

    sl::Renderer renderer(scene);
    sl::SnapshotStore snapshot(snapshotPath);
    std::map<int, sl::SnapshotBlockRecord> completed;
    std::vector<int> resumedBlocks;
    std::vector<int> renderedBlocks;
    std::string snapshotResetReason;
    std::string snapshotDocumentDigest;
    bool resumedMode = false;

    if (options.fresh) {
        snapshot.discard();
        snapshotResetReason = "fresh-requested";
    } else {
        sl::SnapshotLoadResult loaded = snapshot.load(renderer);
        if (loaded.valid) {
            completed = std::move(loaded.data.completed);
            snapshotDocumentDigest = loaded.data.documentDigest;
            for (const auto& item : completed) resumedBlocks.push_back(item.first);
            if (!completed.empty()) {
                resumedMode = true;
                snapshotResetReason = "ok";
            }
        } else if (loaded.exists) {
            snapshotResetReason = loaded.reason;
            snapshot.discard();
            completed.clear();
        } else {
            snapshotResetReason = "no-snapshot";
        }
    }

    int lastCompleted = -1;
    for (int blockId = 0; blockId < renderer.blockCount(); ++blockId) {
        auto found = completed.find(blockId);
        if (found != completed.end()) {
            lastCompleted = blockId;
            continue;
        }
        if (options.tileDelayMs > 0) {
            std::this_thread::sleep_for(std::chrono::milliseconds(options.tileDelayMs));
        }
        sl::BlockResult block = renderer.renderBlock(blockId);
        sl::SnapshotBlockRecord record;
        record.id = blockId;
        record.width = block.rect.width;
        record.height = block.rect.height;
        record.rgba = block.rgba;
        record.owner = block.owner;
        record.metrics = block.metrics;
        record.rgbaDigest = block.rgbaDigest;
        record.ownerDigest = block.ownerDigest;
        completed[blockId] = std::move(record);
        snapshotDocumentDigest = snapshot.commit(renderer, completed);
        renderedBlocks.push_back(blockId);
        lastCompleted = blockId;
        if (!options.quiet) {
            std::cerr << "committed block " << blockId + 1 << "/" << renderer.blockCount()
                      << "\n";
        }
        if (options.crashAfterBlocks > 0 &&
            static_cast<int>(renderedBlocks.size()) >= options.crashAfterBlocks) {
#if defined(_WIN32)
            std::_Exit(137);
#else
            std::_Exit(137);
#endif
        }
        if (g_interrupted != 0 ||
            (options.stopAfterBlocks > 0 &&
             static_cast<int>(renderedBlocks.size()) >= options.stopAfterBlocks)) {
            sj::Value recovery =
                recoveryJson(renderer, snapshotPath, snapshotDocumentDigest, completed,
                             lastCompleted);
            sl::writeFileAtomic(recoveryPath, recovery.dump(2) + "\n");
            outcome.status = RenderStatus::InterruptedResumable;
            outcome.exitCode = 0;
            outcome.summary = recovery;
            return outcome;
        }
    }

    if (static_cast<int>(completed.size()) != renderer.blockCount()) {
        outcome.status = RenderStatus::Failed;
        outcome.exitCode = 3;
        outcome.summary = sj::Value::object();
        outcome.summary["ok"] = sj::Value(false);
        outcome.summary["code"] = sj::Value("E_RENDER_INCOMPLETE");
        outcome.summary["message"] = sj::Value("snapshot has missing block records");
        return outcome;
    }

    std::vector<std::uint8_t> rgba(
        static_cast<std::size_t>(scene.image.width * scene.image.height * 4), 0);
    std::vector<std::uint8_t> owner(
        static_cast<std::size_t>(scene.image.width * scene.image.height), 0);
    std::vector<sl::LayerMetric> metrics;
    metrics.resize(scene.layers.size());
    for (std::size_t i = 0; i < scene.layers.size(); ++i) {
        metrics[i].layerIndex = scene.layers[i].layerIndex;
        metrics[i].separationId = scene.layers[i].separationId;
    }

    for (int blockId = 0; blockId < renderer.blockCount(); ++blockId) {
        const sl::SnapshotBlockRecord& block = completed.at(blockId);
        const sl::BlockRect rect = renderer.blockRect(blockId);
        if (block.width != rect.width || block.height != rect.height ||
            block.rgba.size() !=
                static_cast<std::size_t>(rect.width * rect.height * 4) ||
            block.owner.size() != static_cast<std::size_t>(rect.width * rect.height)) {
            outcome.status = RenderStatus::Failed;
            outcome.exitCode = 3;
            outcome.summary = sj::Value::object();
            outcome.summary["ok"] = sj::Value(false);
            outcome.summary["code"] = sj::Value("E_SNAPSHOT_BLOCK_INVALID");
            outcome.summary["block"] = sj::Value(blockId);
            return outcome;
        }
        for (int row = 0; row < rect.height; ++row) {
            const std::size_t source =
                static_cast<std::size_t>(row * rect.width * 4);
            const std::size_t target =
                (static_cast<std::size_t>(rect.y + row) * scene.image.width + rect.x) * 4;
            std::copy(block.rgba.begin() + static_cast<std::ptrdiff_t>(source),
                      block.rgba.begin() +
                          static_cast<std::ptrdiff_t>(source + rect.width * 4),
                      rgba.begin() + static_cast<std::ptrdiff_t>(target));
            const std::size_t ownerSource = static_cast<std::size_t>(row * rect.width);
            const std::size_t ownerTarget =
                static_cast<std::size_t>(rect.y + row) * scene.image.width + rect.x;
            std::copy(block.owner.begin() + static_cast<std::ptrdiff_t>(ownerSource),
                      block.owner.begin() +
                          static_cast<std::ptrdiff_t>(ownerSource + rect.width),
                      owner.begin() + static_cast<std::ptrdiff_t>(ownerTarget));
        }
        metrics = sl::Renderer::combineMetrics(metrics, block.metrics);
    }

    writeBytesAtomic(rawPath, rgba);
    writeBytesAtomic(ownerRawPath, owner);

    std::vector<std::uint8_t> ownerPng(
        static_cast<std::size_t>(scene.image.width * scene.image.height * 4), 0);
    const std::uint8_t palette[5][4] = {
        {10, 12, 18, 255},
        {28, 196, 220, 255},
        {52, 122, 244, 255},
        {232, 52, 168, 255},
        {238, 214, 62, 255},
    };
    for (std::size_t i = 0; i < owner.size(); ++i) {
        const std::uint8_t id = owner[i];
        const std::size_t slot = id <= 4 ? id : 0;
        ownerPng[i * 4 + 0] = palette[slot][0];
        ownerPng[i * 4 + 1] = palette[slot][1];
        ownerPng[i * 4 + 2] = palette[slot][2];
        ownerPng[i * 4 + 3] = palette[slot][3];
    }
    sl::writePngRGBA(pngPath, scene.image.width, scene.image.height, rgba);
    sl::writePngRGBA(ownerPngPath, scene.image.width, scene.image.height, ownerPng);

    sj::Value report = renderer.baseReport();
    report["code"] = sj::Value("E_RENDER_OK");
    report["sceneDigest"] = sj::Value(scene.sceneDigest);
    report["layerContributions"] = renderer.metricsJson(metrics);
    report["projection"]["pointCount"] = sj::Value(
        static_cast<double>(renderer.projectionPoints().size()));
    report["projection"]["recompute"] =
        sj::Value("ray(light, holeWorld) -> board plane; boardUV uses backboard u/v axes");
    report["snapshot"] = sj::Value::object();
    report["snapshot"]["path"] = sj::Value(snapshotPath);
    report["snapshot"]["documentDigest"] = sj::Value(snapshotDocumentDigest);
    report["snapshot"]["completedBlockCount"] = sj::Value(static_cast<double>(completed.size()));
    report["snapshot"]["committedEveryBlock"] = sj::Value(true);
    report["snapshot"]["wholeReplacement"] = sj::Value(true);
    report["resume"] = sj::Value::object();
    report["resume"]["resumed"] = sj::Value(resumedMode);
    report["resume"]["snapshotResetReason"] = sj::Value(snapshotResetReason);
    report["resume"]["reusedBlockCount"] = sj::Value(static_cast<double>(resumedBlocks.size()));
    report["resume"]["renderedBlockCount"] = sj::Value(static_cast<double>(renderedBlocks.size()));
    report["resume"]["reusedBlocks"] = sj::Value::array();
    for (int id : resumedBlocks) {
        report["resume"]["reusedBlocks"].push(sj::Value(id));
    }
    report["outputs"] = sj::Value::object();
    report["outputs"]["rgbaRaw"] = sj::Value("render_rgba.raw");
    report["outputs"]["rgbaRawDigest"] = sj::Value(sl::Renderer::rgbaDigest(rgba));
    report["outputs"]["rgbaPng"] = sj::Value("render_rgba.png");
    report["outputs"]["ownerRaw"] = sj::Value("owner_map.raw");
    report["outputs"]["ownerPng"] = sj::Value("owner_map.png");
    report["outputs"]["rgbaBytes"] = sj::Value(static_cast<double>(rgba.size()));
    report["outputs"]["ownerBytes"] = sj::Value(static_cast<double>(owner.size()));
    report["ownerMap"] = sj::Value::object();
    report["ownerMap"]["encoding"] = sj::Value("uint8 separation index; 0=background");
    report["ownerMap"]["compositionOrder"] = sj::Value("layerIndex ascending");
    report["ownerMap"]["policy"] = sj::Value("last contributing layer owns pixel");
    report["ownerMap"]["indices"] = sj::Value::array();
    for (const sl::ScreenLayer& layer : scene.layers) {
        sj::Value item = sj::Value::object();
        item["index"] = sj::Value(layer.layerIndex);
        item["layer"] = sj::Value(layer.layerIndex);
        item["separationId"] = sj::Value(layer.separationId);
        report["ownerMap"]["indices"].push(std::move(item));
    }
    report["determinism"] =
        sj::Value("double-precision CPU path; no unseeded RNG; stable layer/triangle order");
    sl::writeFileAtomic(reportPath, report.dump(2) + "\n");

    if (!resumedMode) {
        removeIfExists(recoveryPath);
    }
    if (resumedMode || snapshotResetReason != "no-snapshot") {
        sj::Value resume = sj::Value::object();
        resume["ok"] = sj::Value(true);
        resume["sceneDigest"] = sj::Value(scene.sceneDigest);
        resume["mode"] = sj::Value(resumedMode ? "resumed" : "from-zero");
        resume["snapshotResetReason"] = sj::Value(snapshotResetReason);
        resume["reusedBlocks"] = sj::Value::array();
        for (int id : resumedBlocks) resume["reusedBlocks"].push(sj::Value(id));
        resume["renderedBlocks"] = sj::Value::array();
        for (int id : renderedBlocks) resume["renderedBlocks"].push(sj::Value(id));
        resume["rgbaDigest"] = sj::Value(sl::Renderer::rgbaDigest(rgba));
        resume["ownerDigest"] = sj::Value(sl::hex64(sl::hashString(std::string(
            reinterpret_cast<const char*>(owner.data()), owner.size()))));
        resume["projectionDigest"] =
            sj::Value(sl::hex64(sl::hashString(renderer.projectionPointsJson().dump())));
        sl::writeFileAtomic(resumePath, resume.dump(2) + "\n");
    }

    outcome.status = RenderStatus::Completed;
    outcome.exitCode = 0;
    outcome.summary = okJson();
    outcome.summary["code"] = sj::Value("E_RENDER_OK");
    outcome.summary["sceneDigest"] = sj::Value(scene.sceneDigest);
    outcome.summary["imageWidth"] = sj::Value(scene.image.width);
    outcome.summary["imageHeight"] = sj::Value(scene.image.height);
    outcome.summary["blockCount"] = sj::Value(renderer.blockCount());
    outcome.summary["resumed"] = sj::Value(resumedMode);
    outcome.summary["outputDir"] = sj::Value(outDir);
    outcome.summary["rgbaDigest"] = sj::Value(sl::Renderer::rgbaDigest(rgba));
    outcome.summary["report"] = sj::Value(reportPath);
    return outcome;
}

struct DifferenceCollector {
    sj::Value differences = sj::Value::array();
    std::size_t total = 0;
    std::size_t limit = 64;

    void add(sj::Value item) {
        ++total;
        if (differences.asArray().size() < limit) {
            differences.push(std::move(item));
        }
    }
};

sj::Value compareDirectories(const std::string& expectedDir, const std::string& actualDir) {
    sj::Value result = sj::Value::object();
    DifferenceCollector collector;
    sj::Value metadata = sj::Value::object();
    metadata["expectedDir"] = sj::Value(expectedDir);
    metadata["actualDir"] = sj::Value(actualDir);
    result["metadata"] = metadata;

    const std::string expectedReportPath = outputPath(expectedDir, "report.json");
    const std::string actualReportPath = outputPath(actualDir, "report.json");
    std::string parseError;
    sj::Value expectedReport =
        sj::Value::parse(readText(expectedReportPath), parseError);
    if (!parseError.empty()) throw std::runtime_error("invalid expected report.json");
    sj::Value actualReport =
        sj::Value::parse(readText(actualReportPath), parseError);
    if (!parseError.empty()) throw std::runtime_error("invalid actual report.json");

    const sj::Value* eImage = expectedReport.find("image");
    const sj::Value* aImage = actualReport.find("image");
    if (eImage == nullptr || aImage == nullptr) {
        throw std::runtime_error("report image metadata missing");
    }
    const int width = static_cast<int>((*eImage)["width"].asNumber());
    const int height = static_cast<int>((*eImage)["height"].asNumber());
    const int blockWidth = static_cast<int>((*eImage)["blockWidth"].asNumber());
    const int blockHeight = static_cast<int>((*eImage)["blockHeight"].asNumber());
    if (width <= 0 || height <= 0 || blockWidth <= 0 || blockHeight <= 0) {
        throw std::runtime_error("invalid expected image metadata");
    }
    const int blocksX = (width + blockWidth - 1) / blockWidth;
    const int actualWidth = static_cast<int>((*aImage)["width"].asNumber());
    const int actualHeight = static_cast<int>((*aImage)["height"].asNumber());
    const int actualBlockWidth = static_cast<int>((*aImage)["blockWidth"].asNumber());
    const int actualBlockHeight = static_cast<int>((*aImage)["blockHeight"].asNumber());
    if (actualWidth != width || actualHeight != height ||
        actualBlockWidth != blockWidth || actualBlockHeight != blockHeight) {
        sj::Value item = sj::Value::object();
        item["kind"] = sj::Value("image-config");
        item["tile"] = sj::Value(-1);
        item["pixel"] = sj::Value::array();
        item["pixel"].push(sj::Value(-1));
        item["pixel"].push(sj::Value(-1));
        item["channel"] = sj::Value("metadata");
        item["expected"] = sj::Value::array();
        item["expected"].push(sj::Value(width));
        item["expected"].push(sj::Value(height));
        item["expected"].push(sj::Value(blockWidth));
        item["expected"].push(sj::Value(blockHeight));
        item["actual"] = sj::Value::array();
        item["actual"].push(sj::Value(actualWidth));
        item["actual"].push(sj::Value(actualHeight));
        item["actual"].push(sj::Value(actualBlockWidth));
        item["actual"].push(sj::Value(actualBlockHeight));
        collector.add(item);
    }
    const auto blockAt = [&](int x, int y) {
        return (y / blockHeight) * blocksX + x / blockWidth;
    };

    const std::vector<std::uint8_t> expectedRgba =
        readBinary(outputPath(expectedDir, "render_rgba.raw"));
    const std::vector<std::uint8_t> actualRgba =
        readBinary(outputPath(actualDir, "render_rgba.raw"));
    const std::size_t expectedSize =
        static_cast<std::size_t>(width * height * 4);
    if (expectedRgba.size() != expectedSize || actualRgba.size() != expectedSize) {
        sj::Value item = sj::Value::object();
        item["kind"] = sj::Value("rgba-size");
        item["tile"] = sj::Value(-1);
        item["pixel"] = sj::Value::array();
        item["pixel"].push(sj::Value(-1));
        item["pixel"].push(sj::Value(-1));
        item["channel"] = sj::Value("all");
        item["expected"] = sj::Value(static_cast<double>(expectedRgba.size()));
        item["actual"] = sj::Value(static_cast<double>(actualRgba.size()));
        collector.add(item);
    } else {
        const char* channels[4] = {"r", "g", "b", "a"};
        for (std::size_t i = 0; i < expectedSize; ++i) {
            if (expectedRgba[i] == actualRgba[i]) continue;
            const int pixelIndex = static_cast<int>(i / 4);
            const int x = pixelIndex % width;
            const int y = pixelIndex / width;
            sj::Value item = sj::Value::object();
            item["kind"] = sj::Value("rgba");
            item["tile"] = sj::Value(blockAt(x, y));
            item["pixel"] = sj::Value::array();
            item["pixel"].push(sj::Value(x));
            item["pixel"].push(sj::Value(y));
            item["channel"] = sj::Value(channels[i % 4]);
            item["expected"] = sj::Value(expectedRgba[i]);
            item["actual"] = sj::Value(actualRgba[i]);
            collector.add(item);
        }
    }

    const std::vector<std::uint8_t> expectedOwner =
        readBinary(outputPath(expectedDir, "owner_map.raw"));
    const std::vector<std::uint8_t> actualOwner =
        readBinary(outputPath(actualDir, "owner_map.raw"));
    const std::size_t ownerSize = static_cast<std::size_t>(width * height);
    if (expectedOwner.size() != ownerSize || actualOwner.size() != ownerSize) {
        sj::Value item = sj::Value::object();
        item["kind"] = sj::Value("owner-size");
        item["tile"] = sj::Value(-1);
        item["pixel"] = sj::Value::array();
        item["pixel"].push(sj::Value(-1));
        item["pixel"].push(sj::Value(-1));
        item["channel"] = sj::Value("separation");
        item["expected"] = sj::Value(static_cast<double>(expectedOwner.size()));
        item["actual"] = sj::Value(static_cast<double>(actualOwner.size()));
        collector.add(item);
    } else {
        for (std::size_t i = 0; i < ownerSize; ++i) {
            if (expectedOwner[i] == actualOwner[i]) continue;
            const int x = static_cast<int>(i) % width;
            const int y = static_cast<int>(i) / width;
            sj::Value item = sj::Value::object();
            item["kind"] = sj::Value("owner");
            item["tile"] = sj::Value(blockAt(x, y));
            item["pixel"] = sj::Value::array();
            item["pixel"].push(sj::Value(x));
            item["pixel"].push(sj::Value(y));
            item["channel"] = sj::Value("separation");
            item["expected"] = sj::Value(expectedOwner[i]);
            item["actual"] = sj::Value(actualOwner[i]);
            collector.add(item);
        }
    }

    const sj::Value* eProjection = expectedReport.find("projection");
    const sj::Value* aProjection = actualReport.find("projection");
    const std::string expectedProjection =
        eProjection != nullptr && eProjection->contains("points")
            ? (*eProjection)["points"].dump()
            : "{}";
    const std::string actualProjection =
        aProjection != nullptr && aProjection->contains("points")
            ? (*aProjection)["points"].dump()
            : "{}";
    if (expectedProjection != actualProjection) {
        const auto& expectedPoints = (*eProjection)["points"].asArray();
        const auto& actualPoints = (*aProjection)["points"].asArray();
        const std::size_t count = std::min(expectedPoints.size(), actualPoints.size());
        for (std::size_t i = 0; i < count; ++i) {
            if (expectedPoints[i].dump() == actualPoints[i].dump()) continue;
            const sj::Value* expectedBoard = expectedPoints[i].find("boardUV");
            const sj::Value* actualBoard = actualPoints[i].find("boardUV");
            const int layer = static_cast<int>(
                expectedPoints[i].contains("layer")
                    ? expectedPoints[i]["layer"].asNumber()
                    : 0);
            const int light = static_cast<int>(
                expectedPoints[i].contains("light")
                    ? expectedPoints[i]["light"].asNumber()
                    : 0);
            sj::Value item = sj::Value::object();
            item["kind"] = sj::Value("projection");
            item["tile"] = sj::Value(-1);
            item["pixel"] = sj::Value::array();
            item["pixel"].push(sj::Value(-1));
            item["pixel"].push(sj::Value(-1));
            item["channel"] = sj::Value("boardUV");
            item["pointIndex"] = sj::Value(static_cast<double>(i));
            item["layer"] = sj::Value(layer);
            item["light"] = sj::Value(light);
            if (expectedBoard != nullptr && actualBoard != nullptr &&
                expectedBoard->isArray() && actualBoard->isArray() &&
                expectedBoard->asArray().size() == 2 &&
                actualBoard->asArray().size() == 2) {
                item["expected"] =
                    sj::Value(expectedBoard->asArray()[0].asNumber());
                item["actual"] =
                    sj::Value(actualBoard->asArray()[0].asNumber());
            } else {
                item["expectedDigest"] =
                    sj::Value(sl::hex64(sl::hashString(expectedPoints[i].dump())));
                item["actualDigest"] =
                    sj::Value(sl::hex64(sl::hashString(actualPoints[i].dump())));
            }
            collector.add(item);
        }
        if (expectedPoints.size() != actualPoints.size()) {
            sj::Value item = sj::Value::object();
            item["kind"] = sj::Value("projection-count");
            item["tile"] = sj::Value(-1);
            item["pixel"] = sj::Value::array();
            item["pixel"].push(sj::Value(-1));
            item["pixel"].push(sj::Value(-1));
            item["channel"] = sj::Value("coordinates");
            item["expected"] = sj::Value(static_cast<double>(expectedPoints.size()));
            item["actual"] = sj::Value(static_cast<double>(actualPoints.size()));
            collector.add(item);
        }
    }

    result["ok"] = sj::Value(collector.total == 0);
    result["code"] =
        sj::Value(collector.total == 0 ? "E_RESUME_MATCH" : "E_RESUME_MISMATCH");
    result["differenceCount"] = sj::Value(static_cast<double>(collector.total));
    result["differences"] = collector.differences;
    return result;
}

sj::Value invalidCase(const sj::Value& base,
                      const std::string& name,
                      const std::function<void(sj::Value&)>& mutate,
                      int expectedLayer) {
    sj::Value bad = base;
    mutate(bad);
    sl::Scene scene;
    std::vector<sl::Diagnostic> diagnostics;
    const bool valid = sl::parseScene(bad, scene, diagnostics);
    bool layerMatched = expectedLayer == 0;
    for (const sl::Diagnostic& diagnostic : diagnostics) {
        if (diagnostic.code == "E_SCENE_INVALID" &&
            diagnostic.layer == expectedLayer) {
            layerMatched = true;
        }
    }
    sj::Value result = sj::Value::object();
    result["name"] = sj::Value(name);
    result["passed"] = sj::Value(!valid && !diagnostics.empty() && layerMatched);
    result["businessCode"] =
        sj::Value(diagnostics.empty() ? "" : diagnostics.front().code);
    result["expectedLayer"] = sj::Value(expectedLayer);
    result["diagnosticCount"] = sj::Value(static_cast<double>(diagnostics.size()));
    return result;
}

bool sameBytes(const std::string& a, const std::string& b) {
    return readBinary(a) == readBinary(b);
}

int runSelfTest(const std::string& workDir) {
    std::filesystem::remove_all(workDir);
    std::filesystem::create_directories(workDir);
    sj::Value result = sj::Value::object();
    bool passed = true;

    const sj::Value sample = sl::makeSampleScene();
    const std::string scenePath = outputPath(workDir, "scene.json");
    sl::writeJsonFile(scenePath, sample, 2);

    sl::Scene parsedScene;
    std::vector<sl::Diagnostic> diagnostics;
    const bool sampleValid = sl::loadScene(scenePath, parsedScene, diagnostics);
    result["sampleValid"] = sj::Value(sampleValid);
    passed = passed && sampleValid;

    bool halftoneStable = true;
    bool phaseRule = true;
    sj::Value halftoneValues = sj::Value::array();
    for (const sl::ScreenLayer& layer : parsedScene.layers) {
        sl::HalftoneTable a =
            sl::HalftoneTable::make(layer.seed, layer.meshLineCount,
                                    layer.meshAngleDegrees);
        sl::HalftoneTable b =
            sl::HalftoneTable::make(layer.seed, layer.meshLineCount,
                                    layer.meshAngleDegrees);
        const bool same = a.digest() == b.digest() &&
                          a.detailBytes() == b.detailBytes() &&
                          a.edgeBytes() == b.edgeBytes();
        const sl::HalftoneTable phaseVariant =
            sl::HalftoneTable::make(layer.seed, layer.meshLineCount + 1,
                                    layer.meshAngleDegrees);
        halftoneStable = halftoneStable && same;
        phaseRule = phaseRule &&
                    phaseVariant.phaseU() == a.phaseU() &&
                    phaseVariant.phaseV() == a.phaseV();
        sj::Value item = sj::Value::object();
        item["layer"] = sj::Value(layer.layerIndex);
        item["digest"] = sj::Value(sl::hex64(a.digest()));
        item["byteIdentical"] = sj::Value(same);
        halftoneValues.push(std::move(item));
    }
    result["halftoneByteIdentical"] = sj::Value(halftoneStable);
    result["phaseDependsOnlyOnSeedAndAngle"] = sj::Value(phaseRule);
    result["halftoneTables"] = halftoneValues;
    passed = passed && halftoneStable && phaseRule;

    sj::Value invalid = sj::Value::array();
    const auto indexCase = invalidCase(
        sample, "mesh-index-out-of-range",
        [](sj::Value& value) {
            value["screens"].asArray()[0]["indices"].asArray()[0] = sj::Value(99);
        },
        1);
    const auto degenerateCase = invalidCase(
        sample, "degenerate-triangle",
        [](sj::Value& value) {
            value["screens"].asArray()[0]["indices"].asArray()[0] = sj::Value(0);
            value["screens"].asArray()[0]["indices"].asArray()[1] = sj::Value(0);
            value["screens"].asArray()[0]["indices"].asArray()[2] = sj::Value(1);
        },
        1);
    const auto transmittanceCase = invalidCase(
        sample, "transmittance-out-of-range",
        [](sj::Value& value) {
            value["screens"].asArray()[2]["inkTransmittance"] = sj::Value(1.5);
        },
        3);
    const auto duplicateCase = invalidCase(
        sample, "duplicate-separation-id",
        [](sj::Value& value) {
            value["screens"].asArray()[1]["separationId"] =
                value["screens"].asArray()[0]["separationId"];
        },
        2);
    const auto missingCase = invalidCase(
        sample, "missing-separation-id",
        [](sj::Value& value) {
            value["screens"].asArray()[3]["separationId"] = sj::Value("");
        },
        4);
    const auto lightCase = invalidCase(
        sample, "light-inside-screen",
        [](sj::Value& value) {
            const sj::Value& firstVertex =
                value["screens"].asArray()[0]["vertices"].asArray()[0];
            value["lights"].asArray()[0]["position"] = firstVertex;
            value["lights"].asArray()[0]["length"] = sj::Value(0);
        },
        1);
    for (const sj::Value* item :
         {&indexCase, &degenerateCase, &transmittanceCase, &duplicateCase,
          &missingCase, &lightCase}) {
        invalid.push(*item);
        passed = passed && (*item)["passed"].asBool();
    }
    result["invalidScenes"] = std::move(invalid);

    RenderOptions fullOptions;
    fullOptions.fresh = true;
    fullOptions.quiet = true;
    const std::string fullDir = outputPath(workDir, "full");
    const std::string fullDir2 = outputPath(workDir, "full-repeat");
    RenderOutcome full = runRender(scenePath, fullDir, fullOptions);
    RenderOutcome fullRepeat = runRender(scenePath, fullDir2, fullOptions);
    const bool fullOk = full.status == RenderStatus::Completed &&
                        fullRepeat.status == RenderStatus::Completed;
    result["fullRender"] = sj::Value(fullOk);
    passed = passed && fullOk;

    const bool deterministic =
        sameBytes(outputPath(fullDir, "render_rgba.raw"),
                  outputPath(fullDir2, "render_rgba.raw")) &&
        sameBytes(outputPath(fullDir, "owner_map.raw"),
                  outputPath(fullDir2, "owner_map.raw"));
    result["sameCameraPixelIdentical"] = sj::Value(deterministic);
    passed = passed && deterministic;

    RenderOptions stopOptions;
    stopOptions.fresh = true;
    stopOptions.quiet = true;
    stopOptions.stopAfterBlocks = 7;
    const std::string interruptedDir = outputPath(workDir, "interrupted");
    RenderOutcome interrupted = runRender(scenePath, interruptedDir, stopOptions);
    const bool interruptedOk =
        interrupted.status == RenderStatus::InterruptedResumable &&
        interrupted.exitCode == 0 &&
        std::filesystem::exists(outputPath(interruptedDir, "snapshot.json")) &&
        std::filesystem::exists(outputPath(interruptedDir, "recovery.json"));
    result["gracefulStopSnapshot"] = sj::Value(interruptedOk);
    passed = passed && interruptedOk;

    RenderOptions resumeOptions;
    resumeOptions.quiet = true;
    RenderOutcome resumed = runRender(scenePath, interruptedDir, resumeOptions);
    bool resumeMatches = resumed.status == RenderStatus::Completed;
    if (resumeMatches) {
        sj::Value diff =
            compareDirectories(fullDir, interruptedDir);
        resumeMatches = diff["ok"].asBool();
        result["resumeDifferenceCount"] = diff["differenceCount"];
    }
    result["resumeByteIdentical"] = sj::Value(resumeMatches);
    passed = passed && resumeMatches;

    sj::Value changedScene = sample;
    changedScene["screens"].asArray()[0]["mesh"]["seed"] =
        sj::Value(static_cast<double>(parsedScene.layers[0].seed + 9001ULL));
    const std::string changedScenePath = outputPath(workDir, "scene-changed.json");
    sl::writeJsonFile(changedScenePath, changedScene, 2);
    const std::string changedStaleDir = outputPath(workDir, "changed-stale");
    RenderOptions changedStop;
    changedStop.fresh = true;
    changedStop.quiet = true;
    changedStop.stopAfterBlocks = 4;
    RenderOutcome changedStopOutcome =
        runRender(scenePath, changedStaleDir, changedStop);
    bool sceneMismatchReset = changedStopOutcome.status ==
                              RenderStatus::InterruptedResumable;
    if (sceneMismatchReset) {
        RenderOutcome changedResume =
            runRender(changedScenePath, changedStaleDir, resumeOptions);
        const std::string changedFullDir = outputPath(workDir, "changed-full");
        RenderOutcome changedFull =
            runRender(changedScenePath, changedFullDir, fullOptions);
        sceneMismatchReset = changedResume.status == RenderStatus::Completed &&
                             changedFull.status == RenderStatus::Completed;
        if (sceneMismatchReset) {
            sj::Value diff = compareDirectories(changedFullDir, changedStaleDir);
            sceneMismatchReset = diff["ok"].asBool();
            std::string parseError;
            sj::Value resumeReport = sj::Value::parse(
                readText(outputPath(changedStaleDir, "resume_report.json")), parseError);
            const sj::Value* reason = resumeReport.find("snapshotResetReason");
            sceneMismatchReset = sceneMismatchReset && parseError.empty() &&
                                 reason != nullptr && reason->isString() &&
                                 reason->asString() == "snapshot-scene-mismatch";
        }
    }
    result["changedSceneSnapshotDiscarded"] = sj::Value(sceneMismatchReset);
    passed = passed && sceneMismatchReset;

    RenderOptions corruptStop;
    corruptStop.fresh = true;
    corruptStop.quiet = true;
    corruptStop.stopAfterBlocks = 4;
    const std::string corruptDir = outputPath(workDir, "corrupt-resume");
    RenderOutcome corruptStopOutcome = runRender(scenePath, corruptDir, corruptStop);
    bool corruptReset = corruptStopOutcome.status == RenderStatus::InterruptedResumable;
    if (corruptReset) {
        const std::string snapshotPath = outputPath(corruptDir, "snapshot.json");
        std::string snapshotText = readText(snapshotPath);
        snapshotText[0] = '!';
        {
            std::ofstream output(snapshotPath, std::ios::binary | std::ios::trunc);
            output << snapshotText;
        }
        RenderOutcome corruptResume = runRender(scenePath, corruptDir, resumeOptions);
        corruptReset = corruptResume.status == RenderStatus::Completed;
        if (corruptReset) {
            sj::Value diff = compareDirectories(fullDir, corruptDir);
            corruptReset = diff["ok"].asBool();
            const std::string resumeText =
                readText(outputPath(corruptDir, "resume_report.json"));
            std::string resumeParseError;
            sj::Value resumeReport =
                sj::Value::parse(resumeText, resumeParseError);
            const sj::Value* reason =
                resumeReport.find("snapshotResetReason");
            corruptReset = corruptReset && resumeParseError.empty() &&
                           reason != nullptr &&
                           reason->isString() &&
                           reason->asString().find("corrupt") != std::string::npos;
        }
    }
    result["corruptSnapshotDiscarded"] = sj::Value(corruptReset);
    passed = passed && corruptReset;

    const std::string reportText = readText(outputPath(fullDir, "report.json"));
    std::string reportError;
    sj::Value report = sj::Value::parse(reportText, reportError);
    bool moireOk = false;
    bool projectionOk = false;
    bool contributionsOk = false;
    if (reportError.empty()) {
        const sj::Value* pairs = report.find("moire");
        if (pairs != nullptr && pairs->contains("pairs")) {
            bool sawTriggered = false;
            bool sawUntriggered = false;
            for (const sj::Value& pair : (*pairs)["pairs"].asArray()) {
                const bool triggered = pair["triggered"].asBool();
                if (triggered) {
                    sawTriggered = true;
                    moireOk = pair["amplitudeAfter"].asNumber() <
                                  pair["amplitudeBefore"].asNumber() &&
                              pair["amplitudeReductionPercent"].asNumber() > 0.0;
                } else {
                    sawUntriggered = true;
                    moireOk = moireOk &&
                              std::fabs(pair["amplitudeAfter"].asNumber() -
                                        pair["amplitudeBefore"].asNumber()) <
                                  1.0e-12;
                }
            }
            moireOk = moireOk && sawTriggered && sawUntriggered;
        }
        const sj::Value* projection = report.find("projection");
        if (projection != nullptr && projection->contains("points")) {
            const auto& points = (*projection)["points"].asArray();
            bool sawInside = false;
            bool sawOutside = false;
            bool sawAdjacent = false;
            for (const sj::Value& point : points) {
                if (point["insideBackboard"].asBool()) sawInside = true;
                else sawOutside = true;
                if (point.contains("adjacentPoint") &&
                    !point["adjacentPoint"].isNull()) {
                    sawAdjacent = true;
                }
            }
            projectionOk = !points.empty() && sawInside && sawOutside && sawAdjacent;
        }
        const sj::Value* contributions = report.find("layerContributions");
        if (contributions != nullptr && contributions->isArray() &&
            contributions->asArray().size() == 4) {
            contributionsOk = true;
            for (const sj::Value& item : contributions->asArray()) {
                contributionsOk = contributionsOk &&
                                  item["transmissionRatio"].asNumber() > 0.0 &&
                                  item["reflectionRatio"].asNumber() > 0.0 &&
                                  std::fabs(item["transmissionRatio"].asNumber() +
                                            item["reflectionRatio"].asNumber() - 1.0) <
                                      1.0e-9;
            }
        }
    }
    result["moireSuppression"] = sj::Value(moireOk);
    result["projectionPoints"] = sj::Value(projectionOk);
    result["transmissionReflectionRatios"] = sj::Value(contributionsOk);
    passed = passed && moireOk && projectionOk && contributionsOk;

    sj::Value gapChanged = sample;
    gapChanged["airGaps"].asArray()[0] = sj::Value(4.5);
    sl::Scene gapScene;
    std::vector<sl::Diagnostic> gapDiagnostics;
    const bool gapValid = sl::parseScene(gapChanged, gapScene, gapDiagnostics);
    bool gapAffectsProjection = false;
    if (gapValid) {
        sl::Renderer baseRenderer(parsedScene);
        sl::Renderer gapRenderer(gapScene);
        const auto& basePoints = baseRenderer.projectionPoints();
        const auto& changedPoints = gapRenderer.projectionPoints();
        for (std::size_t i = 0; i < std::min(basePoints.size(), changedPoints.size()); ++i) {
            if (basePoints[i].layerIndex == 2 && basePoints[i].lightIndex == 0 &&
                changedPoints[i].layerIndex == 2 && changedPoints[i].lightIndex == 0) {
                const double du = basePoints[i].boardUV.x - changedPoints[i].boardUV.x;
                const double dv = basePoints[i].boardUV.y - changedPoints[i].boardUV.y;
                if (std::sqrt(du * du + dv * dv) > 1.0e-4) {
                    gapAffectsProjection = true;
                }
                break;
            }
        }
    }
    result["airGapMovesProjection"] = sj::Value(gapAffectsProjection);
    passed = passed && gapAffectsProjection;

    sj::Value lightChanged = sample;
    lightChanged["lights"].asArray()[0]["position"].asArray()[0] = sj::Value(-45.0);
    sl::Scene lightScene;
    std::vector<sl::Diagnostic> lightDiagnostics;
    const bool lightValid = sl::parseScene(lightChanged, lightScene, lightDiagnostics);
    bool lightAffectsProjection = false;
    if (lightValid) {
        sl::Renderer baseRenderer(parsedScene);
        sl::Renderer lightRenderer(lightScene);
        const auto& basePoints = baseRenderer.projectionPoints();
        const auto& changedPoints = lightRenderer.projectionPoints();
        for (std::size_t i = 0; i < std::min(basePoints.size(), changedPoints.size()); ++i) {
            if (basePoints[i].layerIndex == 2 && basePoints[i].lightIndex == 0 &&
                changedPoints[i].layerIndex == 2 && changedPoints[i].lightIndex == 0) {
                const double du = basePoints[i].boardUV.x - changedPoints[i].boardUV.x;
                const double dv = basePoints[i].boardUV.y - changedPoints[i].boardUV.y;
                if (std::sqrt(du * du + dv * dv) > 1.0e-4) {
                    lightAffectsProjection = true;
                }
                break;
            }
        }
    }
    result["lightMovesProjection"] = sj::Value(lightAffectsProjection);
    passed = passed && lightAffectsProjection;

    const std::vector<std::uint8_t> image =
        readBinary(outputPath(fullDir, "render_rgba.raw"));
    std::size_t pureWhite = 0;
    std::size_t chromatic = 0;
    const std::size_t pixelCount = image.size() / 4;
    for (std::size_t i = 0; i + 3 < image.size(); i += 4) {
        const std::uint8_t minimum =
            std::min({image[i], image[i + 1], image[i + 2]});
        const std::uint8_t maximum =
            std::max({image[i], image[i + 1], image[i + 2]});
        if (minimum >= 250) ++pureWhite;
        if (maximum - minimum > 10) ++chromatic;
    }
    const double whiteFraction =
        pixelCount > 0 ? static_cast<double>(pureWhite) / pixelCount : 1.0;
    const double chromaticFraction =
        pixelCount > 0 ? static_cast<double>(chromatic) / pixelCount : 0.0;
    const bool colorOk = whiteFraction < 0.01 && chromaticFraction > 0.2;
    result["pureWhiteFraction"] = sj::Value(whiteFraction);
    result["chromaticFraction"] = sj::Value(chromaticFraction);
    result["overexposureAndHueCheck"] = sj::Value(colorOk);
    passed = passed && colorOk;

    result["ok"] = sj::Value(passed);
    result["code"] = sj::Value(passed ? "E_SELF_TEST_OK" : "E_SELF_TEST_FAILED");
    result["workDir"] = sj::Value(workDir);
    sl::writeJsonFile(outputPath(workDir, "self_test.json"), result, 2);
    std::cout << result.dump(2) << "\n";
    return passed ? 0 : 5;
}

} // namespace

int main(int argc, char** argv) {
    try {
        g_interrupted = 0;
        std::signal(SIGINT, signalHandler);
#if defined(SIGTERM)
        std::signal(SIGTERM, signalHandler);
#endif
        if (argc < 2) {
            printUsage();
            return 1;
        }
        const Cli cli = parseCli(argc, argv);
        if (cli.command == "help" || cli.command == "--help" || cli.command == "-h") {
            printUsage();
            return 0;
        }
        if (cli.command == "sample") {
            const std::string output = optionalOutput(cli, "scene.json");
            const int seed = parseIntOption(cli, "seed", 20261118);
            if (seed < 0) throw std::runtime_error("--seed must be non-negative");
            sj::Value scene = sl::makeSampleScene(static_cast<std::uint64_t>(seed));
            sl::writeJsonFile(output, scene, 2);
            sj::Value result = okJson();
            result["code"] = sj::Value("E_SAMPLE_OK");
            result["output"] = sj::Value(output);
            std::cout << result.dump() << "\n";
            return 0;
        }
        if (cli.command == "validate") {
            sl::Scene scene;
            std::vector<sl::Diagnostic> diagnostics;
            const bool valid = sl::loadScene(required(cli, "scene"), scene, diagnostics);
            if (!valid) {
                std::cout << sl::diagnosticsJson(diagnostics).dump(2) << "\n";
                return 2;
            }
            sj::Value result = okJson();
            result["code"] = sj::Value("E_SCENE_VALID");
            result["sceneDigest"] = sj::Value(scene.sceneDigest);
            result["layerCount"] = sj::Value(static_cast<double>(scene.layers.size()));
            std::cout << result.dump(2) << "\n";
            return 0;
        }
        if (cli.command == "halftone") {
            sl::Scene scene;
            std::vector<sl::Diagnostic> diagnostics;
            if (!sl::loadScene(required(cli, "scene"), scene, diagnostics)) {
                std::cout << sl::diagnosticsJson(diagnostics).dump(2) << "\n";
                return 2;
            }
            const std::string outDir = required(cli, "out-dir");
            std::filesystem::create_directories(outDir);
            sj::Value manifest = sj::Value::object();
            manifest["sceneDigest"] = sj::Value(scene.sceneDigest);
            manifest["tables"] = sj::Value::array();
            for (const sl::ScreenLayer& layer : scene.layers) {
                const std::string name =
                    "layer-" + std::to_string(layer.layerIndex) + "-" +
                    layer.separationId + ".slht";
                layer.halftone.writeBinary(outputPath(outDir, name));
                sj::Value item = sj::Value::object();
                item["layer"] = sj::Value(layer.layerIndex);
                item["separationId"] = sj::Value(layer.separationId);
                item["file"] = sj::Value(name);
                item["digest"] = sj::Value(sl::hex64(layer.halftone.digest()));
                item["phaseU"] = sj::Value(layer.halftone.phaseU());
                item["phaseV"] = sj::Value(layer.halftone.phaseV());
                manifest["tables"].push(std::move(item));
            }
            sl::writeJsonFile(outputPath(outDir, "halftone_manifest.json"), manifest, 2);
            std::cout << manifest.dump(2) << "\n";
            return 0;
        }
        if (cli.command == "render") {
            RenderOptions options;
            options.fresh = cli.has("fresh");
            options.quiet = cli.has("quiet");
            options.stopAfterBlocks = parseIntOption(cli, "stop-after-blocks", 0);
            options.crashAfterBlocks = parseIntOption(cli, "crash-after-blocks", 0);
            options.tileDelayMs = parseIntOption(cli, "tile-delay-ms", 0);
            if (options.stopAfterBlocks < 0 || options.crashAfterBlocks < 0 ||
                options.tileDelayMs < 0) {
                throw std::runtime_error("block count and delay options must be non-negative");
            }
            RenderOutcome outcome =
                runRender(required(cli, "scene"), required(cli, "out-dir"), options);
            std::cout << outcome.summary.dump(2) << "\n";
            return outcome.exitCode;
        }
        if (cli.command == "compare") {
            const std::string expectedDir = required(cli, "expected");
            const std::string actualDir = required(cli, "actual");
            sj::Value diff = compareDirectories(expectedDir, actualDir);
            std::string out = optionalOutput(cli, outputPath(actualDir, "resume_diff.json"));
            sl::writeJsonFile(out, diff, 2);
            std::cout << diff.dump(2) << "\n";
            return diff["ok"].asBool() ? 0 : 4;
        }
        if (cli.command == "self-test") {
            return runSelfTest(cli.get("work-dir", outputPath("artifacts", "self-test")));
        }
        printUsage();
        return 1;
    } catch (const std::exception& error) {
        sj::Value result = sj::Value::object();
        result["ok"] = sj::Value(false);
        result["code"] = sj::Value("E_RUNTIME");
        result["message"] = sj::Value(error.what());
        std::cerr << result.dump(2) << "\n";
        return 3;
    }
}

