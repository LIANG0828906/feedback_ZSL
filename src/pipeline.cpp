#include "core.hpp"
#include "json.hpp"

#include <algorithm>
#include <array>
#include <cmath>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <map>
#include <sstream>
#include <stdexcept>

namespace fill {
namespace {

namespace fs = std::filesystem;

std::string num(double value) { return formatDouble(value); }

std::string directionName(Direction direction) {
    switch (direction) {
        case Direction::Clockwise:
            return "cw";
        case Direction::CounterClockwise:
            return "ccw";
        default:
            return "degenerate";
    }
}

std::string fillRuleName(FillRule rule) {
    return rule == FillRule::NonZero ? "nonzero" : "evenodd";
}

std::string segmentTypeName(SegmentType type) {
    switch (type) {
        case SegmentType::Line:
            return "line";
        case SegmentType::Quadratic:
            return "quadratic";
        case SegmentType::Cubic:
            return "cubic";
    }
    return "line";
}

std::string pointJson(Vec2 point) {
    return "[" + num(point.x) + "," + num(point.y) + "]";
}

std::string boundsJson(const Bounds& bounds) {
    std::ostringstream out;
    out << "{\"minX\":" << num(bounds.minX)
        << ",\"minY\":" << num(bounds.minY)
        << ",\"maxX\":" << num(bounds.maxX)
        << ",\"maxY\":" << num(bounds.maxY) << "}";
    return out.str();
}

std::string budgetJson(const ToleranceBudget& budget) {
    std::ostringstream out;
    out << "{\"vertexMerge\":" << num(budget.vertexMerge)
        << ",\"intersectionClassify\":" << num(budget.intersectionClassify)
        << ",\"sampleDistance\":" << num(budget.sampleDistance)
        << ",\"totalLimit\":" << num(budget.totalLimit) << "}";
    return out.str();
}

std::string pgm(int width, int height, const std::vector<std::uint8_t>& data) {
    std::ostringstream out;
    out << "P5\n" << width << " " << height << "\n255\n";
    std::string bytes = out.str();
    bytes.append(reinterpret_cast<const char*>(data.data()), data.size());
    return bytes;
}

std::string ppm(int width, int height, const std::vector<std::uint8_t>& rgbaLike) {
    std::ostringstream out;
    out << "P6\n" << width << " " << height << "\n255\n";
    std::string bytes = out.str();
    bytes.append(reinterpret_cast<const char*>(rgbaLike.data()), rgbaLike.size());
    return bytes;
}

std::string diffPpm(int width, int height,
                    const std::vector<std::uint8_t>& diff) {
    std::vector<std::uint8_t> rgb;
    rgb.reserve(diff.size() * 3U);
    for (const std::uint8_t value : diff) {
        if (value == 128U) {
            rgb.push_back(224);
            rgb.push_back(176);
            rgb.push_back(32);
        } else if (value == 255U) {
            rgb.push_back(220);
            rgb.push_back(48);
            rgb.push_back(48);
        } else {
            rgb.push_back(24);
            rgb.push_back(24);
            rgb.push_back(24);
        }
    }
    return ppm(width, height, rgb);
}

std::string fillPpm(int width, int height,
                    const std::vector<std::uint8_t>& fill,
                    const std::vector<std::uint8_t>& source) {
    std::vector<std::uint8_t> rgb;
    rgb.reserve(fill.size() * 3U);
    for (std::size_t i = 0; i < fill.size(); ++i) {
        if (source[i] == 170U) {
            rgb.push_back(128);
            rgb.push_back(128);
            rgb.push_back(128);
        } else if (fill[i] == 255U) {
            rgb.push_back(24);
            rgb.push_back(96);
            rgb.push_back(196);
        } else {
            rgb.push_back(248);
            rgb.push_back(248);
            rgb.push_back(248);
        }
    }
    return ppm(width, height, rgb);
}

bool writeFile(const fs::path& path, const std::string& bytes) {
    std::ofstream output(path, std::ios::binary | std::ios::trunc);
    if (!output) {
        return false;
    }
    output.write(bytes.data(), static_cast<std::streamsize>(bytes.size()));
    return static_cast<bool>(output);
}

bool writeAtomic(const fs::path& path, const std::string& bytes) {
    const fs::path temporary = path.string() + ".tmp";
    if (!writeFile(temporary, bytes)) {
        return false;
    }
    std::error_code ec;
    fs::rename(temporary, path, ec);
    if (ec) {
        fs::remove(path, ec);
        ec.clear();
        fs::rename(temporary, path, ec);
    }
    if (ec) {
        fs::remove(temporary, ec);
        return false;
    }
    return true;
}

std::string normalizedPathJson(const Path& path) {
    std::ostringstream out;
    out << "{\"fillRule\":" << json::escape(fillRuleName(path.fillRule))
        << ",\"bounds\":" << boundsJson(path.bounds)
        << ",\"budget\":" << budgetJson(path.budget)
        << ",\"contours\":[";
    for (std::size_t ci = 0; ci < path.contours.size(); ++ci) {
        if (ci != 0) {
            out << ",";
        }
        const Contour& contour = path.contours[ci];
        out << "{\"index\":" << ci
            << ",\"direction\":" << json::escape(directionName(
                   contour.declaredDirection))
            << ",\"actualDirection\":"
            << json::escape(directionName(contour.actualDirection))
            << ",\"role\":"
            << json::escape(contour.hasRole
                                ? (contour.roleHole ? "hole" : "outer")
                                : "unspecified")
            << ",\"signedArea\":" << num(contour.signedArea)
            << ",\"bounds\":" << boundsJson(contour.bounds)
            << ",\"vertices\":[";
        for (std::size_t vi = 0; vi < contour.vertices.size(); ++vi) {
            if (vi != 0) {
                out << ",";
            }
            out << pointJson(contour.vertices[vi]);
        }
        out << "],\"segments\":[";
        for (std::size_t si = 0; si < contour.segments.size(); ++si) {
            if (si != 0) {
                out << ",";
            }
            const Segment& segment = contour.segments[si];
            out << "{\"sourceIndex\":" << segment.sourceIndex
                << ",\"type\":" << json::escape(segmentTypeName(segment.type))
                << ",\"from\":" << pointJson(segment.p0)
                << ",\"to\":" << pointJson(segment.p1);
            if (segment.type == SegmentType::Quadratic) {
                out << ",\"control1\":" << pointJson(segment.c1);
            } else if (segment.type == SegmentType::Cubic) {
                out << ",\"control1\":" << pointJson(segment.c1)
                    << ",\"control2\":" << pointJson(segment.c2);
            }
            out << "}";
        }
        out << "]}";
    }
    out << "]}";
    return out.str();
}

std::string diagnosticsJson(const Diagnostics& diagnostics) {
    std::ostringstream out;
    out << "{\"monotonicSegmentCount\":"
        << diagnostics.monotonicSegmentCount
        << ",\"intersectionCount\":" << diagnostics.intersections.size()
        << ",\"nearCoincidentCount\":" << diagnostics.nearCoincident.size()
        << ",\"degenerateCount\":" << diagnostics.degenerate.size()
        << ",\"isolatedCount\":" << diagnostics.isolated.size()
        << ",\"intersections\":[";
    for (std::size_t i = 0; i < diagnostics.intersections.size(); ++i) {
        if (i != 0) {
            out << ",";
        }
        const Intersection& item = diagnostics.intersections[i];
        out << "{\"point\":" << pointJson(item.point)
            << ",\"contourA\":" << item.contourA
            << ",\"segmentA\":" << item.segmentA
            << ",\"contourB\":" << item.contourB
            << ",\"segmentB\":" << item.segmentB
            << ",\"kind\":" << json::escape(item.kind) << "}";
    }
    out << "],\"nearCoincident\":[";
    for (std::size_t i = 0; i < diagnostics.nearCoincident.size(); ++i) {
        if (i != 0) {
            out << ",";
        }
        const NearCoincidentPair& item = diagnostics.nearCoincident[i];
        out << "{\"contourA\":" << item.contourA
            << ",\"segmentA\":" << item.segmentA
            << ",\"contourB\":" << item.contourB
            << ",\"segmentB\":" << item.segmentB
            << ",\"overlapDirection\":"
            << json::escape(item.overlapDirection)
            << ",\"confidence\":" << json::escape(item.confidence)
            << ",\"maxEndpointDistance\":"
            << num(item.maxEndpointDistance) << "}";
    }
    out << "],\"degenerate\":[";
    for (std::size_t i = 0; i < diagnostics.degenerate.size(); ++i) {
        if (i != 0) {
            out << ",";
        }
        const DegenerateSegment& item = diagnostics.degenerate[i];
        out << "{\"contour\":" << item.contour
            << ",\"segment\":" << item.segment
            << ",\"reason\":" << json::escape(item.reason)
            << ",\"measure\":" << num(item.measure) << "}";
    }
    out << "],\"isolated\":[";
    for (std::size_t i = 0; i < diagnostics.isolated.size(); ++i) {
        if (i != 0) {
            out << ",";
        }
        const IsolatedSegment& item = diagnostics.isolated[i];
        out << "{\"contour\":" << item.contour
            << ",\"segment\":" << item.segment
            << ",\"reason\":" << json::escape(item.reason) << "}";
    }
    out << "]}";
    return out.str();
}

const char* pixelSource(const PixelDecision& pixel) {
    if (pixel.uncertain) {
        return "uncertain";
    }
    if (pixel.ruleSensitive) {
        return "rule_sensitive";
    }
    return "consistent";
}

const char* coverageState(bool definite, bool filled) {
    if (!definite) {
        return "uncertain";
    }
    return filled ? "filled" : "empty";
}

std::string pixelTrace(int width, int height,
                       const std::vector<PixelDecision>& pixels) {
    std::ostringstream out;
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            const PixelDecision& pixel =
                pixels[static_cast<std::size_t>(y * width + x)];
            out << "{\"x\":" << x << ",\"y\":" << y
                << ",\"winding\":{\"lower\":" << pixel.windingLower
                << ",\"upper\":" << pixel.windingUpper << "}"
                << ",\"crossings\":{\"lower\":" << pixel.crossingsLower
                << ",\"upper\":" << pixel.crossingsUpper << "}"
                << ",\"nonzero\":"
                << json::escape(coverageState(pixel.nonzeroDefinite,
                                              pixel.nonzeroFilled))
                << ",\"evenodd\":"
                << json::escape(coverageState(pixel.evenOddDefinite,
                                              pixel.evenOddFilled))
                << ",\"source\":" << json::escape(pixelSource(pixel))
                << ",\"nearestBoundaryWorld\":"
                << num(pixel.nearestBoundaryWorld)
                << ",\"nearestBoundaryPixel\":"
                << num(pixel.nearestBoundaryPixel)
                << ",\"ambiguousSegments\":[";
            for (std::size_t i = 0; i < pixel.ambiguousSegments.size(); ++i) {
                if (i != 0) {
                    out << ",";
                }
                out << "[" << pixel.ambiguousSegments[i].first << ","
                    << pixel.ambiguousSegments[i].second << "]";
            }
            out << "]}\n";
        }
    }
    return out.str();
}

