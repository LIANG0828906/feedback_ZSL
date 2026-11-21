#include "font_atlas/atlas.hpp"
#include "font_atlas/engine.hpp"
#include "font_atlas/font.hpp"
#include "font_atlas/io.hpp"
#include "font_atlas/json.hpp"
#include "font_atlas/snapshot.hpp"
#include "font_atlas/trace.hpp"
#include "font_atlas/types.hpp"
#include "font_atlas/workbench.hpp"

#include <algorithm>
#include <filesystem>
#include <iostream>
#include <map>
#include <set>
#include <sstream>
#include <string>
#include <vector>

namespace {

using namespace font_atlas;

struct Arguments {
    std::map<std::string, std::string> values;
    std::set<std::string> flags;

    std::string get(const std::string& key, const std::string& fallback = "") const {
        const auto iterator = values.find(key);
        return iterator == values.end() ? fallback : iterator->second;
    }
    bool has(const std::string& key) const { return flags.count(key) != 0; }
};

Result<Arguments> parseArguments(int argc, char** argv, int start,
                                 const std::set<std::string>& flagNames,
                                 const std::set<std::string>& valueNames) {
    Arguments arguments;
    for (int i = start; i < argc; ++i) {
        std::string token = argv[i];
        if (token.rfind("--", 0) != 0) {
            Diagnostic diagnostic;
            diagnostic.code = ErrorCode::InvalidArgument;
            diagnostic.message = "unexpected positional argument: " + token;
            return Result<Arguments>::failure(std::move(diagnostic));
        }
        token = token.substr(2);
        if (flagNames.count(token) != 0) {
            arguments.flags.insert(token);
            continue;
        }
        if (valueNames.count(token) == 0) {
            Diagnostic diagnostic;
            diagnostic.code = ErrorCode::InvalidArgument;
            diagnostic.message = "unknown option --" + token;
            return Result<Arguments>::failure(std::move(diagnostic));
        }
        if (i + 1 >= argc) {
            Diagnostic diagnostic;
            diagnostic.code = ErrorCode::InvalidArgument;
            diagnostic.message = "option --" + token + " requires a value";
            return Result<Arguments>::failure(std::move(diagnostic));
        }
        arguments.values[token] = argv[++i];
    }
    return Result<Arguments>::success(std::move(arguments));
}

void printDiagnostic(const Diagnostic& diagnostic) {
    std::cerr << errorCodeName(diagnostic.code);
    if (!diagnostic.glyph.empty()) {
        std::cerr << " glyph=" << diagnostic.glyph;
    }
    if (diagnostic.contourIndex >= 0) {
        std::cerr << " contour=" << diagnostic.contourIndex;
    }
    if (diagnostic.segmentIndex >= 0) {
        std::cerr << " segment=" << diagnostic.segmentIndex;
    }
    if (diagnostic.chunkIndex >= 0) {
        std::cerr << " chunk=" << diagnostic.chunkIndex;
    }
    if (diagnostic.pairedChunkIndex >= 0) {
        std::cerr << " pairedChunk=" << diagnostic.pairedChunkIndex;
    }
    if (diagnostic.pixelX >= 0) {
        std::cerr << " pixel=" << diagnostic.pixelX << "," << diagnostic.pixelY;
    }
    std::cerr << ": " << diagnostic.message << '\n';
}

Result<void> writeResult(const std::filesystem::path& path, const Json& result) {
    return writeFileAtomic(path, result.dump(2));
}

Json diagnosticJson(const Diagnostic& diagnostic) {
    Json item = Json::object();
    item["code"] = Json(errorCodeName(diagnostic.code));
    item["message"] = Json(diagnostic.message);
    if (!diagnostic.glyph.empty()) {
        item["glyph"] = Json(diagnostic.glyph);
    }
    if (diagnostic.contourIndex >= 0) {
        item["contour"] = Json(diagnostic.contourIndex);
    }
    if (diagnostic.segmentIndex >= 0) {
        item["segment"] = Json(diagnostic.segmentIndex);
    }
    if (diagnostic.chunkIndex >= 0) {
        item["chunk"] = Json(diagnostic.chunkIndex);
    }
    if (diagnostic.pairedChunkIndex >= 0) {
        item["pairedChunk"] = Json(diagnostic.pairedChunkIndex);
    }
    if (diagnostic.pixelX >= 0) {
        item["pixelX"] = Json(diagnostic.pixelX);
        item["pixelY"] = Json(diagnostic.pixelY);
    }
    return item;
}

bool parseInteger(const std::string& text, long long& value) {
    try {
        std::size_t used = 0;
        value = std::stoll(text, &used);
        return used == text.size();
    } catch (const std::exception&) {
        return false;
    }
}

bool parseDouble(const std::string& text, double& value) {
    try {
        std::size_t used = 0;
        value = std::stod(text, &used);
        return used == text.size();
    } catch (const std::exception&) {
        return false;
    }
}

Result<void> applyOverrides(const Arguments& arguments, WorkbenchConfig& config) {
    if (arguments.values.count("fill") != 0) {
        if (!parseFillRule(arguments.get("fill"), config.raster.fillRule)) {
            Diagnostic diagnostic;
            diagnostic.code = ErrorCode::InvalidArgument;
            diagnostic.message = "--fill must be nonzero or evenodd";
            return Result<void>::failure(std::move(diagnostic));
        }
    }
    const auto integerOverride = [&](const std::string& key, int& target,
                                     long long minimum, long long maximum) {
        if (arguments.values.count(key) == 0) {
            return Result<void>::success();
        }
        long long value = 0;
        if (!parseInteger(arguments.get(key), value) || value < minimum ||
            value > maximum) {
            Diagnostic diagnostic;
            diagnostic.code = ErrorCode::InvalidArgument;
            diagnostic.message = "--" + key + " must be in [" +
                                 std::to_string(minimum) + ", " +
                                 std::to_string(maximum) + "]";
            return Result<void>::failure(std::move(diagnostic));
        }
        target = static_cast<int>(value);
        return Result<void>::success();
    };
    auto result = integerOverride("size", config.raster.targetPixelSize, 1, 8192);
    if (!result.ok()) {
        return result;
    }
    result = integerOverride("quant", config.raster.quantizationStep, 0, 65535);
    if (!result.ok()) {
        return result;
    }
    result = integerOverride("samples-per-axis", config.raster.samplesPerAxis, 1, 16);
    if (!result.ok()) {
        return result;
    }
    result = integerOverride("tile-width", config.raster.tileWidth, 1, 8192);
    if (!result.ok()) {
        return result;
    }
    result = integerOverride("tile-height", config.raster.tileHeight, 1, 8192);
    if (!result.ok()) {
        return result;
    }
    result = integerOverride("workers", config.budget.parallelism, 1, 256);
    if (!result.ok()) {
        return result;
    }
    if (arguments.values.count("subpixel") != 0) {
        const std::string& value = arguments.get("subpixel");
        const std::size_t comma = value.find(',');
        if (comma == std::string::npos || !parseDouble(value.substr(0, comma),
                                                       config.raster.subpixelX) ||
            !parseDouble(value.substr(comma + 1), config.raster.subpixelY)) {
            Diagnostic diagnostic;
            diagnostic.code = ErrorCode::InvalidArgument;
            diagnostic.message = "--subpixel must be X,Y";
            return Result<void>::failure(std::move(diagnostic));
        }
    }
    return Result<void>::success();
}

Result<void> commandGenerate(const Arguments& arguments) {
    const std::string output = arguments.get("out");
    if (output.empty()) {
        Diagnostic diagnostic;
        diagnostic.code = ErrorCode::InvalidArgument;
        diagnostic.message = "generate requires --out DIRECTORY";
        return Result<void>::failure(std::move(diagnostic));
    }
    std::uint64_t seed = 42;
    if (arguments.values.count("seed") != 0) {
        long long parsed = 0;
        if (!parseInteger(arguments.get("seed"), parsed) || parsed < 0) {
            Diagnostic diagnostic;
            diagnostic.code = ErrorCode::InvalidArgument;
            diagnostic.message = "--seed must be a non-negative integer";
            return Result<void>::failure(std::move(diagnostic));
        }
        seed = static_cast<std::uint64_t>(parsed);
    }
    auto result = generateSampleSet(output, seed);
    if (!result.ok()) {
        return result;
    }
    std::cout << "generated samples in " << output << " seed=" << seed << '\n';
    return Result<void>::success();
}

Result<void> commandRaster(const Arguments& arguments) {
    const std::string fontPath = arguments.get("font");
    const std::string configPath = arguments.get("config");
    const std::string output = arguments.get("out");
    if (fontPath.empty() || configPath.empty() || output.empty()) {
        Diagnostic diagnostic;
        diagnostic.code = ErrorCode::InvalidArgument;
        diagnostic.message = "raster requires --font FILE --config FILE --out DIRECTORY";
        return Result<void>::failure(std::move(diagnostic));
    }
    auto config = loadWorkbenchConfig(configPath);
    if (!config.ok()) {
        return Result<void>::failure(std::move(config.error));
    }
    auto overrides = applyOverrides(arguments, *config.value);
    if (!overrides.ok()) {
        return overrides;
    }
    auto font = loadFontFile(fontPath);
    if (!font.ok()) {
        return Result<void>::failure(std::move(font.error));
    }
    const std::filesystem::path state =
        arguments.values.count("state") != 0
            ? std::filesystem::path(arguments.get("state"))
            : std::filesystem::path(output) / "state";
    const bool resume = arguments.has("resume");
    const bool persist = !arguments.has("no-persist");
    std::uint64_t hardExitAfter = 0;
    if (arguments.values.count("hard-exit-after") != 0) {
        long long parsed = 0;
        if (!parseInteger(arguments.get("hard-exit-after"), parsed) || parsed <= 0) {
            Diagnostic diagnostic;
            diagnostic.code = ErrorCode::InvalidArgument;
            diagnostic.message = "--hard-exit-after must be positive";
            return Result<void>::failure(std::move(diagnostic));
        }
        hardExitAfter = static_cast<std::uint64_t>(parsed);
    }

    TraceLog trace;
    Json header = Json::object();
    header["event"] = Json("run");
    header["font"] = Json(font.value->name);
    header["fontFingerprint"] = Json(font.value->fingerprint);
    header["fillRule"] = Json(fillRuleName(config.value->raster.fillRule));
    header["targetPixelSize"] = Json(config.value->raster.targetPixelSize);
    header["subpixelX"] = Json(config.value->raster.subpixelX);
    header["subpixelY"] = Json(config.value->raster.subpixelY);
    header["quantizationStep"] = Json(config.value->raster.quantizationStep);
    header["parallelism"] = Json(config.value->budget.parallelism);
    header["globalBudgetBytes"] = Json(config.value->budget.globalBudgetBytes);
    header["perTaskBudgetBytes"] = Json(config.value->budget.perTaskBudgetBytes);
    trace.add(std::move(header));

    FontEngine engine(*font.value, &trace);
    BatchRasterOptions batchOptions;
    batchOptions.resume = resume;
    batchOptions.continueOnGlyphFailure =
        arguments.has("continue-on-budget") ||
        arguments.has("continue-on-glyph-failure");
    batchOptions.persist = persist;
    batchOptions.hardExitAfterCommittedChunks = hardExitAfter;
    auto batch = rasterizeFontToAtlas(
        engine, *config.value, output, state, batchOptions, {}, {});
    const std::string tracePath =
        arguments.values.count("trace") != 0
            ? arguments.get("trace")
            : pathString(std::filesystem::path(output) / "trace.jsonl");
    if (batch.ok()) {
        Json atlasRecord = Json::object();
        atlasRecord["event"] = Json("atlas");
        atlasRecord["width"] = Json(batch.value->atlas.width);
        atlasRecord["height"] = Json(batch.value->atlas.height);
        atlasRecord["glyphCount"] =
            Json(static_cast<std::uint64_t>(batch.value->atlas.glyphs.size()));
        atlasRecord["imageHash"] = Json(batch.value->atlas.imageHash);
        trace.add(std::move(atlasRecord));
    }
    auto traceWrite = trace.writeJsonLines(tracePath);
    if (!traceWrite.ok()) {
        return traceWrite;
    }

    Json result = Json::object();
    result["command"] = Json("raster");
    result["fontFingerprint"] = Json(font.value->fingerprint);
    result["trace"] = Json(tracePath);
    if (!batch.ok()) {
        result["ok"] = Json(false);
        result["error"] = diagnosticJson(batch.error);
        auto write = writeResult(std::filesystem::path(output) / "result.json", result);
        if (!write.ok()) {
            return write;
        }
        printDiagnostic(batch.error);
        return Result<void>::failure(std::move(batch.error));
    }
    result["ok"] = Json(true);
    Json glyphs = Json::array();
    for (const auto& glyph : batch.value->glyphs) {
        Json item = Json::object();
        item["glyph"] = Json(glyph.glyph);
        item["coverageHash"] = Json(glyph.coverageHash);
        item["chunks"] = Json(static_cast<std::uint64_t>(glyph.chunks.size()));
        item["reusedChunks"] = Json(glyph.reusedChunks);
        item["recomputedChunks"] = Json(glyph.recomputedChunks);
        item["peakMemoryBytes"] = Json(glyph.peakMemoryBytes);
        glyphs.pushBack(item);
    }
    result["glyphs"] = glyphs;
    Json failures = Json::array();
    for (const auto& failure : batch.value->failures) {
        failures.pushBack(diagnosticJson(failure));
    }
    result["isolatedFailures"] = failures;
    result["atlas"] = Json::object();
    result["atlas"]["width"] = Json(batch.value->atlas.width);
    result["atlas"]["height"] = Json(batch.value->atlas.height);
    result["atlas"]["imageHash"] = Json(batch.value->atlas.imageHash);
    result["atlas"]["glyphs"] =
        Json(static_cast<std::uint64_t>(batch.value->atlas.glyphs.size()));
    auto write = writeResult(std::filesystem::path(output) / "result.json", result);
    if (!write.ok()) {
        return write;
    }
    std::cout << "atlas " << batch.value->atlas.width << "x"
              << batch.value->atlas.height << " glyphs="
              << batch.value->atlas.glyphs.size() << " isolatedFailures="
              << batch.value->failures.size() << " hash="
              << batch.value->atlas.imageHash << '\n';
    return Result<void>::success();
}

Result<void> commandCompareQuant(const Arguments& arguments) {
    const std::string fontPath = arguments.get("font");
    const std::string configPath = arguments.get("config");
    if (fontPath.empty() || configPath.empty() || arguments.values.count("old") == 0 ||
        arguments.values.count("new") == 0) {
        Diagnostic diagnostic;
        diagnostic.code = ErrorCode::InvalidArgument;
        diagnostic.message =
            "compare-quant requires --font FILE --config FILE --old STEP --new STEP";
        return Result<void>::failure(std::move(diagnostic));
    }
    long long oldStep = 0;
    long long newStep = 0;
    if (!parseInteger(arguments.get("old"), oldStep) ||
        !parseInteger(arguments.get("new"), newStep)) {
        Diagnostic diagnostic;
        diagnostic.code = ErrorCode::QuantInvalid;
        diagnostic.message = "quantization steps must be integers";
        return Result<void>::failure(std::move(diagnostic));
    }
    if (oldStep <= 0 || oldStep > 255 || newStep <= 0 || newStep > 255) {
        Diagnostic diagnostic;
        diagnostic.code = ErrorCode::QuantInvalid;
        diagnostic.message = "quantization steps must be in [1, 255]";
        return Result<void>::failure(std::move(diagnostic));
    }
    auto config = loadWorkbenchConfig(configPath);
    if (!config.ok()) {
        return Result<void>::failure(std::move(config.error));
    }
    auto overrides = applyOverrides(arguments, *config.value);
    if (!overrides.ok()) {
        return overrides;
    }
    auto font = loadFontFile(fontPath);
    if (!font.ok()) {
        return Result<void>::failure(std::move(font.error));
    }
    FontEngine engine(*font.value);
    std::vector<std::string> glyphs = font.value->glyphOrder;
    if (arguments.values.count("glyph") != 0) {
        const std::string requested = arguments.get("glyph");
        if (font.value->findGlyph(requested) == nullptr) {
            Diagnostic diagnostic;
            diagnostic.code = ErrorCode::FontInvalid;
            diagnostic.glyph = requested;
            diagnostic.message = "unknown glyph";
            return Result<void>::failure(std::move(diagnostic));
        }
        glyphs = {requested};
    }
    QuantErrorSummary total;
    total.oldStep = static_cast<int>(oldStep);
    total.newStep = static_cast<int>(newStep);
    Json root = Json::object();
    root["command"] = Json("compare-quant");
    root["oldStep"] = Json(static_cast<int>(oldStep));
    root["newStep"] = Json(static_cast<int>(newStep));
    Json glyphArray = Json::array();
    int maximum = 0;
    for (const auto& glyph : glyphs) {
        auto summary =
            engine.compareQuantization(glyph, static_cast<int>(oldStep),
                                       static_cast<int>(newStep),
                                       config.value->raster);
        if (!summary.ok()) {
            return Result<void>::failure(std::move(summary.error));
        }
        total.totalAbsoluteError += summary.value->totalAbsoluteError;
        total.comparedPixels += summary.value->comparedPixels;
        maximum = std::max(maximum, summary.value->glyphMaxAbsoluteError);
        Json item = Json::object();
        item["glyph"] = Json(glyph);
        item["glyphMaxAbsoluteError"] = Json(summary.value->glyphMaxAbsoluteError);
        item["totalAbsoluteError"] = Json(summary.value->totalAbsoluteError);
        item["comparedPixels"] = Json(summary.value->comparedPixels);
        item["meanAbsoluteError"] = Json(summary.value->meanAbsoluteError);
        glyphArray.pushBack(item);
    }
    total.glyphMaxAbsoluteError = maximum;
    if (total.comparedPixels > 0) {
        total.meanAbsoluteError =
            static_cast<double>(total.totalAbsoluteError) /
            static_cast<double>(total.comparedPixels);
    }
    root["glyphs"] = glyphArray;
    root["totalAbsoluteError"] = Json(total.totalAbsoluteError);
    root["comparedPixels"] = Json(total.comparedPixels);
    root["meanAbsoluteError"] = Json(total.meanAbsoluteError);
    root["glyphMaxAbsoluteError"] = Json(maximum);
    const std::string output =
        arguments.values.count("out") != 0 ? arguments.get("out") : "";
    if (!output.empty()) {
        auto write = writeResult(output, root);
        if (!write.ok()) {
            return write;
        }
    } else {
        std::cout << root.dump(2);
    }
    return Result<void>::success();
}

struct VerifyState {
    Json checks = Json::array();
    bool allPassed = true;

