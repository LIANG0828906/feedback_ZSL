#include "font_atlas/workbench.hpp"

#include "font_atlas/atlas.hpp"
#include "font_atlas/io.hpp"
#include "font_atlas/json.hpp"
#include "font_atlas/raster.hpp"
#include "font_atlas/sha256.hpp"
#include "font_atlas/trace.hpp"

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <filesystem>
#include <iomanip>
#include <sstream>

namespace font_atlas {
namespace {

Diagnostic configError(const std::string& message) {
    Diagnostic diagnostic;
    diagnostic.code = ErrorCode::InvalidArgument;
    diagnostic.message = message;
    return diagnostic;
}

Json degradeJson(const std::vector<std::string>& sequence) {
    Json array = Json::array();
    for (const auto& item : sequence) {
        array.pushBack(Json(item));
    }
    return array;
}

Json rasterJson(const RasterOptions& options) {
    Json object = Json::object();
    object["fillRule"] = Json(fillRuleName(options.fillRule));
    object["quantizationStep"] = Json(options.quantizationStep);
    object["samplesPerAxis"] = Json(options.samplesPerAxis);
    object["targetPixelSize"] = Json(options.targetPixelSize);
    object["subpixelX"] = Json(options.subpixelX);
    object["subpixelY"] = Json(options.subpixelY);
    object["tileWidth"] = Json(options.tileWidth);
    object["tileHeight"] = Json(options.tileHeight);
    object["seamTolerance"] = Json(options.seamTolerance);
    return object;
}

Json budgetJson(const BudgetOptions& budget) {
    Json object = Json::object();
    object["parallelism"] = Json(budget.parallelism);
    object["globalBudgetBytes"] = Json(budget.globalBudgetBytes);
    object["perTaskBudgetBytes"] = Json(budget.perTaskBudgetBytes);
    object["sampleBatchSize"] = Json(budget.sampleBatchSize);
    object["degradeSequence"] = degradeJson(budget.degradeSequence);
    return object;
}

Json atlasJson(const AtlasOptions& atlas) {
    Json object = Json::object();
    object["width"] = Json(atlas.width);
    object["height"] = Json(atlas.height);
    object["padding"] = Json(atlas.padding);
    return object;
}

std::string safeDirectoryName(const std::string& glyph, std::size_t index) {
    std::string result;
    for (unsigned char c : glyph) {
        if ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
            (c >= '0' && c <= '9') || c == '_' || c == '-') {
            result.push_back(static_cast<char>(c));
        } else {
            result.push_back('_');
        }
    }
    if (result.empty()) {
        result = "glyph";
    }
    return result + "_" + std::to_string(index) + "_" + Sha256::hex(glyph).substr(0, 8);
}

Json pointJson(const PointD& point) {
    Json value = Json::object();
    value["x"] = Json(point.x);
    value["y"] = Json(point.y);
    return value;
}

Json lineSegment(const PointD& to) {
    Json segment = Json::object();
    segment["type"] = Json("line");
    segment["to"] = pointJson(to);
    return segment;
}

Json quadSegment(const PointD& control, const PointD& to) {
    Json segment = Json::object();
    segment["type"] = Json("quad");
    segment["control"] = pointJson(control);
    segment["to"] = pointJson(to);
    return segment;
}

Json cubicSegment(const PointD& c1, const PointD& c2, const PointD& to) {
    Json segment = Json::object();
    segment["type"] = Json("cubic");
    segment["control1"] = pointJson(c1);
    segment["control2"] = pointJson(c2);
    segment["to"] = pointJson(to);
    return segment;
}

Json closeSegment() {
    Json segment = Json::object();
    segment["type"] = Json("close");
    return segment;
}

Json contourJson(const PointD& start, const std::string& direction,
                 const std::vector<Json>& segments) {
    Json contour = Json::object();
    contour["start"] = pointJson(start);
    contour["direction"] = Json(direction);
    Json array = Json::array();
    for (const auto& segment : segments) {
        array.pushBack(segment);
    }
    contour["segments"] = array;
    return contour;
}

class XorShift {
public:
    explicit XorShift(std::uint64_t seed) : state_(seed == 0 ? 0x9e3779b97f4a7c15ull : seed) {}
    std::uint64_t next() {
        state_ ^= state_ >> 12u;
        state_ ^= state_ << 25u;
        state_ ^= state_ >> 27u;
        return state_ * 0x2545f4914f6cdd1dull;
    }
    double jitter(double amount) {
        const double unit = static_cast<double>(next() >> 11u) /
                            static_cast<double>(1ull << 53u);
        return (unit * 2.0 - 1.0) * amount;
    }

private:
    std::uint64_t state_;
};

PointD jitterPoint(const PointD& point, XorShift& random) {
    return {point.x + random.jitter(4.0), point.y + random.jitter(4.0)};
}

Json makeEllipseContour(const PointD& center, double radiusX, double radiusY,
                        const std::string& direction, XorShift& random) {
    static constexpr double kappa = 0.5522847498307936;
    const double kx = radiusX * kappa;
    const double ky = radiusY * kappa;
    const PointD left{center.x - radiusX, center.y};
    const PointD top{center.x, center.y + radiusY};
    const PointD right{center.x + radiusX, center.y};
    const PointD bottom{center.x, center.y - radiusY};
    std::vector<Json> segments;
    segments.push_back(cubicSegment(
        jitterPoint({left.x, left.y + ky}, random),
        jitterPoint({top.x - kx, top.y}, random), jitterPoint(top, random)));
    segments.push_back(cubicSegment(
        jitterPoint({top.x + kx, top.y}, random),
        jitterPoint({right.x, right.y + ky}, random), jitterPoint(right, random)));
    segments.push_back(cubicSegment(
        jitterPoint({right.x, right.y - ky}, random),
        jitterPoint({bottom.x + kx, bottom.y}, random), jitterPoint(bottom, random)));
    segments.push_back(cubicSegment(
        jitterPoint({bottom.x - kx, bottom.y}, random),
        jitterPoint({left.x, left.y - ky}, random), jitterPoint(left, random)));
    segments.push_back(closeSegment());
    return contourJson(jitterPoint(left, random), direction, segments);
}

Json makeRectangleContour(const PointD& x0y0, const PointD& x1y1,
                          const std::string& direction, XorShift& random) {
    const PointD a = jitterPoint(x0y0, random);
    const PointD b = jitterPoint({x1y1.x, x0y0.y}, random);
    const PointD c = jitterPoint(x1y1, random);
    const PointD d = jitterPoint({x0y0.x, x1y1.y}, random);
    return contourJson(a, direction,
                       {lineSegment(b), lineSegment(c), lineSegment(d), closeSegment()});
}

Json makeQuadBlobContour(XorShift& random) {
    const PointD start = jitterPoint({90, 40}, random);
    return contourJson(
        start, "ccw",
        {quadSegment(jitterPoint({230, 920}, random), jitterPoint({520, 760}, random)),
         quadSegment(jitterPoint({850, 650}, random), jitterPoint({900, 250}, random)),
         quadSegment(jitterPoint({850, -80}, random), jitterPoint({420, 30}, random)),
         closeSegment()});
}

Json makeGlyph(const std::string& name, const std::string& unicode, int advance,
               const std::vector<Json>& contours,
               const std::vector<Json>& components = {}) {
    Json glyph = Json::object();
    glyph["name"] = Json(name);
    glyph["unicode"] = Json(unicode);
    glyph["advance"] = Json(advance);
    Json contourArray = Json::array();
    for (const auto& contour : contours) {
        contourArray.pushBack(contour);
    }
    glyph["contours"] = contourArray;
    if (!components.empty()) {
        Json componentArray = Json::array();
        for (const auto& component : components) {
            componentArray.pushBack(component);
        }
        glyph["components"] = componentArray;
    }
    return glyph;
}

Json component(const std::string& glyph, double dx, double dy, double scale = 1.0) {
    Json value = Json::object();
    value["glyph"] = Json(glyph);
    value["dx"] = Json(dx);
    value["dy"] = Json(dy);
    value["scale"] = Json(scale);
    return value;
}

Json makeSampleFont(std::uint64_t seed) {
    XorShift random(seed);
    Json font = Json::object();
    font["version"] = Json(1);
    font["name"] = Json("DeterministicHoleFont");
    font["unitsPerEm"] = Json(1000);
    font["ascent"] = Json(820);
    font["descent"] = Json(-180);
    Json glyphs = Json::array();

    glyphs.pushBack(makeGlyph(
        "ring_same_direction", "U+0041", 1000,
        {makeEllipseContour({500, 400}, 400, 400, "cw", random),
         makeEllipseContour({500, 400}, 220, 220, "cw", random)}));
    glyphs.pushBack(makeGlyph(
        "ring_hole", "U+004F", 1000,
        {makeEllipseContour({500, 400}, 400, 400, "cw", random),
         makeEllipseContour({500, 400}, 220, 220, "ccw", random)}));
    glyphs.pushBack(makeGlyph("quad_blob", "U+0050", 950, {makeQuadBlobContour(random)}));
    glyphs.pushBack(makeGlyph(
        "overlap_pair", "U+0053", 1000,
        {makeRectangleContour({80, 80}, {560, 700}, "ccw", random),
         makeRectangleContour({440, 180}, {900, 800}, "ccw", random)}));
    glyphs.pushBack(makeGlyph(
        "Oacute", "U+00D3", 1050,
        {makeEllipseContour({500, 390}, 390, 390, "cw", random),
         makeEllipseContour({500, 390}, 210, 210, "ccw", random)},
        {component("ring_hole", 0, 0, 0.72),
         component("dot", 710, 740, 1.0)}));
    glyphs.pushBack(makeGlyph(
        "dot", "U+002E", 180,
        {makeRectangleContour({15, 20}, {65, 70}, "ccw", random)}));
    font["glyphs"] = glyphs;
    return font;
}

Json makeInvalidFont(bool cycle) {
    Json font = Json::object();
    font["version"] = Json(1);
    font["name"] = Json(cycle ? "CyclicComponents" : "DuplicateUnicode");
    font["unitsPerEm"] = Json(1000);
    Json glyphs = Json::array();
    if (cycle) {
        Json a = Json::object();
        a["name"] = Json("a");
        a["advance"] = Json(500);
        Json componentsA = Json::array();
        componentsA.pushBack(component("b", 0, 0));
        a["components"] = componentsA;
        glyphs.pushBack(a);
        Json b = Json::object();
        b["name"] = Json("b");
        b["advance"] = Json(500);
        Json componentsB = Json::array();
        componentsB.pushBack(component("a", 0, 0));
        b["components"] = componentsB;
        glyphs.pushBack(b);
    } else {
        XorShift firstRandom(1);
        XorShift secondRandom(2);
        glyphs.pushBack(makeGlyph(
            "first", "U+0041", 500,
            {makeRectangleContour({10, 10}, {400, 700}, "ccw", firstRandom)}));
        glyphs.pushBack(makeGlyph(
            "second", "U+0041", 500,
            {makeRectangleContour({10, 10}, {400, 700}, "ccw", secondRandom)}));
    }
    font["glyphs"] = glyphs;
    return font;
}

Result<void> writeJsonFile(const std::filesystem::path& path, const Json& value) {
    return writeFileAtomic(path, value.dump(2));
}

Result<void> writeBaseline(const Font& font, const WorkbenchConfig& config,
                           const std::vector<std::string>& scenarios,
                           const std::filesystem::path& path) {
    Json root = Json::object();
    root["format"] = Json("font-atlas-baseline");
    root["version"] = Json(1);
    root["fontFingerprint"] = Json(font.fingerprint);
    Json items = Json::array();
    for (const auto& scenario : scenarios) {
        WorkbenchConfig current = config;
        std::string fillText = scenario.substr(0, scenario.find('_'));
        if (!parseFillRule(fillText, current.raster.fillRule)) {
            return Result<void>::failure(configError("bad baseline scenario"));
        }
        const std::size_t underscore = scenario.find('_');
        current.raster.targetPixelSize =
            std::stoi(scenario.substr(underscore + 1));
        FontEngine engine(font);
        Json item = Json::object();
        item["scenario"] = Json(scenario);
        item["fillRule"] = Json(fillRuleName(current.raster.fillRule));
        item["targetPixelSize"] = Json(current.raster.targetPixelSize);
        item["tileWidth"] = Json(current.raster.tileWidth);
        item["tileHeight"] = Json(current.raster.tileHeight);
        item["samplesPerAxis"] = Json(current.raster.samplesPerAxis);
        Json glyphs = Json::array();
        for (const auto& glyphName : font.glyphOrder) {
            RasterRequest request;
            request.glyph = glyphName;
            request.raster = current.raster;
            request.budget = current.budget;
            auto result = engine.rasterize(request);
            if (!result.ok()) {
                return Result<void>::failure(result.error);
            }
            Json glyph = Json::object();
            glyph["glyph"] = Json(glyphName);
            glyph["width"] = Json(result.value->width);
            glyph["height"] = Json(result.value->height);
            glyph["coverageHash"] = Json(result.value->coverageHash);
            glyph["chunks"] = Json(static_cast<std::uint64_t>(result.value->chunks.size()));
            glyphs.pushBack(glyph);
        }
        item["glyphs"] = glyphs;
        items.pushBack(item);
    }
    root["scenarios"] = items;
    return writeJsonFile(path, root);
}

}  // namespace