std::string metricsJson(const RenderResult& result, double degradeRatio) {
    std::ostringstream out;
    const RenderMetrics& metrics = result.metrics;
    out << "{\"totalPixels\":" << metrics.totalPixels
        << ",\"nonzeroCoveredPixels\":" << metrics.nonzeroCoveredPixels
        << ",\"evenOddCoveredPixels\":" << metrics.evenOddCoveredPixels
        << ",\"ruleSensitivePixels\":" << metrics.ruleSensitivePixels
        << ",\"uncertainPixels\":" << metrics.uncertainPixels
        << ",\"ambiguousEdgeReports\":" << metrics.ambiguousEdgeReports
        << ",\"nonzeroCoverageRatio\":" << num(metrics.nonzeroCoverageRatio)
        << ",\"evenOddCoverageRatio\":" << num(metrics.evenOddCoverageRatio)
        << ",\"coverageAreaDeviation\":"
        << num(metrics.coverageAreaDeviation)
        << ",\"degradeRatioThreshold\":" << num(degradeRatio)
        << ",\"degraded\":" << (metrics.degraded ? "true" : "false")
        << ",\"committable\":" << (metrics.committable ? "true" : "false")
        << "}";
    return out.str();
}

std::string budgetAnalysisJson(const BudgetAnalysis& analysis) {
    std::ostringstream out;
    out << "{\"tighteningOrder\":[";
    for (std::size_t i = 0; i < analysis.tighteningOrder.size(); ++i) {
        if (i != 0) {
            out << ",";
        }
        out << json::escape(analysis.tighteningOrder[i]);
    }
    out << "],\"impacts\":[";
    for (std::size_t i = 0; i < analysis.impacts.size(); ++i) {
        if (i != 0) {
            out << ",";
        }
        const BudgetImpact& impact = analysis.impacts[i];
        out << "{\"item\":" << json::escape(impact.item)
            << ",\"originalValue\":" << num(impact.originalValue)
            << ",\"perturbedValue\":" << num(impact.perturbedValue)
            << ",\"uncertainPixelDelta\":" << impact.uncertainPixelDelta
            << ",\"perturbedUncertainPixels\":"
            << impact.perturbedUncertainPixels
            << ",\"perturbedCoverageRatio\":"
            << num(impact.perturbedCoverageRatio)
            << ",\"signedCoverageAreaDelta\":"
            << num(impact.signedCoverageAreaDelta)
            << ",\"coverageAreaDeviation\":"
            << num(impact.coverageAreaDeviation) << "}";
    }
    const BoundDiff& diff = analysis.toleranceComparison;
    out << "],\"toleranceComparison\":{\"oldBudget\":"
        << budgetJson(diff.oldBudget) << ",\"newBudget\":"
        << budgetJson(diff.newBudget)
        << ",\"changedPixels\":" << diff.changedPixels
        << ",\"lowerBoundDeltaSum\":" << diff.lowerBoundDeltaSum
        << ",\"upperBoundDeltaSum\":" << diff.upperBoundDeltaSum
        << ",\"uncertainPixelDelta\":" << diff.uncertainPixelDelta
        << ",\"lowerBoundHistogram\":{";
    bool first = true;
    for (const auto& entry : diff.lowerBoundHistogram) {
        if (!first) {
            out << ",";
        }
        first = false;
        out << json::escape(std::to_string(entry.first)) << ":"
            << entry.second;
    }
    out << "},\"remainingBudget\":"
        << num(diff.newBudget.totalLimit -
               diff.newBudget.vertexMerge -
               diff.newBudget.intersectionClassify -
               diff.newBudget.sampleDistance)
        << "}}";
    return out.str();
}

