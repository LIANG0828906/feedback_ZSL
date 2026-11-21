#include "font_atlas/atlas.hpp"
#include "font_atlas/engine.hpp"
#include "font_atlas/font.hpp"
#include "font_atlas/io.hpp"
#include "font_atlas/json.hpp"
#include "font_atlas/raster.hpp"
#include "font_atlas/sha256.hpp"
#include "font_atlas/snapshot.hpp"
#include "font_atlas/trace.hpp"
#include "font_atlas/workbench.hpp"

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <filesystem>
#include <iostream>
#include <map>
#include <mutex>
#include <set>
#include <string>
#include <thread>
#include <vector>

#ifndef _WIN32
#include <sys/wait.h>
#include <unistd.h>
#endif

namespace {

using namespace font_atlas;

int failures = 0;

void check(bool condition, const std::string& name, const std::string& detail = {}) {
    if (condition) {
        std::cout << "PASS " << name << '\n';
    } else {
        ++failures;
        std::cout << "FAIL " << name;
        if (!detail.empty()) {
            std::cout << ": " << detail;
        }
        std::cout << '\n';
    }
}

RasterRequest requestFor(const Font& font, const WorkbenchConfig& config,
                         const std::string& glyph) {
    (void)font;
    RasterRequest request;
    request.glyph = glyph;
    request.raster = config.raster;
    request.budget = config.budget;
    return request;
}

std::string fileHash(const std::filesystem::path& path) {
    auto bytes = readBinaryFile(path);
    if (!bytes.ok()) {
        return "MISSING";
    }
    return Sha256::hex(bytes.value->data(), bytes.value->size());
}

void testFontValidation(const std::filesystem::path& samples) {
    auto cycle = loadFontFile(pathString(samples / "invalid_cycle.json"));
    check(!cycle.ok() && cycle.error.code == ErrorCode::FontInvalid,
          "component cycle returns E_FONT_INVALID",
          cycle.ok() ? "" : cycle.error.message);

    auto duplicate = loadFontFile(pathString(samples / "invalid_unicode.json"));
    check(!duplicate.ok() && duplicate.error.code == ErrorCode::FontInvalid,
          "duplicate Unicode returns E_FONT_INVALID",
          duplicate.ok() ? "" : duplicate.error.message);
    check(!duplicate.ok() && duplicate.error.glyph == "second",
          "duplicate Unicode diagnostic locates glyph",
          duplicate.ok() ? "" : duplicate.error.glyph);

    const std::string missingControl = R"({
      "version":1,"unitsPerEm":1000,"glyphs":[
        {"name":"bad","advance":500,"contours":[
          {"start":{"x":0,"y":0},"segments":[
            {"type":"quad","to":{"x":10,"y":10}}
          ]}
        ]}
      ]})";
    auto missing = Json::parse(missingControl);
    check(missing.ok(), "missing-control fixture parses");
    if (missing.ok()) {
        auto font = parseFont(*missing.value);
        check(!font.ok() && font.error.code == ErrorCode::FontInvalid &&
                  font.error.glyph == "bad" && font.error.segmentIndex == 0,
              "missing quadratic control locates glyph/segment",
              font.ok() ? "" : font.error.message);
    }

    const std::string badType = R"({
      "version":1,"unitsPerEm":1000,"glyphs":[
        {"name":"bad","advance":500,"contours":[
          {"start":{"x":0,"y":0},"segments":[
            {"type":"spiral","to":{"x":10,"y":10}}
          ]}
        ]}
      ]})";
    auto typeDocument = Json::parse(badType);
    if (typeDocument.ok()) {
        auto font = parseFont(*typeDocument.value);
        check(!font.ok() && font.error.code == ErrorCode::FontInvalid &&
                  font.error.glyph == "bad" && font.error.segmentIndex == 0,
              "illegal segment type locates glyph/segment",
              font.ok() ? "" : font.error.message);
    } else {
        check(false, "bad-type fixture parses");
    }

    const std::string indexed = R"({
      "version":1,"unitsPerEm":1000,"glyphs":[
        {"name":"indexed","advance":500,"contours":[
          {"points":[[0,0],[100,0],[100,100],[0,100]],
           "segments":[
             {"type":"line","from":0,"to":1},
             {"type":"line","from":1,"to":2},
             {"type":"line","from":2,"to":3},
             {"type":"close"}
           ]}
        ]}
      ]})";
    auto indexedDocument = Json::parse(indexed);
    if (indexedDocument.ok()) {
        auto font = parseFont(*indexedDocument.value);
        check(font.ok() && font.value->findGlyph("indexed") != nullptr &&
                  font.value->findGlyph("indexed")->contours.size() == 1,
              "indexed point/from references stay continuous",
              font.ok() ? "" : font.error.message);
    } else {
        check(false, "indexed fixture parses");
    }
}