Result<WorkbenchConfig> loadWorkbenchConfig(const std::filesystem::path& path) {
    auto document = Json::parseFile(pathString(path));
    if (!document.ok()) {
        return Result<WorkbenchConfig>::failure(std::move(document.error));
    }
    if (!document.value->isObject()) {
        return Result<WorkbenchConfig>::failure(
            configError("config root must be an object"));
    }
    const Json& root = *document.value;
    Diagnostic diagnostic;
    WorkbenchConfig config;
    const Json* raster = root.find("raster");
    const Json* budget = root.find("budget");
    const Json* atlas = root.find("atlas");
    if (raster == nullptr || !raster->isObject() || budget == nullptr ||
        !budget->isObject() || atlas == nullptr || !atlas->isObject()) {
        return Result<WorkbenchConfig>::failure(
            configError("config requires raster, budget, and atlas objects"));
    }
    const std::string fill = jsonString(*raster, "fillRule", "nonzero", diagnostic);
    if (!parseFillRule(fill, config.raster.fillRule)) {
        return Result<WorkbenchConfig>::failure(
            configError("fillRule must be nonzero or evenodd"));
    }
    config.raster.quantizationStep =
        jsonInt(*raster, "quantizationStep", 1, diagnostic);
    config.raster.samplesPerAxis = jsonInt(*raster, "samplesPerAxis", 4, diagnostic);
    config.raster.targetPixelSize = jsonInt(*raster, "targetPixelSize", 48, diagnostic);
    config.raster.subpixelX = jsonDouble(*raster, "subpixelX", 0.0, diagnostic);
    config.raster.subpixelY = jsonDouble(*raster, "subpixelY", 0.0, diagnostic);
    config.raster.tileWidth = jsonInt(*raster, "tileWidth", 11, diagnostic);
    config.raster.tileHeight = jsonInt(*raster, "tileHeight", 9, diagnostic);
    config.raster.seamTolerance = jsonInt(*raster, "seamTolerance", 0, diagnostic);
    config.budget.parallelism = jsonInt(*budget, "parallelism", 4, diagnostic);
    config.budget.globalBudgetBytes =
        jsonUInt64(*budget, "globalBudgetBytes", 64ull * 1024ull * 1024ull, diagnostic);
    config.budget.perTaskBudgetBytes =
        jsonUInt64(*budget, "perTaskBudgetBytes", 4ull * 1024ull * 1024ull, diagnostic);
    config.budget.sampleBatchSize =
        jsonUInt64(*budget, "sampleBatchSize", 4096, diagnostic);
    if (const Json* degrade = budget->find("degradeSequence")) {
        if (!degrade->isArray()) {
            return Result<WorkbenchConfig>::failure(
                configError("degradeSequence must be an array"));
        }
        config.budget.degradeSequence.clear();
        for (const auto& item : degrade->asArray()) {
            if (!item.isString()) {
                return Result<WorkbenchConfig>::failure(
                    configError("degradeSequence entries must be strings"));
            }
            config.budget.degradeSequence.push_back(item.asString());
        }
    }
    config.atlas.width = jsonInt(*atlas, "width", 512, diagnostic);
    config.atlas.height = jsonInt(*atlas, "height", 512, diagnostic);
    config.atlas.padding = jsonInt(*atlas, "padding", 1, diagnostic);
    if (!diagnostic.ok()) {
        return Result<WorkbenchConfig>::failure(std::move(diagnostic));
    }
    auto rasterValidation = validateRasterOptions(config.raster);
    if (!rasterValidation.ok()) {
        return Result<WorkbenchConfig>::failure(std::move(rasterValidation.error));
    }
    if (config.budget.parallelism <= 0 || config.budget.parallelism > 256 ||
        config.budget.sampleBatchSize == 0) {
        return Result<WorkbenchConfig>::failure(
            configError("parallelism and sampleBatchSize must be positive"));
    }
    for (const auto& operation : config.budget.degradeSequence) {
        if (operation != "halve_batch" && operation != "split_tile") {
            return Result<WorkbenchConfig>::failure(
                configError("unknown degrade operation: " + operation));
        }
    }
    if (config.atlas.width <= 0 || config.atlas.height <= 0 ||
        config.atlas.padding < 0) {
        return Result<WorkbenchConfig>::failure(
            configError("atlas dimensions must be positive"));
    }
    return Result<WorkbenchConfig>::success(std::move(config));
}