    void check(const std::string& name, bool passed, const std::string& detail) {
        Json item = Json::object();
        item["name"] = Json(name);
        item["passed"] = Json(passed);
        item["detail"] = Json(detail);
        checks.pushBack(item);
        if (!passed) {
            allPassed = false;
        }
        std::cout << (passed ? "PASS " : "FAIL ") << name << ": " << detail << '\n';
    }
};

const Json* findScenario(const Json& baseline, const std::string& name) {
    const Json* scenarios = baseline.find("scenarios");
    if (scenarios == nullptr || !scenarios->isArray()) {
        return nullptr;
    }
    for (const auto& item : scenarios->asArray()) {
        const Json* scenario = item.find("scenario");
        if (scenario != nullptr && scenario->isString() &&
            scenario->asString() == name) {
            return &item;
        }
    }
    return nullptr;
}

std::map<std::string, std::string> glyphHashes(const Json& scenario) {
    std::map<std::string, std::string> result;
    const Json* glyphs = scenario.find("glyphs");
    if (glyphs != nullptr && glyphs->isArray()) {
        for (const auto& glyph : glyphs->asArray()) {
            result[glyph.at("glyph").asString()] = glyph.at("coverageHash").asString();
        }
    }
    return result;
}

Result<void> commandVerify(const Arguments& arguments) {
    const std::string sampleDir = arguments.get("samples");
    const std::string output = arguments.get("out");
    if (sampleDir.empty() || output.empty()) {
        Diagnostic diagnostic;
        diagnostic.code = ErrorCode::InvalidArgument;
        diagnostic.message = "verify requires --samples DIR --out DIR";
        return Result<void>::failure(std::move(diagnostic));
    }
    const std::filesystem::path samples(sampleDir);
    if (!std::filesystem::exists(samples / "font.json")) {
        auto generated = generateSampleSet(samples, 42);
        if (!generated.ok()) {
            return generated;
        }
    }
    auto config = loadWorkbenchConfig(samples / "config.json");
    if (!config.ok()) {
        return Result<void>::failure(std::move(config.error));
    }
    auto font = loadFontFile(pathString(samples / "font.json"));
    if (!font.ok()) {
        return Result<void>::failure(std::move(font.error));
    }
    auto baselineJson = Json::parseFile(pathString(samples / "baseline.json"));
    if (!baselineJson.ok()) {
        return Result<void>::failure(std::move(baselineJson.error));
    }
    auto directory = ensureDirectory(output);
    if (!directory.ok()) {
        return directory;
    }
    VerifyState verify;

    for (const char* invalid : {"invalid_cycle.json", "invalid_unicode.json"}) {
        auto invalidFont = loadFontFile(pathString(samples / invalid));
        verify.check(std::string("invalid_font_") + invalid,
                     !invalidFont.ok() &&
                         invalidFont.error.code == ErrorCode::FontInvalid,
                     invalidFont.ok()
                         ? "invalid sample unexpectedly loaded"
                         : std::string(errorCodeName(invalidFont.error.code)) + " " +
                               invalidFont.error.message);
    }

    const std::vector<std::string> scenarios = {
        "nonzero_24", "evenodd_24", "nonzero_48",
        "evenodd_48", "nonzero_96", "evenodd_96"};
    std::map<std::string, std::map<std::string, std::string>> hashesByScenario;
    for (const auto& scenarioName : scenarios) {
        WorkbenchConfig scenarioConfig = *config.value;
        const std::size_t underscore = scenarioName.find('_');
        if (!parseFillRule(scenarioName.substr(0, underscore),
                           scenarioConfig.raster.fillRule)) {
            verify.check("scenario_config", false, "bad scenario fill rule");
            continue;
        }
        scenarioConfig.raster.targetPixelSize =
            std::stoi(scenarioName.substr(underscore + 1));
        TraceLog trace;
        FontEngine engine(*font.value, &trace);
        BatchRasterOptions scenarioOptions;
        scenarioOptions.persist = false;
        auto batch = rasterizeFontToAtlas(
            engine, scenarioConfig, std::filesystem::path(output) / ("scenario_" + scenarioName),
            std::filesystem::path(output) / "state" / scenarioName, scenarioOptions);
        if (!batch.ok()) {
            verify.check("raster_" + scenarioName, false,
                         std::string(errorCodeName(batch.error.code)) + ": " +
                             batch.error.message);
            continue;
        }
        const Json* expected = findScenario(*baselineJson.value, scenarioName);
        if (expected == nullptr) {
            verify.check("baseline_" + scenarioName, false, "missing baseline scenario");
            continue;
        }
        const auto expectedHashes = glyphHashes(*expected);
        bool passed = expectedHashes.size() == batch.value->glyphs.size();
        std::string detail = "hashes and whole/tiled seam agree";
        for (const auto& glyph : batch.value->glyphs) {
            const auto iterator = expectedHashes.find(glyph.glyph);
            if (iterator == expectedHashes.end() || iterator->second != glyph.coverageHash) {
                passed = false;
                detail = "coverage hash mismatch for " + glyph.glyph;
                break;
            }
            hashesByScenario[scenarioName][glyph.glyph] = glyph.coverageHash;
        }
        verify.check("raster_baseline_" + scenarioName, passed, detail);
    }

    const auto& nonzero = hashesByScenario["nonzero_48"];
    const auto& evenodd = hashesByScenario["evenodd_48"];
    const bool fillDifference =
        nonzero.count("ring_same_direction") != 0 &&
        evenodd.count("ring_same_direction") != 0 &&
        nonzero.at("ring_same_direction") != evenodd.at("ring_same_direction") &&
        nonzero.count("overlap_pair") != 0 && evenodd.count("overlap_pair") != 0 &&
        nonzero.at("overlap_pair") != evenodd.at("overlap_pair");
    verify.check("fill_rule_shapes_differ", fillDifference,
                 "same-direction hole and overlapping contours differ under nonzero/evenodd");

    {
        WorkbenchConfig firstConfig = *config.value;
        firstConfig.raster.targetPixelSize = 48;
        firstConfig.budget.parallelism = 1;
        WorkbenchConfig secondConfig = firstConfig;
        secondConfig.budget.parallelism = 4;
        FontEngine firstEngine(*font.value);
        FontEngine secondEngine(*font.value);
        BatchRasterOptions deterministicOptions;
        deterministicOptions.persist = false;
        auto first = rasterizeFontToAtlas(
            firstEngine, firstConfig, std::filesystem::path(output) / "determinism_1",
            std::filesystem::path(output) / "unused1", deterministicOptions);
        auto second = rasterizeFontToAtlas(
            secondEngine, secondConfig, std::filesystem::path(output) / "determinism_4",
            std::filesystem::path(output) / "unused4", deterministicOptions);
        bool passed = first.ok() && second.ok() &&
                      first.value->atlas.imageHash == second.value->atlas.imageHash;
        verify.check("parallelism_independent_output", passed,
                     passed ? "atlas hash identical at parallelism 1 and 4"
                            : "parallel output differs");
    }

    {
        auto rejectConfig = loadWorkbenchConfig(samples / "config_budget_reject.json");
        if (!rejectConfig.ok()) {
            verify.check("budget_rejection_isolation", false,
                         rejectConfig.error.message);
        } else {
            FontEngine engine(*font.value);
            BatchRasterOptions rejectOptions;
            rejectOptions.persist = false;
            rejectOptions.continueOnGlyphFailure = true;
            auto batch = rasterizeFontToAtlas(
                engine, *rejectConfig.value,
                std::filesystem::path(output) / "budget_reject",
                std::filesystem::path(output) / "unused_reject", rejectOptions);
            bool passed = batch.ok() && !batch.value->failures.empty() &&
                          !batch.value->glyphs.empty();
            std::set<std::string> failedGlyphs;
            std::string detail = "some glyphs rejected while others committed";
            if (batch.ok()) {
                for (const auto& failure : batch.value->failures) {
                    failedGlyphs.insert(failure.glyph);
                }
                for (const auto& metric : batch.value->atlas.glyphs) {
                    if (failedGlyphs.count(metric.glyph) != 0) {
                        passed = false;
                        detail = "rejected glyph leaked into atlas: " + metric.glyph;
                    }
                }
                if (batch.value->failures.empty() || batch.value->glyphs.empty()) {
                    detail = "expected mixed success/rejection result";
                }
            } else {
                detail = batch.error.message;
            }
            verify.check("budget_rejection_isolation", passed, detail);
        }
    }

    {
        const std::string glyph = "ring_hole";
        RasterRequest request;
        request.glyph = glyph;
        request.raster = config.value->raster;
        request.budget = config.value->budget;
        request.persist = true;
        request.stateDirectory =
            pathString(std::filesystem::path(output) / "resume_state");
        FontEngine engine(*font.value);
        auto first = engine.rasterize(request);
        bool passed = first.ok();
        std::string detail = "initial persisted run completed";
        if (passed) {
            request.resume = true;
            auto second = engine.rasterize(request);
            passed = second.ok() &&
                     second.value->coverageHash == first.value->coverageHash &&
                     second.value->reusedChunks ==
                         static_cast<std::uint64_t>(first.value->chunks.size()) &&
                     second.value->recomputedChunks == 0;
            detail = passed ? "all committed chunks reused without recomputation"
                            : "resume did not reuse committed chunk prefix";
        } else {
            detail = first.error.message;
        }
        verify.check("snapshot_resume_reuses_chunks", passed, detail);

        if (passed) {
            auto snapshotPath =
                std::filesystem::path(output) / "resume_state" / "snapshot.json";
            auto snapshotJson = Json::parseFile(pathString(snapshotPath));
            if (!snapshotJson.ok()) {
                verify.check("snapshot_hash_chain_validation", false,
                             snapshotJson.error.message);
            } else {
                (*snapshotJson.value)["hashChain"] = Json("tampered");
                auto write = writeFileAtomic(snapshotPath, snapshotJson.value->dump(2));
                auto tampered = engine.rasterize(request);
                verify.check("snapshot_hash_chain_validation",
                             write.ok() && !tampered.ok() &&
                                 tampered.error.code == ErrorCode::SnapshotMismatch,
                             tampered.ok() ? "tampered snapshot was accepted"
                                           : std::string(errorCodeName(tampered.error.code)) +
                                                 " " + tampered.error.message);
            }
        }
    }

    Json report = Json::object();
    report["format"] = Json("font-atlas-verification");
    report["version"] = Json(1);
    report["passed"] = Json(verify.allPassed);
    report["checks"] = verify.checks;
    auto write = writeResult(std::filesystem::path(output) / "verification.json", report);
    if (!write.ok()) {
        return write;
    }
    if (!verify.allPassed) {
        Diagnostic diagnostic;
        diagnostic.code = ErrorCode::Internal;
        diagnostic.message = "one or more verification checks failed";
        return Result<void>::failure(std::move(diagnostic));
    }
    std::cout << "all verification checks passed\n";
    return Result<void>::success();
}

void printUsage() {
    std::cout
        << "font_atlas commands:\n"
        << "  generate --out DIR [--seed N]\n"
        << "  raster --font FILE --config FILE --out DIR [--state DIR] [--resume]\n"
        << "         [--continue-on-budget|--continue-on-glyph-failure]\n"
        << "         [--fill nonzero|evenodd] [--size N]\n"
        << "         [--quant N] [--samples-per-axis N] [--tile-width N]\n"
        << "         [--tile-height N] [--subpixel X,Y] [--workers N]\n"
        << "         [--hard-exit-after N] [--no-persist] [--trace FILE]\n"
        << "  compare-quant --font FILE --config FILE --old STEP --new STEP\n"
        << "         [--glyph NAME] [--out FILE] [raster overrides]\n"
        << "  verify --samples DIR --out DIR\n";
}

}  // namespace