void testFillRulesAndQuantization(const Font& font, const WorkbenchConfig& config) {
    FontEngine engine(font);
    WorkbenchConfig nonzero = config;
    nonzero.raster.fillRule = FillRule::NonZero;
    nonzero.raster.targetPixelSize = 48;
    WorkbenchConfig evenodd = nonzero;
    evenodd.raster.fillRule = FillRule::EvenOdd;
    auto ringNonzero =
        engine.rasterize(requestFor(font, nonzero, "ring_same_direction"));
    auto ringEvenodd =
        engine.rasterize(requestFor(font, evenodd, "ring_same_direction"));
    auto overlapNonzero =
        engine.rasterize(requestFor(font, nonzero, "overlap_pair"));
    auto overlapEvenodd =
        engine.rasterize(requestFor(font, evenodd, "overlap_pair"));
    check(ringNonzero.ok() && ringEvenodd.ok() && overlapNonzero.ok() &&
              overlapEvenodd.ok(),
          "both fill rules rasterize curves and composite contours");
    if (ringNonzero.ok() && ringEvenodd.ok() && overlapNonzero.ok() &&
        overlapEvenodd.ok()) {
        check(ringNonzero.value->coverageHash != ringEvenodd.value->coverageHash,
              "nonzero/evenodd differ for same-direction hole");
        check(overlapNonzero.value->coverageHash !=
                  overlapEvenodd.value->coverageHash,
              "nonzero/evenodd differ for overlapping contours");
        check(ringNonzero.value->chunks.size() > 1,
              "glyph is split into multiple chunks");
        check(ringNonzero.value->recomputedChunks ==
                  static_cast<std::uint64_t>(ringNonzero.value->chunks.size()),
              "all chunks counted exactly once");
        std::uint64_t covered = 0;
        for (std::uint8_t pixel : ringEvenodd.value->coverage) {
            covered += pixel > 0 ? 1u : 0u;
        }
        std::uint64_t center = ringEvenodd.value->width / 2 +
                               (ringEvenodd.value->height / 2) *
                                   ringEvenodd.value->width;
        check(center < ringEvenodd.value->coverage.size() &&
                  ringEvenodd.value->coverage[center] == 0,
              "oriented hole remains empty under evenodd");
        check(covered > 0, "coverage image contains filled pixels");
    }

    RasterOptions invalid = config.raster;
    invalid.quantizationStep = 0;
    auto zero = engine.compareQuantization("ring_hole", 0, 8, invalid);
    check(!zero.ok() && zero.error.code == ErrorCode::QuantInvalid,
          "zero quantization step returns E_QUANT_INVALID");
    auto tooLarge = engine.compareQuantization("ring_hole", 1, 256, config.raster);
    check(!tooLarge.ok() && tooLarge.error.code == ErrorCode::QuantInvalid,
          "out-of-byte-range quantization returns E_QUANT_INVALID");
    auto summary = engine.compareQuantization("ring_hole", 1, 8, config.raster);
    check(summary.ok() && summary.value->totalAbsoluteError > 0 &&
              summary.value->glyphMaxAbsoluteError > 0 &&
              summary.value->meanAbsoluteError > 0.0,
          "quantization comparison reports total and per-glyph max error",
          summary.ok() ? "" : summary.error.message);
}