std::string workbenchConfigJson(const WorkbenchConfig& config) {
    Json root = Json::object();
    root["raster"] = rasterJson(config.raster);
    root["budget"] = budgetJson(config.budget);
    root["atlas"] = atlasJson(config.atlas);
    return root.dump(2);
}

Result<void> saveWorkbenchConfig(const std::filesystem::path& path,
                                 const WorkbenchConfig& config) {
    return writeFileAtomic(path, workbenchConfigJson(config));
}

Result<BatchRasterResult> rasterizeFontToAtlas(
    FontEngine& engine, const WorkbenchConfig& config,
    const std::filesystem::path& outputDirectory,
    const std::filesystem::path& stateDirectory,
    const BatchRasterOptions& options,
    const CancelCheck& cancelled,
    const ProgressCallback& progress) {
    auto directory = ensureDirectory(outputDirectory);
    if (!directory.ok()) {
        return Result<BatchRasterResult>::failure(std::move(directory.error));
    }
    BatchRasterResult batch;
    for (std::size_t i = 0; i < engine.font().glyphOrder.size(); ++i) {
        if (cancelled && cancelled()) {
            Diagnostic diagnostic;
            diagnostic.code = ErrorCode::Cancelled;
            diagnostic.message = "font batch cancelled";
            return Result<BatchRasterResult>::failure(std::move(diagnostic));
        }
        const std::string& glyph = engine.font().glyphOrder[i];
        RasterRequest request;
        request.glyph = glyph;
        request.raster = config.raster;
        request.budget = config.budget;
        request.persist = options.persist;
        request.resume = options.resume;
        request.hardExitAfterCommittedChunks =
            options.hardExitAfterCommittedChunks;
        request.stateDirectory =
            pathString(stateDirectory / safeDirectoryName(glyph, i));
        auto result = engine.rasterize(request, cancelled, progress);
        if (result.ok()) {
            batch.glyphs.push_back(std::move(*result.value));
            continue;
        }
        const bool recoverableGlyphFailure =
            result.error.code != ErrorCode::Cancelled &&
            result.error.code != ErrorCode::SnapshotMismatch &&
            result.error.code != ErrorCode::IoError &&
            result.error.code != ErrorCode::InvalidArgument &&
            result.error.code != ErrorCode::AtlasOverflow;
        if (recoverableGlyphFailure && options.continueOnGlyphFailure) {
            batch.failures.push_back(std::move(result.error));
            continue;
        }
        return Result<BatchRasterResult>::failure(std::move(result.error));
    }
    auto atlas = packAtlas(batch.glyphs, config.atlas, outputDirectory);
    if (!atlas.ok()) {
        return Result<BatchRasterResult>::failure(std::move(atlas.error));
    }
    batch.atlas = std::move(*atlas.value);
    return Result<BatchRasterResult>::success(std::move(batch));
}