int main(int argc, char** argv) {
    if (argc < 2) {
        printUsage();
        return 1;
    }
    const std::string command = argv[1];
    const std::set<std::string> flags = {"resume", "continue-on-budget",
                                         "continue-on-glyph-failure",
                                         "no-persist"};
    const std::set<std::string> values = {
        "out",      "seed",       "font",       "config",     "state",
        "trace",    "fill",       "size",       "quant",      "samples-per-axis",
        "tile-width", "tile-height", "subpixel", "workers", "hard-exit-after",
        "old",      "new",        "glyph",      "samples"};
    auto arguments = parseArguments(argc, argv, 2, flags, values);
    if (!arguments.ok()) {
        printDiagnostic(arguments.error);
        return 2;
    }
    Diagnostic initialDiagnostic;
    initialDiagnostic.code = ErrorCode::InvalidArgument;
    initialDiagnostic.message = "no command executed";
    Result<void> result = Result<void>::failure(std::move(initialDiagnostic));
    if (command == "generate") {
        result = commandGenerate(*arguments.value);
    } else if (command == "raster") {
        result = commandRaster(*arguments.value);
    } else if (command == "compare-quant") {
        result = commandCompareQuant(*arguments.value);
    } else if (command == "verify") {
        result = commandVerify(*arguments.value);
    } else if (command == "help" || command == "--help") {
        printUsage();
        return 0;
    } else {
        Diagnostic diagnostic;
        diagnostic.code = ErrorCode::InvalidArgument;
        diagnostic.message = "unknown command: " + command;
        result = Result<void>::failure(std::move(diagnostic));
    }
    if (!result.ok()) {
        if (result.error.code != ErrorCode::Cancelled) {
            printDiagnostic(result.error);
        }
        return 2;
    }
    return 0;
}
