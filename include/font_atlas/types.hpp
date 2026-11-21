#pragma once

#include <cstdint>
#include <optional>
#include <string>
#include <utility>
#include <vector>

namespace font_atlas {

enum class ErrorCode {
    Ok = 0,
    FontInvalid,
    QuantInvalid,
    Busy,
    BudgetExceeded,
    SeamUnresolved,
    AtlasOverflow,
    SnapshotMismatch,
    Cancelled,
    IoError,
    InvalidArgument,
    Internal
};

enum class FillRule {
    NonZero,
    EvenOdd
};

struct Diagnostic {
    ErrorCode code = ErrorCode::Ok;
    std::string message;
    std::string glyph;
    int contourIndex = -1;
    int segmentIndex = -1;
    int chunkIndex = -1;
    int pairedChunkIndex = -1;
    int pixelX = -1;
    int pixelY = -1;
    int expected = -1;
    int actual = -1;

    bool ok() const { return code == ErrorCode::Ok; }
};

template <typename T>
struct Result {
    std::optional<T> value;
    Diagnostic error;

    static Result success(T item) {
        Result result;
        result.value = std::move(item);
        return result;
    }

    static Result failure(Diagnostic diagnostic) {
        Result result;
        result.error = std::move(diagnostic);
        return result;
    }

    bool ok() const { return value.has_value() && error.ok(); }
};

template <>
struct Result<void> {
    Diagnostic error;

    static Result success() {
        Result result;
        return result;
    }

    static Result failure(Diagnostic diagnostic) {
        Result result;
        result.error = std::move(diagnostic);
        return result;
    }

    bool ok() const { return error.ok(); }
};

const char* errorCodeName(ErrorCode code);
const char* fillRuleName(FillRule rule);
bool parseFillRule(const std::string& text, FillRule& rule);

struct PointD {
    double x = 0.0;
    double y = 0.0;
};

struct BoundsD {
    double x0 = 0.0;
    double y0 = 0.0;
    double x1 = 0.0;
    double y1 = 0.0;

    double width() const { return x1 - x0; }
    double height() const { return y1 - y0; }
};

struct PixelRect {
    int x = 0;
    int y = 0;
    int width = 0;
    int height = 0;

    int right() const { return x + width; }
    int bottom() const { return y + height; }
    int area() const { return width * height; }
};

struct RasterOptions {
    FillRule fillRule = FillRule::NonZero;
    int quantizationStep = 1;
    int samplesPerAxis = 4;
    int targetPixelSize = 32;
    double subpixelX = 0.0;
    double subpixelY = 0.0;
    int tileWidth = 16;
    int tileHeight = 16;
    int seamTolerance = 0;
};

struct BudgetOptions {
    int parallelism = 4;
    std::uint64_t globalBudgetBytes = 64ull * 1024ull * 1024ull;
    std::uint64_t perTaskBudgetBytes = 4ull * 1024ull * 1024ull;
    std::uint64_t sampleBatchSize = 4096;
    std::vector<std::string> degradeSequence{"split_tile", "halve_batch"};
};

struct AtlasOptions {
    int width = 512;
    int height = 512;
    int padding = 1;
};

struct WorkbenchConfig {
    RasterOptions raster;
    BudgetOptions budget;
    AtlasOptions atlas;
};

struct ChunkPlan {
    int index = 0;
    int parentIndex = -1;
    int splitLevel = 0;
    PixelRect rect;
    std::uint64_t estimatedBytes = 0;
    std::uint64_t samples = 0;
    std::uint64_t batchSize = 0;
    std::string contentHash;
    bool completed = false;
    bool reused = false;
};

struct GlyphMetrics {
    std::string glyph;
    int pixelWidth = 0;
    int pixelHeight = 0;
    int atlasX = 0;
    int atlasY = 0;
    int bearingX = 0;
    int bearingY = 0;
    int advance = 0;
    BoundsD sourceBounds;
};

struct RasterResult {
    std::string glyph;
    int width = 0;
    int height = 0;
    std::vector<std::uint8_t> coverage;
    std::vector<ChunkPlan> chunks;
    GlyphMetrics metrics;
    std::uint64_t reusedChunks = 0;
    std::uint64_t recomputedChunks = 0;
    std::uint64_t totalSamples = 0;
    std::uint64_t peakMemoryBytes = 0;
    std::string coverageHash;
};

struct QuantErrorSummary {
    int oldStep = 0;
    int newStep = 0;
    std::uint64_t totalAbsoluteError = 0;
    std::uint64_t comparedPixels = 0;
    double meanAbsoluteError = 0.0;
    int glyphMaxAbsoluteError = 0;
    std::string glyph;
};

struct AtlasManifest {
    int width = 0;
    int height = 0;
    int padding = 0;
    std::string imageFile;
    std::string imageHash;
    std::vector<GlyphMetrics> glyphs;
};

struct RasterRequest {
    std::string glyph;
    RasterOptions raster;
    BudgetOptions budget;
    std::string stateDirectory;
    bool resume = false;
    bool persist = false;
    std::uint64_t hardExitAfterCommittedChunks = 0;
};

enum class ProgressKind {
    Planned,
    ChunkStarted,
    ChunkCompleted,
    ChunkReused,
    GlyphComposed,
    Cancelled
};

struct ProgressEvent {
    ProgressKind kind = ProgressKind::Planned;
    std::string glyph;
    int chunkIndex = -1;
    int completed = 0;
    int total = 0;
    std::uint64_t activeBytes = 0;
    std::uint64_t peakBytes = 0;
};

}  // namespace font_atlas