Result<void> generateSampleSet(const std::filesystem::path& outputDirectory,
                               std::uint64_t seed) {
    auto directory = ensureDirectory(outputDirectory);
    if (!directory.ok()) {
        return directory;
    }
    const Json fontJson = makeSampleFont(seed);
    auto fontWrite = writeJsonFile(outputDirectory / "font.json", fontJson);
    if (!fontWrite.ok()) {
        return fontWrite;
    }
    Json alternativeFont = makeSampleFont(seed ^ 0x9e3779b97f4a7c15ull);
    alternativeFont["name"] = Json("DeterministicHoleFontAlternate");
    auto alternateWrite =
        writeJsonFile(outputDirectory / "font_alt.json", alternativeFont);
    if (!alternateWrite.ok()) {
        return alternateWrite;
    }
    auto invalidCycle = writeJsonFile(outputDirectory / "invalid_cycle.json",
                                      makeInvalidFont(true));
    if (!invalidCycle.ok()) {
        return invalidCycle;
    }
    auto invalidUnicode = writeJsonFile(outputDirectory / "invalid_unicode.json",
                                        makeInvalidFont(false));
    if (!invalidUnicode.ok()) {
        return invalidUnicode;
    }

    WorkbenchConfig base;
    base.raster.targetPixelSize = 48;
    base.raster.samplesPerAxis = 4;
    base.raster.tileWidth = 11;
    base.raster.tileHeight = 9;
    base.raster.subpixelX = 0.25;
    base.raster.subpixelY = 0.125;
    base.budget.parallelism = 4;
    base.budget.globalBudgetBytes = 64ull * 1024ull * 1024ull;
    base.budget.perTaskBudgetBytes = 4ull * 1024ull * 1024ull;
    base.budget.sampleBatchSize = 4096;
    base.budget.degradeSequence = {"halve_batch", "split_tile"};
    base.atlas = {512, 512, 1};
    auto configWrite = saveWorkbenchConfig(outputDirectory / "config.json", base);
    if (!configWrite.ok()) {
        return configWrite;
    }

    const int sizes[] = {24, 48, 96};
    for (int size : sizes) {
        WorkbenchConfig sized = base;
        sized.raster.targetPixelSize = size;
        sized.raster.tileWidth = size <= 24 ? 7 : (size <= 48 ? 11 : 17);
        sized.raster.tileHeight = size <= 24 ? 5 : (size <= 48 ? 9 : 13);
        auto write =
            saveWorkbenchConfig(outputDirectory / ("config_" + std::to_string(size) + ".json"),
                                sized);
        if (!write.ok()) {
            return write;
        }
    }
    for (const char* fill : {"nonzero", "evenodd"}) {
        WorkbenchConfig filled = base;
        parseFillRule(fill, filled.raster.fillRule);
        auto write = saveWorkbenchConfig(
            outputDirectory / (std::string("config_") + fill + ".json"), filled);
        if (!write.ok()) {
            return write;
        }
    }
    WorkbenchConfig reject = base;
    reject.raster.targetPixelSize = 96;
    reject.raster.samplesPerAxis = 1;
    reject.raster.tileWidth = 24;
    reject.raster.tileHeight = 24;
    reject.budget.perTaskBudgetBytes = 1500;
    reject.budget.sampleBatchSize = 256;
    reject.budget.degradeSequence = {"halve_batch"};
    auto rejectWrite =
        saveWorkbenchConfig(outputDirectory / "config_budget_reject.json", reject);
    if (!rejectWrite.ok()) {
        return rejectWrite;
    }

    Json operations = Json::object();
    operations["format"] = Json("font-atlas-operations");
    operations["version"] = Json(1);
    operations["seed"] = Json(seed);
    Json operationArray = Json::array();
    for (const char* scenario : {"nonzero_24", "evenodd_24", "nonzero_48",
                                 "evenodd_48", "nonzero_96", "evenodd_96"}) {
        Json item = Json::object();
        item["operation"] = Json("raster");
        item["scenario"] = Json(scenario);
        operationArray.pushBack(item);
    }
    operations["operations"] = operationArray;
    auto operationsWrite = writeJsonFile(outputDirectory / "operations.json", operations);
    if (!operationsWrite.ok()) {
        return operationsWrite;
    }

    auto loaded = parseFont(fontJson);
    if (!loaded.ok()) {
        return Result<void>::failure(std::move(loaded.error));
    }
    auto baseline = writeBaseline(
        *loaded.value, base,
        {"nonzero_24", "evenodd_24", "nonzero_48", "evenodd_48", "nonzero_96",
         "evenodd_96"},
        outputDirectory / "baseline.json");
    if (!baseline.ok()) {
        return baseline;
    }
    Json manifest = Json::object();
    manifest["format"] = Json("font-atlas-sample-set");
    manifest["version"] = Json(1);
    manifest["seed"] = Json(seed);
    manifest["fontFingerprint"] = Json(loaded.value->fingerprint);
    manifest["files"] = Json::array();
    manifest["files"].pushBack(Json("font.json"));
    manifest["files"].pushBack(Json("font_alt.json"));
    manifest["files"].pushBack(Json("config.json"));
    manifest["files"].pushBack(Json("config_24.json"));
    manifest["files"].pushBack(Json("config_48.json"));
    manifest["files"].pushBack(Json("config_96.json"));
    manifest["files"].pushBack(Json("config_nonzero.json"));
    manifest["files"].pushBack(Json("config_evenodd.json"));
    manifest["files"].pushBack(Json("config_budget_reject.json"));
    manifest["files"].pushBack(Json("operations.json"));
    manifest["files"].pushBack(Json("baseline.json"));
    manifest["files"].pushBack(Json("invalid_cycle.json"));
    manifest["files"].pushBack(Json("invalid_unicode.json"));
    return writeJsonFile(outputDirectory / "manifest.json", manifest);
}

}  // namespace font_atlas