void testBusyAndCancel(const Font& font, const WorkbenchConfig& config) {
    FontEngine engine(font);
    std::mutex mutex;
    std::condition_variable condition;
    bool started = false;
    bool release = false;
    std::atomic<bool> cancel{false};
    auto request = requestFor(font, config, "ring_hole");
    request.budget.parallelism = 1;
    std::thread first([&]() {
        engine.rasterize(
            request, [&]() { return cancel.load(); },
            [&](const ProgressEvent& event) {
                if (event.kind == ProgressKind::ChunkStarted) {
                    std::unique_lock<std::mutex> lock(mutex);
                    started = true;
                    condition.notify_all();
                    condition.wait(lock, [&]() { return release; });
                }
            });
    });
    {
        std::unique_lock<std::mutex> lock(mutex);
        condition.wait_for(lock, std::chrono::seconds(5),
                           [&]() { return started; });
    }
    auto busy = engine.rasterize(request);
    check(!busy.ok() && busy.error.code == ErrorCode::Busy,
          "duplicate glyph launch returns E_BUSY");
    cancel.store(true);
    {
        std::lock_guard<std::mutex> lock(mutex);
        release = true;
        condition.notify_all();
    }
    first.join();
    check(!engine.isGlyphBusy("ring_hole"),
          "busy guard is released after cancellation");

    std::atomic<bool> cancelled{false};
    std::atomic<int> completed{0};
    WorkbenchConfig cancelConfig = config;
    cancelConfig.raster.targetPixelSize = 96;
    cancelConfig.raster.tileWidth = 8;
    cancelConfig.raster.tileHeight = 8;
    cancelConfig.budget.parallelism = 2;
    auto cancelRequest = requestFor(font, cancelConfig, "Oacute");
    auto cancelledResult = engine.rasterize(
        cancelRequest, [&]() { return cancelled.load(); },
        [&](const ProgressEvent& event) {
            if (event.kind == ProgressKind::ChunkCompleted && ++completed >= 2) {
                cancelled.store(true);
            }
        });
    check(!cancelledResult.ok() &&
              cancelledResult.error.code == ErrorCode::Cancelled &&
              completed.load() >= 2,
          "cooperative cancellation stops after committed chunk checkpoints",
          cancelledResult.ok() ? "run completed before cancellation" :
                                 cancelledResult.error.message);
}

void testBudgetIsolation(const Font& font, const WorkbenchConfig& config) {
    WorkbenchConfig reject = config;
    reject.raster.targetPixelSize = 96;
    reject.raster.samplesPerAxis = 1;
    reject.raster.tileWidth = 24;
    reject.raster.tileHeight = 24;
    reject.budget.perTaskBudgetBytes = 1500;
    reject.budget.sampleBatchSize = 256;
    reject.budget.degradeSequence = {"halve_batch"};
    TraceLog trace;
    FontEngine engine(font, &trace);
    BatchRasterOptions options;
    options.persist = false;
    options.continueOnGlyphFailure = true;
    auto batch = rasterizeFontToAtlas(
        engine, reject, "test_work/budget", "test_work/budget_state", options);
    check(batch.ok() && !batch.value->failures.empty() &&
              !batch.value->glyphs.empty(),
          "budget rejection isolates failed glyphs",
          batch.ok() ? "" : batch.error.message);
    if (batch.ok()) {
        std::set<std::string> failed;
        for (const auto& failure : batch.value->failures) {
            failed.insert(failure.glyph);
            check(failure.code == ErrorCode::BudgetExceeded,
                  "failed glyph reports E_BUDGET_EXCEEDED: " + failure.glyph);
        }
        bool leaked = false;
        for (const auto& metric : batch.value->atlas.glyphs) {
            leaked = leaked || failed.count(metric.glyph) != 0;
        }
        check(!leaked, "rejected glyph is absent from atlas manifest");
    }
}

