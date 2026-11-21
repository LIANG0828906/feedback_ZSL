#include "font_atlas/engine.hpp"

#include "font_atlas/io.hpp"
#include "font_atlas/sha256.hpp"
#include "font_atlas/snapshot.hpp"

#include <algorithm>
#include <atomic>
#include <cmath>
#include <condition_variable>
#include <cstdlib>
#include <deque>
#include <filesystem>
#include <functional>
#include <map>
#include <mutex>
#include <optional>
#include <thread>
#include <utility>
#include <vector>

namespace font_atlas {
namespace {

Diagnostic makeDiagnostic(ErrorCode code, const std::string& glyph,
                          const std::string& message) {
    Diagnostic diagnostic;
    diagnostic.code = code;
    diagnostic.glyph = glyph;
    diagnostic.message = message;
    return diagnostic;
}

struct BudgetPlanResult {
    std::vector<ChunkPlan> chunks;
    std::uint64_t sampleBatchSize = 1;
};

struct Planner {
    const RasterOptions& raster;
    const BudgetOptions& budget;
    std::uint64_t limit = 0;
    const CancelCheck& cancelled;
    std::string glyph;

    bool isCancelled() const { return cancelled && cancelled(); }

    bool canSplit(const PixelRect& rect) const { return rect.width > 1 || rect.height > 1; }

    void split(const PixelRect& rect, PixelRect& a, PixelRect& b, PixelRect& c,
               PixelRect& d) const {
        const int leftWidth = (rect.width + 1) / 2;
        const int topHeight = (rect.height + 1) / 2;
        a = {rect.x, rect.y, leftWidth, topHeight};
        b = {rect.x + leftWidth, rect.y, rect.width - leftWidth, topHeight};
        c = {rect.x, rect.y + topHeight, leftWidth, rect.height - topHeight};
        d = {rect.x + leftWidth, rect.y + topHeight, rect.width - leftWidth,
             rect.height - topHeight};
    }

    Result<std::vector<ChunkPlan>> planRegion(const PixelRect& rect, int level,
                                              std::uint64_t batch, std::size_t sequencePos) {
        if (isCancelled()) {
            return Result<std::vector<ChunkPlan>>::failure(
                makeDiagnostic(ErrorCode::Cancelled, glyph, "chunk planning cancelled"));
        }
        if (level > 40) {
            return Result<std::vector<ChunkPlan>>::failure(makeDiagnostic(
                ErrorCode::BudgetExceeded, glyph,
                "chunk degradation exceeded the maximum split depth"));
        }
        const std::uint64_t estimate = estimatedTaskBytes(rect, raster, batch);
        if (estimate <= limit) {
            ChunkPlan chunk;
            chunk.splitLevel = level;
            chunk.rect = rect;
            chunk.estimatedBytes = estimate;
            chunk.samples = sampleCountFor(rect, raster);
            chunk.batchSize = batch;
            return Result<std::vector<ChunkPlan>>::success({std::move(chunk)});
        }

        for (std::size_t position = sequencePos; position < budget.degradeSequence.size();
             ++position) {
            const std::string& operation = budget.degradeSequence[position];
            if (operation == "halve_batch" && batch > 1) {
                auto reduced =
                    planRegion(rect, level, std::max<std::uint64_t>(1, batch / 2),
                               position);
                if (reduced.ok()) {
                    return reduced;
                }
            } else if (operation == "split_tile" && canSplit(rect)) {
                PixelRect a;
                PixelRect b;
                PixelRect c;
                PixelRect d;
                split(rect, a, b, c, d);
                std::vector<ChunkPlan> leaves;
                const PixelRect children[4] = {a, b, c, d};
                bool failed = false;
                for (const auto& child : children) {
                    if (child.area() == 0) {
                        continue;
                    }
                    auto childPlan = planRegion(child, level + 1, batch, 0);
                    if (!childPlan.ok()) {
                        return childPlan;
                    }
                    for (auto& leaf : *childPlan.value) {
                        leaves.push_back(std::move(leaf));
                    }
                }
                if (!failed) {
                    return Result<std::vector<ChunkPlan>>::success(std::move(leaves));
                }
            }
        }
        return Result<std::vector<ChunkPlan>>::failure(makeDiagnostic(
            ErrorCode::BudgetExceeded, glyph,
            "a chunk cannot meet per-task/global budget " + std::to_string(limit) +
                " bytes after the declared degradation sequence"));
    }

