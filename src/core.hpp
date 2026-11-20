#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <map>
#include <optional>
#include <string>
#include <utility>
#include <vector>

namespace fill {

struct Vec2 {
    double x = 0.0;
    double y = 0.0;
};

Vec2 operator+(Vec2 a, Vec2 b);
Vec2 operator-(Vec2 a, Vec2 b);
Vec2 operator*(Vec2 a, double s);
Vec2 operator/(Vec2 a, double s);
double dot(Vec2 a, Vec2 b);
double cross(Vec2 a, Vec2 b);
double length(Vec2 v);
double distance(Vec2 a, Vec2 b);
bool finite(Vec2 v);

struct Bounds {
    double minX = 0.0;
    double minY = 0.0;
    double maxX = 0.0;
    double maxY = 0.0;
    bool valid = false;

    void include(Vec2 p);
    double width() const;
    double height() const;
};

enum class SegmentType { Line, Quadratic, Cubic };
enum class Direction { Clockwise = -1, Degenerate = 0, CounterClockwise = 1 };
enum class FillRule { NonZero, EvenOdd };

struct Segment {
    SegmentType type = SegmentType::Line;
    Vec2 p0;
    Vec2 c1;
    Vec2 c2;
    Vec2 p1;
    int sourceIndex = -1;
};

struct Contour {
    std::vector<Segment> segments;
    std::vector<Vec2> vertices;
    Direction declaredDirection = Direction::Degenerate;
    Direction actualDirection = Direction::Degenerate;
    bool roleHole = false;
    bool hasRole = false;
    double signedArea = 0.0;
    Bounds bounds;
    int sourceIndex = -1;
};

struct ToleranceBudget {
    double vertexMerge = 0.001;
    double intersectionClassify = 0.001;
    double sampleDistance = 0.01;
    double totalLimit = 2.0;
};

struct Path {
    FillRule fillRule = FillRule::NonZero;
    std::vector<Contour> contours;
    Bounds bounds;
    ToleranceBudget budget;
    std::string sourceName;
};

struct Error {
    std::string code;
    int contour = -1;
    int segment = -1;
    std::string detail;
    bool injected = false;
    std::string injectionClass;
    std::vector<int> relatedContours;