void testSnapshotAndCrash(const Font& font, const WorkbenchConfig& config) {
    FontEngine fullEngine(font);
    auto full =
        fullEngine.rasterize(requestFor(font, config, "ring_hole"));
    check(full.ok(), "uninterrupted raster completes");
    if (!full.ok()) {
        return;
    }
    const std::filesystem::path state = "test_work/snapshot_state";
    std::error_code error;
    std::filesystem::remove_all(state, error);
    RasterRequest request = requestFor(font, config, "ring_hole");
    request.persist = true;
    request.stateDirectory = pathString(state);
    FontEngine snapshotEngine(font);
    auto first = snapshotEngine.rasterize(request);
    check(first.ok(), "persistent raster writes snapshot");
    if (!first.ok()) {
        return;
    }
    auto originalSnapshot = readTextFile(state / "snapshot.json");
    if (originalSnapshot.ok()) {
        auto versionDocument = Json::parse(*originalSnapshot.value);
        if (versionDocument.ok()) {
            (*versionDocument.value)["version"] = Json(99);
            writeFileAtomic(state / "snapshot.json",
                            versionDocument.value->dump(2));
            request.resume = true;
            auto wrongVersion = snapshotEngine.rasterize(request);
            check(!wrongVersion.ok() &&
                      wrongVersion.error.code == ErrorCode::SnapshotMismatch,
                  "unsupported snapshot version returns E_SNAPSHOT_MISMATCH");
            writeFileAtomic(state / "snapshot.json", *originalSnapshot.value);
        }
    }
    request.resume = true;
    auto resumed = snapshotEngine.rasterize(request);
    check(resumed.ok() && resumed.value->coverageHash == full.value->coverageHash &&
              resumed.value->reusedChunks ==
                  static_cast<std::uint64_t>(first.value->chunks.size()) &&
              resumed.value->recomputedChunks == 0,
          "resume reuses every committed chunk without recomputation");

    auto document = Json::parseFile(pathString(state / "snapshot.json"));
    if (document.ok()) {
        (*document.value)["hashChain"] = Json("broken");
        writeFileAtomic(state / "snapshot.json", document.value->dump(2));
        auto mismatch = snapshotEngine.rasterize(request);
        check(!mismatch.ok() && mismatch.error.code == ErrorCode::SnapshotMismatch,
              "tampered hash chain returns E_SNAPSHOT_MISMATCH");
    } else {
        check(false, "snapshot JSON can be loaded");
    }

#ifndef _WIN32
    const std::filesystem::path crashState = "test_work/crash_state";
    std::filesystem::remove_all(crashState, error);
    pid_t child = fork();
    if (child == 0) {
        FontEngine childEngine(font);
        RasterRequest crashRequest = requestFor(font, config, "ring_hole");
        crashRequest.persist = true;
        crashRequest.stateDirectory = pathString(crashState);
        crashRequest.hardExitAfterCommittedChunks = 2;
        childEngine.rasterize(crashRequest);
        std::_Exit(91);
    }
    int status = 0;
    if (child > 0) {
        waitpid(child, &status, 0);
    }
    check(child > 0 && WIFEXITED(status) && WEXITSTATUS(status) == 77,
          "child process is forcibly terminated after two committed chunks");
    FontEngine crashEngine(font);
    RasterRequest crashResume = requestFor(font, config, "ring_hole");
    crashResume.persist = true;
    crashResume.resume = true;
    crashResume.stateDirectory = pathString(crashState);
    auto recovered = crashEngine.rasterize(crashResume);
    check(recovered.ok() &&
              recovered.value->coverageHash == full.value->coverageHash &&
              recovered.value->reusedChunks == 2 &&
              recovered.value->recomputedChunks ==
                  static_cast<std::uint64_t>(recovered.value->chunks.size() - 2),
          "forced-termination recovery uses only committed disk prefix",
          recovered.ok() ? "" : recovered.error.message);
#endif
}