std::string hashBundle(const std::vector<std::pair<std::string, std::string>>& files) {
    std::string aggregate;
    for (const auto& file : files) {
        aggregate += file.first;
        aggregate.push_back('\0');
        aggregate += std::to_string(file.second.size());
        aggregate.push_back('\0');
        aggregate += file.second;
        aggregate.push_back('\0');
    }
    return sha256Hex(aggregate);
}

std::string fileHashJson(
    const std::map<std::string, std::string>& hashes,
    const std::vector<std::string>& order) {
    std::ostringstream out;
    out << "{";
    for (std::size_t i = 0; i < order.size(); ++i) {
        if (i != 0) {
            out << ",";
        }
        out << json::escape(order[i]) << ":"
            << json::escape(hashes.at(order[i]));
    }
    out << "}";
    return out.str();
}

}  // namespace

int runRenderPipeline(const PipelineOptions& options) {
    if (options.inputFile.empty() || options.outputDir.empty()) {
        Error error;
        error.code = "E_ARGUMENT_INVALID";
        error.detail = "--input and --out are required";
        std::cout << errorToJson(error) << "\n";
        return 1;
    }

    std::ifstream input(options.inputFile, std::ios::binary);
    if (!input) {
        Error error;
        error.code = "E_INPUT_IO";
        error.detail = "cannot open input file";
        std::cout << errorToJson(error) << "\n";
        return 1;
    }
    std::ostringstream inputBuffer;
    inputBuffer << input.rdbuf();
    const std::string source = inputBuffer.str();

    ParseOptions parseOptions;
    parseOptions.budget = options.budget;
    parseOptions.injection = options.injection;
    parseOptions.overrideBudget = options.budgetProvided;
    const ParseResult parsed =
        parsePathJson(source, parseOptions, options.inputFile);
    if (!parsed.path.has_value()) {
        std::cout << errorToJson(parsed.error) << "\n";
        return 2;
    }

    int remainingBlocks = options.cancelAfterBlocks;
    const auto isCancelled = [&remainingBlocks]() {
        if (remainingBlocks < 0) {
            return false;
        }
        if (remainingBlocks == 0) {
            return true;
        }
        --remainingBlocks;
        return false;
    };

    RenderOptions renderOptions;
    renderOptions.width = options.width;
    renderOptions.height = options.height;
    renderOptions.degradeRatio = options.degradeRatio;
    RenderResult render =
        renderPath(*parsed.path, renderOptions, isCancelled);
    if (render.cancelled) {
        Error error;
        error.code = "E_CANCELLED";
        error.detail = "cancelled at sampling-block checkpoint";
        std::cout << "{\"code\":\"E_CANCELLED\",\"detail\":"
                  << json::escape(error.detail)
                  << ",\"publishedBitmapRetained\":true,\"wroteOutputs\":false}\n";
        return 4;
    }
    if (!render.error.ok()) {
        std::cout << errorToJson(render.error) << "\n";
        return 2;
    }

    if (options.comparePrevious) {
        std::string previousDetail;
        if (!validateToleranceBudget(options.previousBudget, previousDetail)) {
            Error error;
            error.code = "E_TOLERANCE_INVALID";
            error.detail = "previous " + previousDetail;
            std::cout << errorToJson(error) << "\n";
            return 2;
        }
    }

    ToleranceBudget previous = options.previousBudget;
    if (!options.comparePrevious) {
        previous = parsed.path->budget;
        previous.vertexMerge *= 2.0;
        previous.intersectionClassify *= 2.0;
        previous.sampleDistance *= 2.0;
    }
    const BudgetAnalysis analysis =
        analyzeBudget(*parsed.path, renderOptions, previous);

    const std::string normalized = normalizedPathJson(*parsed.path);
    const std::string normalizedHash = sha256Hex(normalized);
    const std::string coverageNonzero =
        pgm(renderOptions.width, renderOptions.height, render.coverage[0]);
    const std::string coverageEvenOdd =
        pgm(renderOptions.width, renderOptions.height, render.coverage[1]);
    const std::string diffPgm =
        pgm(renderOptions.width, renderOptions.height, render.diffMap);
    const std::string diffColor =
        diffPpm(renderOptions.width, renderOptions.height, render.diffMap);
    const std::string fillPgm =
        pgm(renderOptions.width, renderOptions.height, render.fillMap);
    const std::string fillColor =
        fillPpm(renderOptions.width, renderOptions.height, render.fillMap,
                render.sourceMap);
    const std::string sourcePgm =
        pgm(renderOptions.width, renderOptions.height, render.sourceMap);
    const std::string pixels =
        pixelTrace(renderOptions.width, renderOptions.height, render.pixels);

    const std::vector<std::pair<std::string, std::string>> hashFiles = {
        {"normalized_path.json", normalized},
        {"coverage_nonzero.pgm", coverageNonzero},
        {"coverage_evenodd.pgm", coverageEvenOdd},
        {"coverage_diff.pgm", diffPgm},
        {"coverage_diff.ppm", diffColor},
        {"fill_bitmap.pgm", fillPgm},
        {"fill_bitmap.ppm", fillColor},
        {"pixel_source.pgm", sourcePgm},
        {"pixel_trace.jsonl", pixels},
    };
    std::map<std::string, std::string> hashes;
    for (const auto& file : hashFiles) {
        hashes[file.first] = sha256Hex(file.second);
    }
    const std::string contentHash = hashBundle(hashFiles);
    const std::vector<std::string> hashOrder = {
        "normalized_path.json",
        "coverage_nonzero.pgm",
        "coverage_evenodd.pgm",
        "coverage_diff.pgm",
        "coverage_diff.ppm",
        "fill_bitmap.pgm",
        "fill_bitmap.ppm",
        "pixel_source.pgm",
        "pixel_trace.jsonl",
    };

    std::ostringstream trace;
    const std::string ruleConsistency =
        render.metrics.uncertainPixels > 0
            ? "uncertain"
            : (render.metrics.ruleSensitivePixels > 0 ? "rule_sensitive"
                                                      : "consistent");
    trace << "{\"event\":\"run\",\"fillRule\":"
          << json::escape(fillRuleName(parsed.path->fillRule))
          << ",\"budget\":" << budgetJson(parsed.path->budget)
          << ",\"intersectionCount\":"
          << render.diagnostics.intersections.size()
          << ",\"monotonicSegmentCount\":"
          << render.diagnostics.monotonicSegmentCount
          << ",\"nearCoincidentCount\":"
          << render.diagnostics.nearCoincident.size()
          << ",\"degenerateCount\":" << render.diagnostics.degenerate.size()
          << ",\"isolatedCount\":" << render.diagnostics.isolated.size()
          << ",\"ruleSensitivePixels\":" << render.metrics.ruleSensitivePixels
          << ",\"uncertainPixels\":" << render.metrics.uncertainPixels
          << ",\"ruleConsistency\":" << json::escape(ruleConsistency)
          << ",\"degraded\":"
          << (render.metrics.degraded ? "true" : "false")
          << ",\"coverageHashes\":"
          << fileHashJson(hashes,
                          {"coverage_nonzero.pgm", "coverage_evenodd.pgm"})
          << ",\"normalizedPathHash\":" << json::escape(normalizedHash)
          << ",\"contentHash\":" << json::escape(contentHash) << "}\n";
    for (const NearCoincidentPair& pair : render.diagnostics.nearCoincident) {
        trace << "{\"event\":\"near_coincident\",\"contourA\":"
              << pair.contourA << ",\"segmentA\":" << pair.segmentA
              << ",\"contourB\":" << pair.contourB
              << ",\"segmentB\":" << pair.segmentB
              << ",\"overlapDirection\":"
              << json::escape(pair.overlapDirection)
              << ",\"confidence\":" << json::escape(pair.confidence) << "}\n";
    }
    for (const DegenerateSegment& item : render.diagnostics.degenerate) {
        trace << "{\"event\":\"degenerate\",\"contour\":" << item.contour
              << ",\"segment\":" << item.segment
              << ",\"reason\":" << json::escape(item.reason)
              << ",\"measure\":" << num(item.measure) << "}\n";
    }
    for (const IsolatedSegment& item : render.diagnostics.isolated) {
        trace << "{\"event\":\"isolated\",\"contour\":" << item.contour
              << ",\"segment\":" << item.segment
              << ",\"reason\":" << json::escape(item.reason) << "}\n";
    }
    for (const auto& segment : render.ambiguousSegments) {
        trace << "{\"event\":\"ambiguous_edge\",\"contour\":"
              << segment.first << ",\"segment\":" << segment.second << "}\n";
    }
    const std::string traceBytes = trace.str();
    const std::string traceHash = sha256Hex(traceBytes);

    std::ostringstream result;
    result << "{\"schema\":\"fill-pipeline-result/1\",\"status\":"
           << json::escape(render.metrics.degraded ? "degraded" : "ok")
           << ",\"input\":"
           << json::escape(fs::path(options.inputFile).filename().string())
           << ",\"inputHash\":" << json::escape(sha256Hex(source))
           << ",\"fillRule\":"
           << json::escape(fillRuleName(parsed.path->fillRule))
           << ",\"ruleConsistency\":" << json::escape(ruleConsistency)
           << ",\"normalizedPathHash\":" << json::escape(normalizedHash)
           << ",\"grid\":{\"width\":" << renderOptions.width
           << ",\"height\":" << renderOptions.height << "}"
           << ",\"transform\":{\"scale\":" << num(render.transform.scale)
           << ",\"offsetX\":" << num(render.transform.offsetX)
           << ",\"offsetY\":" << num(render.transform.offsetY)
           << ",\"worldPerPixel\":"
           << num(render.transform.worldPerPixel) << "}"
           << ",\"budgetAnalysis\":" << budgetAnalysisJson(analysis)
           << ",\"diagnostics\":" << diagnosticsJson(render.diagnostics)
           << ",\"metrics\":" << metricsJson(render, options.degradeRatio)
           << ",\"hashes\":" << fileHashJson(hashes, hashOrder)
           << ",\"traceHash\":" << json::escape(traceHash)
           << ",\"contentHash\":" << json::escape(contentHash)
           << ",\"degraded\":"
           << (render.metrics.degraded ? "true" : "false")
           << ",\"committable\":"
           << (render.metrics.committable ? "true" : "false")
           << ",\"submission\":"
           << json::escape(render.metrics.committable ? "allowed" : "blocked")
           << ",\"notices\":[";
    bool firstNotice = true;
    if (render.metrics.uncertainPixels > 0) {
        result << "{\"code\":\"E_AMBIGUOUS_EDGE\",\"pixelCount\":"
               << render.metrics.uncertainPixels << ",\"segments\":[";
        for (std::size_t i = 0; i < render.ambiguousSegments.size(); ++i) {
            if (i != 0) {
                result << ",";
            }
            result << "[" << render.ambiguousSegments[i].first << ","
                   << render.ambiguousSegments[i].second << "]";
        }
        result << "]}";
        firstNotice = false;
    }
    if (render.metrics.degraded) {
        if (!firstNotice) {
            result << ",";
        }
        result << "{\"code\":\"E_DEGRADED_RESULT\",\"reason\":"
               << json::escape("rule-sensitive or uncertain pixel ratio "
                               "exceeds declared threshold")
               << "}";
    }
    result << "]}";
    const std::string resultBytes = result.str();

    std::error_code ec;
    fs::create_directories(options.outputDir, ec);
    if (ec) {
        Error error;
        error.code = "E_OUTPUT_IO";
        error.detail = "cannot create output directory";
        std::cout << errorToJson(error) << "\n";
        return 1;
    }
    const fs::path outputDir(options.outputDir);
    for (const auto& file : hashFiles) {
        if (!writeAtomic(outputDir / file.first, file.second)) {
            Error error;
            error.code = "E_OUTPUT_IO";
            error.detail = "failed to publish " + file.first;
            std::cout << errorToJson(error) << "\n";
            return 1;
        }
    }
    if (!writeAtomic(outputDir / "trace.jsonl", traceBytes) ||
        !writeAtomic(outputDir / "result.json", resultBytes)) {
        Error error;
        error.code = "E_OUTPUT_IO";
        error.detail = "failed to publish result metadata";
        std::cout << errorToJson(error) << "\n";
        return 1;
    }

    std::cout << "{\"status\":"
              << json::escape(render.metrics.degraded ? "degraded" : "ok")
              << ",\"result\":"
              << json::escape((outputDir / "result.json").string())
              << ",\"contentHash\":" << json::escape(contentHash)
              << ",\"degraded\":"
              << (render.metrics.degraded ? "true" : "false") << "}\n";
    return 0;
}