    bool ok() const { return code.empty(); }
};

struct ParseResult {
    std::optional<Path> path;
    Error error;
};

struct ParseOptions {
    ToleranceBudget budget;
    std::string injection;
    bool overrideBudget = false;
};

struct Intersection {
    Vec2 point;
    int contourA = -1;
    int segmentA = -1;
    int contourB = -1;
    int segmentB = -1;
    std::string kind;
};

struct NearCoincidentPair {
    int contourA = -1;
    int segmentA = -1;
    int contourB = -1;
    int segmentB = -1;
    std::string overlapDirection;
    std::string confidence;
    double maxEndpointDistance = 0.0;
};

struct DegenerateSegment {
    int contour = -1;
    int segment = -1;
    std::string reason;
    double measure = 0.0;
};

struct IsolatedSegment {
    int contour = -1;
    int segment = -1;
    std::string reason;
};

struct Diagnostics {
    int monotonicSegmentCount = 0;
    std::vector<Intersection> intersections;
    std::vector<NearCoincidentPair> nearCoincident;
    std::vector<DegenerateSegment> degenerate;
    std::vector<IsolatedSegment> isolated;
};

struct FlatEdge {
    Vec2 a;
    Vec2 b;
    int contour = -1;
    int segment = -1;
};

struct FlatPath {
    std::vector<FlatEdge> edges;
    std::vector<std::vector<Vec2>> contourVertices;
};

struct RenderOptions {
    int width = 64;
    int height = 64;
    double degradeRatio = 0.05;
    int blockSize = 16;
};

struct PixelDecision {
    int windingLower = 0;
    int windingUpper = 0;
    int crossingsLower = 0;
    int crossingsUpper = 0;
    bool nonzeroDefinite = true;
    bool nonzeroFilled = false;
    bool evenOddDefinite = true;
    bool evenOddFilled = false;
    bool ruleSensitive = false;
    bool uncertain = false;
    double nearestBoundaryWorld = 0.0;
    double nearestBoundaryPixel = 0.0;
    std::vector<std::pair<int, int>> ambiguousSegments;
};

struct ViewTransform {
    double scale = 1.0;
    double offsetX = 0.0;
    double offsetY = 0.0;
    double worldPerPixel = 1.0;
};

struct RenderMetrics {
    int totalPixels = 0;
    int nonzeroCoveredPixels = 0;
    int evenOddCoveredPixels = 0;
    int ruleSensitivePixels = 0;
    int uncertainPixels = 0;
    int ambiguousEdgeReports = 0;
    double nonzeroCoverageRatio = 0.0;
    double evenOddCoverageRatio = 0.0;
    double coverageAreaDeviation = 0.0;
    bool degraded = false;
    bool committable = false;
};

struct RenderResult {
    Error error;
    bool cancelled = false;
    Path path;
    ViewTransform transform;
    Bounds screenBounds;
    std::vector<PixelDecision> pixels;
    std::array<std::vector<std::uint8_t>, 2> coverage;
    std::vector<std::uint8_t> diffMap;
    std::vector<std::uint8_t> fillMap;
    std::vector<std::uint8_t> sourceMap;
    RenderMetrics metrics;
    Diagnostics diagnostics;
    std::vector<std::pair<int, int>> ambiguousSegments;
    std::string noticeCode;
};

struct BudgetImpact {
    std::string item;
    double originalValue = 0.0;
    double perturbedValue = 0.0;
    int uncertainPixelDelta = 0;
    int perturbedUncertainPixels = 0;
    double perturbedCoverageRatio = 0.0;
    double signedCoverageAreaDelta = 0.0;
    double coverageAreaDeviation = 0.0;
};

struct BoundDiff {
    ToleranceBudget oldBudget;
    ToleranceBudget newBudget;
    int changedPixels = 0;
    long long lowerBoundDeltaSum = 0;
    long long upperBoundDeltaSum = 0;
    int uncertainPixelDelta = 0;
    std::map<int, int> lowerBoundHistogram;
};

struct BudgetAnalysis {
    std::vector<BudgetImpact> impacts;
    BoundDiff toleranceComparison;
    std::vector<std::string> tighteningOrder;
};

ParseResult parsePathJson(const std::string& json, const ParseOptions& options,
                          const std::string& sourceName = "<memory>");
ParseResult parsePathFile(const std::string& filename, const ParseOptions& options);
bool validateToleranceBudget(const ToleranceBudget& budget, std::string& detail);

FlatPath flattenPath(const Path& path, double tolerance);
Diagnostics diagnosePath(const Path& path, const FlatPath& flat, double tolerance);
double contourSignedAreaWorld(const Contour& contour);
bool pointInPolygon(Vec2 point, const std::vector<Vec2>& polygon);

ViewTransform makeViewTransform(const Path& path, int width, int height);
RenderResult renderPath(const Path& path, const RenderOptions& options,
                        const std::function<bool()>& isCancelled = {});

BudgetAnalysis analyzeBudget(const Path& path, const RenderOptions& options,
                             const ToleranceBudget& previousBudget);

std::string sha256Hex(const std::vector<std::uint8_t>& bytes);
std::string sha256Hex(const std::string& bytes);
std::string formatDouble(double value);
std::string errorToJson(const Error& error);

struct PipelineOptions {
    std::string inputFile;
    std::string outputDir;
    std::string injection;
    int width = 64;
    int height = 64;
    double degradeRatio = 0.05;
    ToleranceBudget budget;
    bool budgetProvided = false;
    bool comparePrevious = false;
    ToleranceBudget previousBudget;
    int cancelAfterBlocks = -1;
};

int runRenderPipeline(const PipelineOptions& options);
int runSubmit(const std::string& resultFile);
int runGenerateSamples(const std::string& outputDir, int width, int height);

}  // namespace fill