void testAtlasOverflow(const Font& font, const WorkbenchConfig& config) {
    FontEngine engine(font);
    auto glyph = engine.rasterize(requestFor(font, config, "ring_hole"));
    if (!glyph.ok()) {
        check(false, "overflow fixture rasterizes");
        return;
    }
    std::vector<RasterResult> glyphs;
    glyphs.push_back(std::move(*glyph.value));
    AtlasOptions tiny{8, 8, 1};
    auto atlas = packAtlas(glyphs, tiny, "test_work/overflow");
    check(!atlas.ok() && atlas.error.code == ErrorCode::AtlasOverflow,
          "undersized atlas returns E_ATLAS_OVERFLOW");
    check(!std::filesystem::exists("test_work/overflow/atlas.json"),
          "overflow does not publish partial atlas manifest");
}

void testDeterminism(const Font& font, const WorkbenchConfig& config) {
    auto render = [&](TraceLog& trace) {
        FontEngine engine(font, &trace);
        return engine.rasterize(requestFor(font, config, "Oacute"));
    };
    TraceLog firstTrace;
    TraceLog secondTrace;
    auto first = render(firstTrace);
    auto second = render(secondTrace);
    check(first.ok() && second.ok() &&
              first.value->coverageHash == second.value->coverageHash &&
              first.value->peakMemoryBytes == second.value->peakMemoryBytes &&
              firstTrace.toJsonLines() == secondTrace.toJsonLines(),
          "same operations produce byte-identical trace and coverage");

    auto generateA = generateSampleSet("test_work/samples_a", 12345);
    auto generateB = generateSampleSet("test_work/samples_b", 12345);
    bool identical = generateA.ok() && generateB.ok();
    const std::vector<std::string> files = {
        "font.json", "config.json", "baseline.json", "operations.json",
        "invalid_cycle.json", "invalid_unicode.json", "manifest.json"};
    for (const auto& file : files) {
        identical = identical &&
                    fileHash(std::filesystem::path("test_work/samples_a") / file) ==
                        fileHash(std::filesystem::path("test_work/samples_b") / file);
    }
    check(identical, "sample generator is byte-identical for the same seed");
}

}  // namespace

int main() {
    std::error_code error;
    std::filesystem::remove_all("test_work", error);
    std::filesystem::create_directories("test_work", error);

    auto generated = generateSampleSet("test_work/samples", 42);
    check(generated.ok(), "sample set generation succeeds",
          generated.ok() ? "" : generated.error.message);
    if (!generated.ok()) {
        return 1;
    }
    auto font = loadFontFile("test_work/samples/font.json");
    auto config = loadWorkbenchConfig("test_work/samples/config.json");
    check(font.ok(), "generated font parses");
    check(config.ok(), "generated config parses");
    if (!font.ok() || !config.ok()) {
        return 1;
    }
    check(font.value->findUnicode(0x004f) != nullptr,
          "Unicode mapping resolves glyph");
    check(font.value->findGlyph("Oacute") != nullptr &&
              font.value->findGlyph("Oacute")->contours.size() >= 3,
          "composite glyph expands referenced contours");

    testFontValidation("test_work/samples");
    testFillRulesAndQuantization(*font.value, *config.value);
    testBusyAndCancel(*font.value, *config.value);
    testBudgetIsolation(*font.value, *config.value);
    testSnapshotAndCrash(*font.value, *config.value);
    testAtlasOverflow(*font.value, *config.value);
    testDeterminism(*font.value, *config.value);

    if (failures != 0) {
        std::cout << failures << " test(s) failed\n";
        return 1;
    }
    std::cout << "all core tests passed\n";
    return 0;
}