    Result<BudgetPlanResult> plan(int width, int height) {
        std::vector<ChunkPlan> chunks;
        for (int y = 0; y < height; y += raster.tileHeight) {
            for (int x = 0; x < width; x += raster.tileWidth) {
                PixelRect rect{x, y, std::min(raster.tileWidth, width - x),
                               std::min(raster.tileHeight, height - y)};
                auto leaves = planRegion(rect, 0, budget.sampleBatchSize, 0);
                if (!leaves.ok()) {
                    return Result<BudgetPlanResult>::failure(std::move(leaves.error));
                }
                for (auto& leaf : *leaves.value) {
                    chunks.push_back(std::move(leaf));
                }
            }
        }
        std::stable_sort(chunks.begin(), chunks.end(), [](const ChunkPlan& left,
                                                          const ChunkPlan& right) {
            if (left.rect.y != right.rect.y) {
                return left.rect.y < right.rect.y;
            }
            if (left.rect.x != right.rect.x) {
                return left.rect.x < right.rect.x;
            }
            if (left.rect.height != right.rect.height) {
                return left.rect.height < right.rect.height;
            }
            return left.rect.width < right.rect.width;
        });
        for (std::size_t i = 0; i < chunks.size(); ++i) {
            chunks[i].index = static_cast<int>(i);
        }
        BudgetPlanResult result;
        result.chunks = std::move(chunks);
        result.sampleBatchSize = budget.sampleBatchSize;
        return Result<BudgetPlanResult>::success(std::move(result));
    }
};

SnapshotConfig makeSnapshotConfig(const RasterRequest& request, const Font& font,
                                  const NormalizedGlyph& glyph, const GlyphPlacement& placement) {
    SnapshotConfig config;
    config.fontFingerprint = font.fingerprint;
    config.glyphFingerprint = normalizedGlyphFingerprint(glyph);
    config.glyph = request.glyph;
    config.targetPixelSize = request.raster.targetPixelSize;
    config.subpixelX = request.raster.subpixelX;
    config.subpixelY = request.raster.subpixelY;
    config.fillRule = request.raster.fillRule;
    config.quantizationStep = request.raster.quantizationStep;
    config.samplesPerAxis = request.raster.samplesPerAxis;
    config.tileWidth = request.raster.tileWidth;
    config.tileHeight = request.raster.tileHeight;
    config.bitmapWidth = placement.width;
    config.bitmapHeight = placement.height;
    config.seamTolerance = request.raster.seamTolerance;
    config.parallelism = request.budget.parallelism;
    config.globalBudgetBytes = request.budget.globalBudgetBytes;
    config.perTaskBudgetBytes = request.budget.perTaskBudgetBytes;
    config.sampleBatchSize = request.budget.sampleBatchSize;
    config.degradeSequence = request.budget.degradeSequence;
    return config;
}

void addTraceChunk(TraceLog* trace, const std::string& glyph, const ChunkPlan& chunk,
                   const BudgetOptions& budget, bool reused) {
    if (trace == nullptr) {
        return;
    }
    Json record = Json::object();
    record["event"] = Json("chunk");
    record["glyph"] = Json(glyph);
    record["chunk"] = Json(chunk.index);
    record["x"] = Json(chunk.rect.x);
    record["y"] = Json(chunk.rect.y);
    record["width"] = Json(chunk.rect.width);
    record["height"] = Json(chunk.rect.height);
    record["samples"] = Json(chunk.samples);
    record["batchSize"] = Json(chunk.batchSize);
    record["estimatedBytes"] = Json(chunk.estimatedBytes);
    record["reused"] = Json(reused);
    record["recomputed"] = Json(!reused);
    record["contentHash"] = Json(chunk.contentHash);
    Json usage = Json::object();
    usage["globalBudgetBytes"] = Json(budget.globalBudgetBytes);
    usage["perTaskBudgetBytes"] = Json(budget.perTaskBudgetBytes);
    record["budget"] = usage;
    trace->add(std::move(record));
}

int adjacentChunk(const std::vector<ChunkPlan>& chunks, int index, int pixelX, int pixelY) {
    const ChunkPlan& chunk = chunks[static_cast<std::size_t>(index)];
    const bool atRight = pixelX == chunk.rect.right() - 1;
    const bool atBottom = pixelY == chunk.rect.bottom() - 1;
    for (const auto& candidate : chunks) {
        if (candidate.index == index) {
            continue;
        }
        if (atRight && candidate.rect.x == chunk.rect.right() && pixelY >= candidate.rect.y &&
            pixelY < candidate.rect.bottom()) {
            return candidate.index;
        }
        if (atBottom && candidate.rect.y == chunk.rect.bottom() &&
            pixelX >= candidate.rect.x && pixelX < candidate.rect.right()) {
            return candidate.index;
        }
    }
    for (const auto& candidate : chunks) {
        if (candidate.index != index && pixelX >= candidate.rect.x &&
            pixelX < candidate.rect.right() && pixelY >= candidate.rect.y &&
            pixelY < candidate.rect.bottom()) {
            return candidate.index;
        }
    }
    return -1;
}

Diagnostic validateBudget(const BudgetOptions& budget, const std::string& glyph) {
    if (budget.parallelism <= 0 || budget.parallelism > 256) {
        return makeDiagnostic(ErrorCode::InvalidArgument, glyph,
                              "parallelism must be in [1, 256]");
    }
    if (budget.sampleBatchSize == 0) {
        return makeDiagnostic(ErrorCode::InvalidArgument, glyph,
                              "sampleBatchSize must be positive");
    }
    for (const auto& operation : budget.degradeSequence) {
        if (operation != "split_tile" && operation != "halve_batch") {
            return makeDiagnostic(ErrorCode::InvalidArgument, glyph,
                                  "unknown degrade operation '" + operation + "'");
        }
    }
    return {};
}

}  // namespace

FontEngine::FontEngine(Font font, TraceLog* trace)
    : font_(std::move(font)), trace_(trace) {}

bool FontEngine::isGlyphBusy(const std::string& glyph) const {
    std::lock_guard<std::mutex> lock(busyMutex_);
    return busyGlyphs_.count(glyph) != 0;
}

Result<RasterResult> FontEngine::rasterize(const RasterRequest& request,
                                           const CancelCheck& cancelled,
                                           const ProgressCallback& progress) {
    {
        std::lock_guard<std::mutex> lock(busyMutex_);
        if (busyGlyphs_.count(request.glyph) != 0) {
            return Result<RasterResult>::failure(
                makeDiagnostic(ErrorCode::Busy, request.glyph,
                               "glyph rasterization is already running"));
        }
        busyGlyphs_.insert(request.glyph);
    }
    struct BusyGuard {
        FontEngine* engine;
        std::string glyph;
        ~BusyGuard() {
            std::lock_guard<std::mutex> lock(engine->busyMutex_);
            engine->busyGlyphs_.erase(glyph);
        }
    } busyGuard{this, request.glyph};

    const NormalizedGlyph* glyph = font_.findGlyph(request.glyph);
    if (glyph == nullptr) {
        return Result<RasterResult>::failure(makeDiagnostic(
            ErrorCode::FontInvalid, request.glyph, "unknown glyph '" + request.glyph + "'"));
    }
    auto rasterValidation = validateRasterOptions(request.raster);
    if (!rasterValidation.ok()) {
        Diagnostic diagnostic = std::move(rasterValidation.error);
        diagnostic.glyph = request.glyph;
        return Result<RasterResult>::failure(std::move(diagnostic));
    }
    Diagnostic budgetError = validateBudget(request.budget, request.glyph);
    if (!budgetError.ok()) {
        return Result<RasterResult>::failure(std::move(budgetError));
    }
    if (request.resume && !request.persist) {
        return Result<RasterResult>::failure(makeDiagnostic(
            ErrorCode::InvalidArgument, request.glyph, "resume requires persistent state"));
    }

    auto prepared = prepareGlyph(font_, *glyph);
    if (!prepared.ok()) {
        return Result<RasterResult>::failure(std::move(prepared.error));
    }
    auto placement = placeGlyph(font_, *prepared.value, request.raster);
    if (!placement.ok()) {
        return Result<RasterResult>::failure(std::move(placement.error));
    }

    const std::uint64_t limit =
        std::min(request.budget.perTaskBudgetBytes, request.budget.globalBudgetBytes);
    Planner planner{request.raster, request.budget, limit, cancelled, request.glyph};
    auto planned = planner.plan(placement.value->width, placement.value->height);
    if (!planned.ok()) {
        if (trace_ != nullptr) {
            Json record = Json::object();
            record["event"] = Json("error");
            record["glyph"] = Json(request.glyph);
            record["code"] = Json(errorCodeName(planned.error.code));
            record["message"] = Json(planned.error.message);
            trace_->add(std::move(record));
        }
        return Result<RasterResult>::failure(std::move(planned.error));
    }
    std::vector<ChunkPlan> chunks = std::move(planned.value->chunks);
    const SnapshotConfig config =
        makeSnapshotConfig(request, font_, *glyph, *placement.value);
    const std::string configHash = snapshotConfigHash(config);

    const std::filesystem::path stateDir =
        request.stateDirectory.empty() ? std::filesystem::path()
                                       : std::filesystem::path(request.stateDirectory);
    std::vector<std::uint8_t> assembled(
        static_cast<std::size_t>(placement.value->width * placement.value->height), 0);
    int startIndex = 0;
    std::uint64_t reusedChunks = 0;
    if (request.persist && request.resume) {
        auto loaded = loadSnapshot(stateDir, configHash);
        if (!loaded.ok()) {
            return Result<RasterResult>::failure(std::move(loaded.error));
        }
        if (loaded.value->has_value()) {
            SnapshotState& snapshot = **loaded.value;
            bool planMatches = snapshot.chunks.size() == chunks.size();
            if (planMatches) {
                for (std::size_t i = 0; i < chunks.size(); ++i) {
                    const ChunkPlan& stored = snapshot.chunks[i];
                    const ChunkPlan& current = chunks[i];
                    if (stored.rect.x != current.rect.x ||
                        stored.rect.y != current.rect.y ||
                        stored.rect.width != current.rect.width ||
                        stored.rect.height != current.rect.height ||
                        stored.samples != current.samples ||
                        stored.batchSize != current.batchSize) {
                        planMatches = false;
                        break;
                    }
                }
            }
            if (!planMatches) {
                return Result<RasterResult>::failure(makeDiagnostic(
                    ErrorCode::SnapshotMismatch, request.glyph,
                    "snapshot chunk plan does not match deterministic planner output"));
            }
            for (int i = 0; i < snapshot.committedChunks; ++i) {
                auto bytes = loadChunk(stateDir, i);
                if (!bytes.ok()) {
                    return Result<RasterResult>::failure(makeDiagnostic(
                        ErrorCode::SnapshotMismatch, request.glyph,
                        "snapshot committed chunk cannot be loaded"));
                }
                chunks[static_cast<std::size_t>(i)].completed = true;
                chunks[static_cast<std::size_t>(i)].reused = true;
                chunks[static_cast<std::size_t>(i)].contentHash =
                    snapshot.chunks[static_cast<std::size_t>(i)].contentHash;
                const PixelRect& rect = chunks[static_cast<std::size_t>(i)].rect;
                for (int row = 0; row < rect.height; ++row) {
                    std::copy(bytes.value->begin() + static_cast<std::ptrdiff_t>(row * rect.width),
                              bytes.value->begin() +
                                  static_cast<std::ptrdiff_t>((row + 1) * rect.width),
                              assembled.begin() +
                                  static_cast<std::ptrdiff_t>((rect.y + row) *
                                                                  placement.value->width +
                                                              rect.x));
                }
                addTraceChunk(trace_, request.glyph,
                              chunks[static_cast<std::size_t>(i)], request.budget, true);
                ++reusedChunks;
            }
            startIndex = snapshot.committedChunks;
        }
    } else if (request.persist) {
        std::error_code removeError;
        std::filesystem::remove(stateDir / "snapshot.json", removeError);
    }

    struct SharedState {
        std::mutex mutex;
        std::condition_variable condition;
        std::deque<int> queue;
        std::map<int, Result<RawRegion>> results;
        std::vector<bool> hasResult;
        std::atomic<bool> stop{false};
        std::atomic<std::uint64_t> activeBytes{0};
        std::atomic<std::uint64_t> peakBytes{0};
        std::uint64_t retainedBytes = 0;
        Diagnostic firstError;
        bool hasError = false;
    } shared;
    shared.hasResult.assign(chunks.size(), false);
    for (int i = startIndex; i < static_cast<int>(chunks.size()); ++i) {
        shared.queue.push_back(i);
    }

    std::mutex progressMutex;
    const auto emitProgress = [&](const ProgressEvent& event) {
        if (progress) {
            std::lock_guard<std::mutex> lock(progressMutex);
            progress(event);
        }
    };
    emitProgress({ProgressKind::Planned, request.glyph, -1, startIndex,
                  static_cast<int>(chunks.size()), 0, 0});

    const int parallelism = request.budget.parallelism;
    std::vector<std::thread> workers;
    workers.reserve(static_cast<std::size_t>(parallelism));
    for (int workerIndex = 0; workerIndex < parallelism; ++workerIndex) {
        workers.emplace_back([&]() {
            while (true) {
                int task = -1;
                std::uint64_t estimate = 0;
                {
                    std::unique_lock<std::mutex> lock(shared.mutex);
                    shared.condition.wait(lock, [&]() {
                        if (shared.stop.load()) {
                            return true;
                        }
                        if (shared.queue.empty()) {
                            return shared.activeBytes == 0;
                        }
                        const int candidate = shared.queue.front();
                        estimate = chunks[static_cast<std::size_t>(candidate)].estimatedBytes;
                        return shared.activeBytes + shared.retainedBytes + estimate <=
                                   request.budget.globalBudgetBytes ||
                               (shared.activeBytes == 0 && shared.retainedBytes == 0);
                    });
                    if (shared.stop.load()) {
                        return;
                    }
                    if (shared.queue.empty()) {
                        return;
                    }
                    task = shared.queue.front();
                    shared.queue.pop_front();
                    estimate = chunks[static_cast<std::size_t>(task)].estimatedBytes;
                    shared.activeBytes += estimate;
                    shared.peakBytes.store(
                        std::max(shared.peakBytes.load(),
                                 shared.activeBytes.load() + shared.retainedBytes));
                }

                const ChunkPlan& chunk = chunks[static_cast<std::size_t>(task)];
                emitProgress({ProgressKind::ChunkStarted, request.glyph, task, startIndex,
                              static_cast<int>(chunks.size()), shared.activeBytes.load(),
                              shared.peakBytes.load()});
                auto region = rasterRegion(
                    *prepared.value, *placement.value, request.raster, chunk.rect,
                    [&]() { return shared.stop.load() || (cancelled && cancelled()); });
                {
                    std::lock_guard<std::mutex> lock(shared.mutex);
                    shared.activeBytes -= estimate;
                    if (region.ok()) {
                        shared.retainedBytes += static_cast<std::uint64_t>(chunk.rect.area());
                    } else {
                        shared.stop.store(true);
                        if (!shared.hasError) {
                            shared.hasError = true;
                            shared.firstError = region.error;
                        }
                    }
                    shared.results.emplace(task, std::move(region));
                    shared.hasResult[static_cast<std::size_t>(task)] = true;
                    shared.condition.notify_all();
                }
                if (shared.stop.load()) {
                    return;
                }
            }
        });
    }

    Diagnostic failure;
    bool failed = false;
    int next = startIndex;
    std::uint64_t newlyCommitted = 0;
    SnapshotState snapshot;
    if (request.persist) {
        snapshot.config = config;
        snapshot.configHash = configHash;
        snapshot.chunks = chunks;
    }
    while (next < static_cast<int>(chunks.size())) {
        Result<RawRegion> region;
        {
            std::unique_lock<std::mutex> lock(shared.mutex);
            shared.condition.wait(lock, [&]() {
                return shared.hasResult[static_cast<std::size_t>(next)] ||
                       shared.stop.load();
            });
            if (shared.hasResult[static_cast<std::size_t>(next)]) {
                region = std::move(shared.results[next]);
                shared.results.erase(next);
                const ChunkPlan& chunk = chunks[static_cast<std::size_t>(next)];
                shared.retainedBytes -= static_cast<std::uint64_t>(chunk.rect.area());
            } else {
                failed = true;
                if (shared.hasError) {
                    failure = shared.firstError;
                } else {
                    failure = makeDiagnostic(ErrorCode::Cancelled, request.glyph,
                                             "rasterization cancelled at a chunk checkpoint");
                }
                shared.condition.notify_all();
                break;
            }
        }
        if (!region.ok()) {
            failed = true;
            failure = region.error;
            shared.stop.store(true);
            shared.condition.notify_all();
            break;
        }
        ChunkPlan& chunk = chunks[static_cast<std::size_t>(next)];
        const PixelRect& rect = chunk.rect;
        for (int row = 0; row < rect.height; ++row) {
            std::copy(region.value->coverage.begin() +
                          static_cast<std::ptrdiff_t>(row * rect.width),
                      region.value->coverage.begin() +
                          static_cast<std::ptrdiff_t>((row + 1) * rect.width),
                      assembled.begin() +
                          static_cast<std::ptrdiff_t>((rect.y + row) *
                                                          placement.value->width +
                                                      rect.x));
        }
        chunk.completed = true;
        chunk.contentHash = hashCoverage(region.value->coverage);
        if (request.persist) {
            auto savedChunk = saveChunk(stateDir, next, region.value->coverage);
            if (!savedChunk.ok()) {
                failed = true;
                failure = std::move(savedChunk.error);
                shared.stop.store(true);
                shared.condition.notify_all();
                break;
            }
            snapshot.chunks = chunks;
            auto savedSnapshot = saveSnapshot(stateDir, snapshot);
            if (!savedSnapshot.ok()) {
                failed = true;
                failure = std::move(savedSnapshot.error);
                shared.stop.store(true);
                shared.condition.notify_all();
                break;
            }
        }
        addTraceChunk(trace_, request.glyph, chunk, request.budget, false);
        ++newlyCommitted;
        emitProgress({ProgressKind::ChunkCompleted, request.glyph, next, next + 1,
                      static_cast<int>(chunks.size()), shared.activeBytes.load(),
                      shared.peakBytes.load()});
        ++next;
        {
            std::lock_guard<std::mutex> lock(shared.mutex);
            shared.condition.notify_all();
        }
        if (request.hardExitAfterCommittedChunks > 0 &&
            newlyCommitted >= request.hardExitAfterCommittedChunks && request.persist) {
            std::_Exit(77);
        }
    }
    shared.stop.store(true);
    shared.condition.notify_all();
    for (auto& worker : workers) {
        if (worker.joinable()) {
            worker.join();
        }
    }

    if (failed) {
        if (trace_ != nullptr) {
            Json record = Json::object();
            record["event"] = Json("error");
            record["glyph"] = Json(request.glyph);
            record["code"] = Json(errorCodeName(failure.code));
            record["message"] = Json(failure.message);
            if (failure.chunkIndex >= 0) {
                record["chunk"] = Json(failure.chunkIndex);
            }
            trace_->add(std::move(record));
        }
        return Result<RasterResult>::failure(std::move(failure));
    }

    auto truth = rasterRegion(*prepared.value, *placement.value, request.raster,
                              {0, 0, placement.value->width, placement.value->height},
                              cancelled);
    if (!truth.ok()) {
        return Result<RasterResult>::failure(std::move(truth.error));
    }
    for (int y = 0; y < placement.value->height; ++y) {
        for (int x = 0; x < placement.value->width; ++x) {
            const std::size_t offset =
                static_cast<std::size_t>(y) * placement.value->width + x;
            const int difference = std::abs(static_cast<int>(assembled[offset]) -
                                            static_cast<int>(truth.value->coverage[offset]));
            if (difference > request.raster.seamTolerance) {
                int chunkIndex = -1;
                for (const auto& chunk : chunks) {
                    if (x >= chunk.rect.x && x < chunk.rect.right() && y >= chunk.rect.y &&
                        y < chunk.rect.bottom()) {
                        chunkIndex = chunk.index;
                        break;
                    }
                }
                Diagnostic seam = makeDiagnostic(
                    ErrorCode::SeamUnresolved, request.glyph,
                    "tiled coverage differs from whole-glyph truth beyond tolerance");
                seam.chunkIndex = chunkIndex;
                seam.pairedChunkIndex = adjacentChunk(chunks, chunkIndex, x, y);
                seam.pixelX = x;
                seam.pixelY = y;
                seam.expected = truth.value->coverage[offset];
                seam.actual = assembled[offset];
                if (trace_ != nullptr) {
                    Json record = Json::object();
                    record["event"] = Json("error");
                    record["glyph"] = Json(request.glyph);
                    record["code"] = Json("E_SEAM_UNRESOLVED");
                    record["chunk"] = Json(seam.chunkIndex);
                    record["pairedChunk"] = Json(seam.pairedChunkIndex);
                    record["pixelX"] = Json(seam.pixelX);
                    record["pixelY"] = Json(seam.pixelY);
                    trace_->add(std::move(record));
                }
                return Result<RasterResult>::failure(std::move(seam));
            }
        }
    }

    RasterResult result;
    result.glyph = request.glyph;
    result.width = placement.value->width;
    result.height = placement.value->height;
    result.coverage = std::move(assembled);
    result.chunks = chunks;
    result.reusedChunks = reusedChunks;
    result.recomputedChunks = newlyCommitted;
    result.totalSamples = 0;
    std::vector<std::uint64_t> estimatedBytes;
    for (const auto& chunk : chunks) {
        if (!chunk.reused) {
            result.totalSamples += chunk.samples;
            estimatedBytes.push_back(chunk.estimatedBytes);
        }
    }
    std::sort(estimatedBytes.begin(), estimatedBytes.end(), std::greater<>());
    std::uint64_t plannedPeak = 0;
    const std::size_t concurrent =
        std::min<std::size_t>(estimatedBytes.size(),
                              static_cast<std::size_t>(request.budget.parallelism));
    for (std::size_t i = 0; i < concurrent; ++i) {
        plannedPeak += estimatedBytes[i];
    }
    result.peakMemoryBytes =
        std::min(plannedPeak, request.budget.globalBudgetBytes);
    result.coverageHash = hashCoverage(result.coverage);
    result.metrics.glyph = request.glyph;
    result.metrics.pixelWidth = result.width;
    result.metrics.pixelHeight = result.height;
    result.metrics.sourceBounds = glyph->bounds;
    result.metrics.bearingX = static_cast<int>(
        std::lround(placement.value->originDeviceX + glyph->bounds.x0 * placement.value->scale));
    result.metrics.bearingY = static_cast<int>(
        std::lround(placement.value->originDeviceY + glyph->bounds.y1 * placement.value->scale));
    result.metrics.advance =
        static_cast<int>(std::lround(glyph->advance * placement.value->scale));

    if (trace_ != nullptr) {
        Json summary = Json::object();
        summary["event"] = Json("glyph");
        summary["glyph"] = Json(request.glyph);
        summary["chunks"] = Json(static_cast<std::uint64_t>(chunks.size()));
        summary["reusedChunks"] = Json(reusedChunks);
        summary["recomputedChunks"] = Json(newlyCommitted);
        summary["reused"] = Json(reusedChunks);
        summary["recomputed"] = Json(newlyCommitted);
        summary["samples"] = Json(result.totalSamples);
        summary["peakMemoryBytes"] = Json(result.peakMemoryBytes);
        summary["contentHash"] = Json(result.coverageHash);
        trace_->add(std::move(summary));
    }
    emitProgress({ProgressKind::GlyphComposed, request.glyph, -1,
                  static_cast<int>(chunks.size()), static_cast<int>(chunks.size()),
                  shared.activeBytes.load(), shared.peakBytes.load()});
    return Result<RasterResult>::success(std::move(result));
}

Result<QuantErrorSummary> FontEngine::compareQuantization(const std::string& glyphName,
                                                          int oldStep, int newStep,
                                                          RasterOptions options,
                                                          const CancelCheck& cancelled) {
    if (oldStep <= 0 || oldStep > 255 || newStep <= 0 || newStep > 255) {
        return Result<QuantErrorSummary>::failure(makeDiagnostic(
            ErrorCode::QuantInvalid, glyphName,
            "quantization steps must be in [1, 255] for an 8-bit coverage buffer"));
    }
    const NormalizedGlyph* glyph = font_.findGlyph(glyphName);
    if (glyph == nullptr) {
        return Result<QuantErrorSummary>::failure(makeDiagnostic(
            ErrorCode::FontInvalid, glyphName, "unknown glyph '" + glyphName + "'"));
    }
    auto prepared = prepareGlyph(font_, *glyph);
    if (!prepared.ok()) {
        return Result<QuantErrorSummary>::failure(std::move(prepared.error));
    }
    auto placement = placeGlyph(font_, *prepared.value, options);
    if (!placement.ok()) {
        return Result<QuantErrorSummary>::failure(std::move(placement.error));
    }
    RasterOptions oldOptions = options;
    oldOptions.quantizationStep = oldStep;
    RasterOptions newOptions = options;
    newOptions.quantizationStep = newStep;
    const PixelRect whole{0, 0, placement.value->width, placement.value->height};
    auto oldCoverage =
        rasterRegion(*prepared.value, *placement.value, oldOptions, whole, cancelled);
    if (!oldCoverage.ok()) {
        return Result<QuantErrorSummary>::failure(std::move(oldCoverage.error));
    }
    auto newCoverage =
        rasterRegion(*prepared.value, *placement.value, newOptions, whole, cancelled);
    if (!newCoverage.ok()) {
        return Result<QuantErrorSummary>::failure(std::move(newCoverage.error));
    }
    QuantErrorSummary summary;
    summary.oldStep = oldStep;
    summary.newStep = newStep;
    summary.glyph = glyphName;
    summary.comparedPixels = oldCoverage.value->coverage.size();
    int maxError = 0;
    for (std::size_t i = 0; i < oldCoverage.value->coverage.size(); ++i) {
        const int difference =
            std::abs(static_cast<int>(oldCoverage.value->coverage[i]) -
                     static_cast<int>(newCoverage.value->coverage[i]));
        summary.totalAbsoluteError += static_cast<std::uint64_t>(difference);
        maxError = std::max(maxError, difference);
    }
    summary.glyphMaxAbsoluteError = maxError;
    if (summary.comparedPixels > 0) {
        summary.meanAbsoluteError =
            static_cast<double>(summary.totalAbsoluteError) /
            static_cast<double>(summary.comparedPixels);
    }
    return Result<QuantErrorSummary>::success(std::move(summary));
}

}  // namespace font_atlas