int runSubmit(const std::string& resultFile) {
    std::ifstream input(resultFile, std::ios::binary);
    if (!input) {
        Error error;
        error.code = "E_RESULT_INVALID";
        error.detail = "cannot open result file";
        std::cout << errorToJson(error) << "\n";
        return 1;
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    const json::ParseResult parsed = json::parse(buffer.str());
    if (!parsed.error.ok() || !parsed.value.isObject()) {
        Error error;
        error.code = "E_RESULT_INVALID";
        error.detail = "result file is not valid JSON";
        std::cout << errorToJson(error) << "\n";
        return 1;
    }
    const json::Value* degradedValue = parsed.value.find("degraded");
    const json::Value* statusValue = parsed.value.find("status");
    const bool degraded =
        (degradedValue != nullptr && degradedValue->isBool() &&
         degradedValue->boolean) ||
        (statusValue != nullptr && statusValue->isString() &&
         statusValue->string == "degraded");
    const json::Value* committableValue = parsed.value.find("committable");
    const bool committable =
        committableValue != nullptr && committableValue->isBool() &&
        committableValue->boolean;
    const json::Value* contentHashValue = parsed.value.find("contentHash");
    const std::string contentHash =
        contentHashValue != nullptr && contentHashValue->isString()
            ? contentHashValue->string
            : std::string();

    if (degraded) {
        std::cout << "{\"code\":\"E_DEGRADED_RESULT\",\"detail\":"
                  << json::escape("degraded bitmap cannot be submitted")
                  << ",\"contentHash\":" << json::escape(contentHash) << "}\n";
        return 9;
    }
    if (!committable) {
        std::cout << "{\"code\":\"E_RESULT_NOT_COMMITTABLE\",\"detail\":"
                  << json::escape("result is not marked committable")
                  << ",\"contentHash\":" << json::escape(contentHash) << "}\n";
        return 9;
    }

    const fs::path resultPath(resultFile);
    const fs::path receiptPath = resultPath.parent_path() / "submission.json";
    std::ostringstream receipt;
    receipt << "{\"schema\":\"fill-submission/1\",\"status\":\"submitted\""
            << ",\"contentHash\":" << json::escape(contentHash);
    const json::Value* traceHash = parsed.value.find("traceHash");
    if (traceHash != nullptr && traceHash->isString()) {
        receipt << ",\"traceHash\":" << json::escape(traceHash->string);
    }
    receipt << "}\n";
    const std::string receiptBytes = receipt.str();
    if (!writeAtomic(receiptPath, receiptBytes)) {
        Error error;
        error.code = "E_OUTPUT_IO";
        error.detail = "cannot write submission receipt";
        std::cout << errorToJson(error) << "\n";
        return 1;
    }
    std::cout << receiptBytes;
    return 0;
}

namespace {

std::string contourFromPoints(const std::vector<Vec2>& points,
                              const std::string& direction,
                              const std::string& role = "") {
    std::vector<Vec2> closedPoints = points;
    if (closedPoints.size() > 1 &&
        distance(closedPoints.front(), closedPoints.back()) > 1e-12) {
        closedPoints.push_back(closedPoints.front());
    }
    std::ostringstream out;
    out << "{\"direction\":" << json::escape(direction);
    if (!role.empty()) {
        out << ",\"role\":" << json::escape(role);
    }
    out << ",\"start\":" << pointJson(closedPoints.front())
        << ",\"segments\":[";
    for (std::size_t i = 1; i < closedPoints.size(); ++i) {
        if (i != 1) {
            out << ",";
        }
        out << "{\"type\":\"line\",\"to\":" << pointJson(closedPoints[i])
            << "}";
    }
    out << "]}";
    return out.str();
}

std::string rectangleContour(double x0, double y0, double x1, double y1,
                             bool counterClockwise,
                             const std::string& role = "") {
    std::vector<Vec2> points;
    if (counterClockwise) {
        points = {{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}};
    } else {
        points = {{x0, y0}, {x0, y1}, {x1, y1}, {x1, y0}};
    }
    return contourFromPoints(points,
                             counterClockwise ? "ccw" : "cw", role);
}

std::string sampleDocument(const std::string& fillRule,
                           const std::vector<std::string>& contours,
                           const ToleranceBudget& budget) {
    std::ostringstream out;
    out << "{\"fillRule\":" << json::escape(fillRule)
        << ",\"budget\":" << budgetJson(budget) << ",\"contours\":[";
    for (std::size_t i = 0; i < contours.size(); ++i) {
        if (i != 0) {
            out << ",";
        }
        out << contours[i];
    }
    out << "]}\n";
    return out.str();
}

std::string bezierOuterContour() {
    return R"({"direction":"ccw","start":[120,10],"segments":[)"
           R"({"type":"line","to":[180,10]},)"
           R"({"type":"cubic","control1":[195,10],"control2":[220,20],"to":[220,50]},)"
           R"({"type":"quadratic","control1":[220,80],"to":[180,90]},)"
           R"({"type":"line","to":[120,90]},)"
           R"({"type":"cubic","control1":[100,75],"control2":[100,25],"to":[120,10]}]})";
}

}  // namespace

int runGenerateSamples(const std::string& outputDir, int width, int height) {
    if (outputDir.empty() || width <= 0 || height <= 0) {
        Error error;
        error.code = "E_ARGUMENT_INVALID";
        error.detail = "--out, positive --width and --height are required";
        std::cout << errorToJson(error) << "\n";
        return 1;
    }
    std::error_code ec;
    fs::create_directories(outputDir, ec);
    if (ec) {
        Error error;
        error.code = "E_OUTPUT_IO";
        error.detail = "cannot create sample directory";
        std::cout << errorToJson(error) << "\n";
        return 1;
    }

    ToleranceBudget standard;
    ToleranceBudget relaxed;
    relaxed.sampleDistance = 3.0;
    relaxed.totalLimit = 10.0;

    std::vector<std::pair<std::string, std::string>> samples;
    samples.push_back(
        {"nested_hole.json",
         sampleDocument("nonzero",
                        {rectangleContour(0, 0, 100, 80, true),
                         rectangleContour(20, 20, 80, 60, false, "hole")},
                        standard)});
    samples.push_back(
        {"nested_same_direction.json",
         sampleDocument("nonzero",
                        {rectangleContour(0, 0, 100, 80, true),
                         rectangleContour(20, 20, 80, 60, true)},
                        standard)});
    samples.push_back(
        {"evenodd_nested.json",
         sampleDocument("evenodd",
                        {rectangleContour(0, 0, 100, 80, true),
                         rectangleContour(20, 20, 80, 60, false, "hole")},
                        standard)});
    samples.push_back(
        {"three_contours.json",
         sampleDocument("nonzero",
                        {rectangleContour(0, 0, 120, 100, true),
                         rectangleContour(20, 20, 100, 80, false, "hole"),
                         rectangleContour(40, 40, 80, 60, true)},
                        standard)});
    samples.push_back(
        {"overlap_pair.json",
         sampleDocument("nonzero",
                        {rectangleContour(0, 0, 80, 60, true),
                         rectangleContour(40, 20, 120, 80, true)},
                        standard)});
    samples.push_back(
        {"bezier_hole.json",
         sampleDocument("nonzero",
                        {bezierOuterContour(),
                         rectangleContour(140, 30, 200, 70, false, "hole")},
                        standard)});
    samples.push_back(
        {"degenerate_short.json",
         sampleDocument(
             "nonzero",
             {contourFromPoints({{0, 0},
                                 {0.0001, 0},
                                 {100, 0},
                                 {100, 80},
                                 {0, 80}},
                                "ccw")},
             standard)});
    samples.push_back(
        {"near_coincident.json",
         sampleDocument(
             "nonzero",
             {rectangleContour(0, 0, 100, 80, true),
              rectangleContour(0, 0.001, 100, 80.001, true)},
             standard)});
    samples.push_back(
        {"tolerance_ambiguity.json",
         sampleDocument("nonzero",
                        {rectangleContour(0, 0, 100, 80, true),
                         rectangleContour(20, 20, 80, 60, true)},
                        relaxed)});

    std::ostringstream manifest;
    manifest << "{\"schema\":\"fill-samples/1\",\"grid\":{\"width\":" << width
             << ",\"height\":" << height << "},\"samples\":[";
    bool first = true;
    for (const auto& sample : samples) {
        if (!writeFile(fs::path(outputDir) / sample.first, sample.second)) {
            Error error;
            error.code = "E_OUTPUT_IO";
            error.detail = "failed to write " + sample.first;
            std::cout << errorToJson(error) << "\n";
            return 1;
        }
        ParseOptions parseOptions;
        const ParseResult parsed =
            parsePathJson(sample.second, parseOptions, sample.first);
        if (!parsed.path.has_value()) {
            std::cout << errorToJson(parsed.error) << "\n";
            return 2;
        }
        RenderOptions renderOptions;
        renderOptions.width = width;
        renderOptions.height = height;
        const RenderResult render = renderPath(*parsed.path, renderOptions);
        if (!render.error.ok()) {
            std::cout << errorToJson(render.error) << "\n";
            return 2;
        }
        const std::vector<std::uint8_t>& declaredCoverage =
            parsed.path->fillRule == FillRule::NonZero ? render.coverage[0]
                                                       : render.coverage[1];
        const std::string coverageHash =
            sha256Hex(pgm(width, height, declaredCoverage));
        const std::string nonzeroHash =
            sha256Hex(pgm(width, height, render.coverage[0]));
        const std::string evenOddHash =
            sha256Hex(pgm(width, height, render.coverage[1]));
        if (!first) {
            manifest << ",";
        }
        first = false;
        manifest << "{\"file\":" << json::escape(sample.first)
                 << ",\"inputHash\":" << json::escape(sha256Hex(sample.second))
                 << ",\"fillRule\":"
                 << json::escape(fillRuleName(parsed.path->fillRule))
                 << ",\"baselineCoverageHash\":"
                 << json::escape(coverageHash)
                 << ",\"coverageNonzeroHash\":" << json::escape(nonzeroHash)
                 << ",\"coverageEvenOddHash\":"
                 << json::escape(evenOddHash)
                 << ",\"ruleSensitivePixels\":"
                 << render.metrics.ruleSensitivePixels
                 << ",\"uncertainPixels\":" << render.metrics.uncertainPixels
                 << "}";
    }
    manifest << "]}\n";
    if (!writeFile(fs::path(outputDir) / "manifest.json", manifest.str())) {
        Error error;
        error.code = "E_OUTPUT_IO";
        error.detail = "failed to write sample manifest";
        std::cout << errorToJson(error) << "\n";
        return 1;
    }
    std::cout << "{\"status\":\"ok\",\"samples\":" << samples.size()
              << ",\"manifest\":"
              << json::escape((fs::path(outputDir) / "manifest.json").string())
              << "}\n";
    return 0;
}

}  // namespace fill
