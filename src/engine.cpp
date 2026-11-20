#include "core.hpp"

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <limits>
#include <map>
#include <set>
#include <tuple>

namespace fill {
namespace {

Vec2 toScreen(Vec2 point, const Path& path, const ViewTransform& transform) {
    return {transform.offsetX + (point.x - path.bounds.minX) * transform.scale,
            transform.offsetY + (path.bounds.maxY - point.y) * transform.scale};
}

double pointSegmentDistance(Vec2 p, Vec2 a, Vec2 b) {
    const Vec2 ab = b - a;
    const double denominator = dot(ab, ab);
    if (denominator <= 0.0) {
        return distance(p, a);
    }
    const double t =
        std::max(0.0, std::min(1.0, dot(p - a, ab) / denominator));
    return distance(p, a + ab * t);
}

bool intervalIsNull(int lower, int upper) {
    return upper < lower;
}

bool nonzeroDefinitelyFilled(int lower, int upper) {
    return !intervalIsNull(lower, upper) && (lower > 0 || upper < 0);
}

bool nonzeroDefinitelyEmpty(int lower, int upper) {
    return lower == 0 && upper == 0;
}

bool parityDefinite(int lower, int upper) {
    if (intervalIsNull(lower, upper)) {
        return false;
    }
    return ((lower & 1) == (upper & 1));
}

bool parityFilled(int value) {
    return (value & 1) != 0;
}

double declaredCoverageRatio(const RenderResult& result) {
    int covered = 0;
    for (const PixelDecision& pixel : result.pixels) {
        if (pixel.uncertain) {
            continue;
        }
        const bool filled = result.path.fillRule == FillRule::NonZero
                                ? pixel.nonzeroFilled
                                : pixel.evenOddFilled;
        covered += filled ? 1 : 0;
    }
    return result.metrics.totalPixels == 0
               ? 0.0
               : static_cast<double>(covered) /
                     static_cast<double>(result.metrics.totalPixels);
}

}  // namespace

RenderResult renderPath(const Path& path, const RenderOptions& options,
                        const std::function<bool()>& isCancelled) {
    RenderResult result;
    result.path = path;
    if (options.width <= 0 || options.height <= 0 || options.width > 8192 ||
        options.height > 8192) {
        result.error.code = "E_RENDER_OPTIONS_INVALID";
        result.error.detail = "width and height must be in [1, 8192]";
        return result;
    }
    if (!std::isfinite(options.degradeRatio) || options.degradeRatio < 0.0 ||
        options.degradeRatio > 1.0) {
        result.error.code = "E_RENDER_OPTIONS_INVALID";
        result.error.detail = "degrade ratio must be in [0, 1]";
        return result;
    }
    if (path.contours.empty() || !path.bounds.valid) {
        result.error.code = "E_PATH_INVALID";
        result.error.detail = "render requires at least one non-empty contour";
        return result;
    }

    result.transform = makeViewTransform(path, options.width, options.height);
    result.screenBounds.include({0.0, 0.0});
    result.screenBounds.include(
        {static_cast<double>(options.width), static_cast<double>(options.height)});

    const double flattenTolerance =
        std::max(1e-9, path.budget.intersectionClassify * 0.5);
    const FlatPath flat = flattenPath(path, flattenTolerance);
    result.diagnostics =
        diagnosePath(path, flat, path.budget.intersectionClassify);

    std::vector<FlatEdge> screenEdges;
    screenEdges.reserve(flat.edges.size());
    for (const FlatEdge& edge : flat.edges) {
        FlatEdge screenEdge = edge;
        screenEdge.a = toScreen(edge.a, path, result.transform);
        screenEdge.b = toScreen(edge.b, path, result.transform);
        screenEdges.push_back(screenEdge);
    }

    const int totalPixels = options.width * options.height;
    result.pixels.resize(static_cast<std::size_t>(totalPixels));
    result.coverage[0].assign(static_cast<std::size_t>(totalPixels), 0U);
    result.coverage[1].assign(static_cast<std::size_t>(totalPixels), 0U);
    result.diffMap.assign(static_cast<std::size_t>(totalPixels), 0U);
    result.fillMap.assign(static_cast<std::size_t>(totalPixels), 0U);
    result.sourceMap.assign(static_cast<std::size_t>(totalPixels), 0U);

    std::map<std::pair<int, int>, int> segmentHits;
    for (std::size_t ci = 0; ci < path.contours.size(); ++ci) {
        for (const Segment& segment : path.contours[ci].segments) {
            segmentHits[{static_cast<int>(ci), segment.sourceIndex}] = 0;
        }
    }

    const double intersectionTolerancePixels =
        path.budget.intersectionClassify * result.transform.scale;
    const double boundaryTolerancePixels =
        path.budget.sampleDistance * result.transform.scale;
    const int blockSize = std::max(1, options.blockSize);

    for (int blockY = 0; blockY < options.height; blockY += blockSize) {
        for (int blockX = 0; blockX < options.width; blockX += blockSize) {
            if (isCancelled && isCancelled()) {
                result.cancelled = true;
                result.noticeCode = "E_CANCELLED";
                result.pixels.clear();
                result.coverage[0].clear();
                result.coverage[1].clear();
                result.diffMap.clear();
                result.fillMap.clear();
                result.sourceMap.clear();
                return result;
            }
            const int endY = std::min(options.height, blockY + blockSize);
            const int endX = std::min(options.width, blockX + blockSize);
            for (int y = blockY; y < endY; ++y) {
                for (int x = blockX; x < endX; ++x) {
                    const Vec2 sample = {static_cast<double>(x) + 0.5,
                                         static_cast<double>(y) + 0.5};
                    int windingLower = 0;
                    int windingUpper = 0;
                    int crossingsLower = 0;
                    int crossingsUpper = 0;
                    bool crossingAmbiguity = false;
                    double nearestDistance = std::numeric_limits<double>::infinity();
                    std::pair<int, int> nearestSegment{-1, -1};
                    std::set<std::pair<int, int>> pixelAmbiguousSegments;

                    for (const FlatEdge& edge : screenEdges) {
                        const double edgeDistance =
                            pointSegmentDistance(sample, edge.a, edge.b);
                        if (edgeDistance < nearestDistance) {
                            nearestDistance = edgeDistance;
                            nearestSegment = {edge.contour, edge.segment};
                        }
                        const double minX = std::min(edge.a.x, edge.b.x);
                        const double minY = std::min(edge.a.y, edge.b.y);
                        const double maxY = std::max(edge.a.y, edge.b.y);
                        const bool yStraddles =
                            (edge.a.y > sample.y) != (edge.b.y > sample.y);
                        const double yTolerance = intersectionTolerancePixels;
                        const double xTolerance = intersectionTolerancePixels;
                        const bool yEndpointNear =
                            std::abs(edge.a.y - sample.y) <= yTolerance ||
                            std::abs(edge.b.y - sample.y) <= yTolerance;
                        const bool horizontalBand =
                            maxY >= sample.y - yTolerance &&
                            minY <= sample.y + yTolerance;
                        const bool spansLeft =
                            minX <= sample.x + xTolerance;
                        if (!spansLeft || !horizontalBand) {
                            continue;
                        }

                        bool isCandidate = false;
                        bool isCertain = false;
                        double xIntersection = 0.0;
                        if (yStraddles) {
                            const double dy = edge.b.y - edge.a.y;
                            xIntersection =
                                edge.a.x +
                                (sample.y - edge.a.y) * (edge.b.x - edge.a.x) / dy;
                            if (xIntersection <= sample.x - xTolerance) {
                                isCandidate = true;
                                isCertain = !yEndpointNear;
                            } else if (xIntersection <= sample.x + xTolerance) {
                                isCandidate = true;
                            }
                        } else if (yEndpointNear && spansLeft) {
                            isCandidate = true;
                        }
                        if (!isCandidate) {
                            continue;
                        }

                        if (isCertain) {
                            const int contribution = edge.a.y < edge.b.y ? 1 : -1;
                            windingLower += contribution;
                            windingUpper += contribution;
                            ++crossingsLower;
                            ++crossingsUpper;
                            segmentHits[{edge.contour, edge.segment}] += 1;
                        } else {
                            const int contribution = edge.a.y < edge.b.y ? 1 : -1;
                            windingLower += std::min(0, contribution);
                            windingUpper += std::max(0, contribution);
                            crossingsUpper += 1;
                            crossingAmbiguity = true;
                            pixelAmbiguousSegments.insert(
                                {edge.contour, edge.segment});
                            segmentHits[{edge.contour, edge.segment}] += 1;
                        }
                    }

                    const bool nearBoundary =
                        std::isfinite(nearestDistance) &&
                        nearestDistance <= boundaryTolerancePixels;
                    if (nearBoundary) {
                        windingLower -= 1;
                        windingUpper += 1;
                        crossingsUpper += 1;
                        if (nearestSegment.first >= 0) {
                            pixelAmbiguousSegments.insert(nearestSegment);
                        }
                    }

                    PixelDecision pixel;
                    pixel.windingLower = windingLower;
                    pixel.windingUpper = windingUpper;
                    pixel.crossingsLower = crossingsLower;
                    pixel.crossingsUpper = crossingsUpper;
                    const bool nonzeroFilled =
                        nonzeroDefinitelyFilled(windingLower, windingUpper);
                    const bool nonzeroEmpty =
                        nonzeroDefinitelyEmpty(windingLower, windingUpper);
                    pixel.nonzeroDefinite = nonzeroFilled || nonzeroEmpty;
                    pixel.nonzeroFilled = nonzeroFilled;
                    pixel.evenOddDefinite =
                        parityDefinite(crossingsLower, crossingsUpper);
                    pixel.evenOddFilled =
                        pixel.evenOddDefinite &&
                        parityFilled(crossingsLower);
                    pixel.uncertain = crossingAmbiguity || nearBoundary ||
                                      !pixel.nonzeroDefinite ||
                                      !pixel.evenOddDefinite;
                    pixel.ruleSensitive =
                        !pixel.uncertain && pixel.nonzeroDefinite &&
                        pixel.evenOddDefinite &&
                        pixel.nonzeroFilled != pixel.evenOddFilled;
                    pixel.nearestBoundaryPixel =
                        std::isfinite(nearestDistance) ? nearestDistance : 0.0;
                    pixel.nearestBoundaryWorld =
                        pixel.nearestBoundaryPixel * result.transform.worldPerPixel;
                    pixel.ambiguousSegments.assign(pixelAmbiguousSegments.begin(),
                                                   pixelAmbiguousSegments.end());

                    const std::size_t index =
                        static_cast<std::size_t>(y * options.width + x);
                    result.pixels[index] = pixel;
                    result.coverage[0][index] =
                        pixel.uncertain
                            ? 128U
                            : (pixel.nonzeroFilled ? 255U : 0U);
                    result.coverage[1][index] =
                        pixel.uncertain
                            ? 128U
                            : (pixel.evenOddFilled ? 255U : 0U);
                    if (pixel.uncertain) {
                        result.diffMap[index] = 128U;
                        result.sourceMap[index] = 170U;
                    } else if (pixel.ruleSensitive) {
                        result.diffMap[index] = 255U;
                        result.sourceMap[index] = 85U;
                    }
                    if (pixel.uncertain) {
                        result.fillMap[index] = 128U;
                    } else {
                        const bool filled =
                            path.fillRule == FillRule::NonZero
                                ? pixel.nonzeroFilled
                                : pixel.evenOddFilled;
                        result.fillMap[index] = filled ? 255U : 0U;
                    }
                }
            }
        }
    }

    result.metrics.totalPixels = totalPixels;
    for (const PixelDecision& pixel : result.pixels) {
        result.metrics.ruleSensitivePixels += pixel.ruleSensitive ? 1 : 0;
        result.metrics.uncertainPixels += pixel.uncertain ? 1 : 0;
        const bool nonzeroCovered = pixel.uncertain ? false : pixel.nonzeroFilled;
        const bool evenOddCovered = pixel.uncertain ? false : pixel.evenOddFilled;
        result.metrics.nonzeroCoveredPixels += nonzeroCovered ? 1 : 0;
        result.metrics.evenOddCoveredPixels += evenOddCovered ? 1 : 0;
        for (const auto& segment : pixel.ambiguousSegments) {
            result.ambiguousSegments.push_back(segment);
        }
    }
    std::sort(result.ambiguousSegments.begin(), result.ambiguousSegments.end());
    result.ambiguousSegments.erase(
        std::unique(result.ambiguousSegments.begin(),
                    result.ambiguousSegments.end()),
        result.ambiguousSegments.end());
    result.metrics.ambiguousEdgeReports =
        static_cast<int>(result.ambiguousSegments.size());
    result.metrics.nonzeroCoverageRatio =
        static_cast<double>(result.metrics.nonzeroCoveredPixels) /
        static_cast<double>(totalPixels);
    result.metrics.evenOddCoverageRatio =
        static_cast<double>(result.metrics.evenOddCoveredPixels) /
        static_cast<double>(totalPixels);
    result.metrics.coverageAreaDeviation =
        std::abs(result.metrics.nonzeroCoverageRatio -
                 result.metrics.evenOddCoverageRatio);
    const double degradedRatio =
        static_cast<double>(result.metrics.ruleSensitivePixels +
                            result.metrics.uncertainPixels) /
        static_cast<double>(totalPixels);
    result.metrics.degraded = degradedRatio > options.degradeRatio;
    result.metrics.committable =
        !result.metrics.degraded && result.error.ok();

    if (result.metrics.uncertainPixels > 0) {
        result.noticeCode = "E_AMBIGUOUS_EDGE";
    }
    for (std::size_t ci = 0; ci < path.contours.size(); ++ci) {
        for (const Segment& segment : path.contours[ci].segments) {
            const std::pair<int, int> key{static_cast<int>(ci),
                                          segment.sourceIndex};
            if (segmentHits[key] == 0) {
                result.diagnostics.isolated.push_back(
                    {key.first, key.second,
                     "no_sample_ray_contribution_in_grid"});
            }
        }
    }
    return result;
}

BudgetAnalysis analyzeBudget(const Path& path, const RenderOptions& options,
                             const ToleranceBudget& previousBudget) {
    BudgetAnalysis analysis;
    analysis.tighteningOrder = {
        "intersection_classify", "sample_distance", "vertex_merge"};

    const RenderResult baseline = renderPath(path, options);
    if (!baseline.error.ok() || baseline.cancelled) {
        analysis.toleranceComparison.oldBudget = previousBudget;
        analysis.toleranceComparison.newBudget = path.budget;
        return analysis;
    }
    const double baselineRatio = declaredCoverageRatio(baseline);

    const std::array<std::string, 3> items = {
        "vertex_merge", "intersection_classify", "sample_distance"};
    for (const std::string& item : items) {
        ToleranceBudget perturbed = path.budget;
        double original = 0.0;
        if (item == "vertex_merge") {
            original = perturbed.vertexMerge;
            perturbed.vertexMerge *= 0.5;
        } else if (item == "intersection_classify") {
            original = perturbed.intersectionClassify;
            perturbed.intersectionClassify *= 0.5;
        } else {
            original = perturbed.sampleDistance;
            perturbed.sampleDistance *= 0.5;
        }
        Path changed = path;
        changed.budget = perturbed;
        const RenderResult render = renderPath(changed, options);
        BudgetImpact impact;
        impact.item = item;
        impact.originalValue = original;
        impact.perturbedValue =
            item == "vertex_merge"
                ? perturbed.vertexMerge
                : (item == "intersection_classify" ? perturbed.intersectionClassify
                                                   : perturbed.sampleDistance);
        impact.uncertainPixelDelta =
            render.metrics.uncertainPixels - baseline.metrics.uncertainPixels;
        impact.perturbedUncertainPixels = render.metrics.uncertainPixels;
        impact.perturbedCoverageRatio = declaredCoverageRatio(render);
        impact.signedCoverageAreaDelta =
            declaredCoverageRatio(render) - baselineRatio;
        impact.coverageAreaDeviation =
            std::abs(impact.signedCoverageAreaDelta);
        analysis.impacts.push_back(impact);
    }

    Path oldPath = path;
    oldPath.budget = previousBudget;
    const RenderResult oldRender = renderPath(oldPath, options);
    analysis.toleranceComparison.oldBudget = previousBudget;
    analysis.toleranceComparison.newBudget = path.budget;
    if (oldRender.error.ok() && !oldRender.cancelled &&
        oldRender.pixels.size() == baseline.pixels.size()) {
        for (std::size_t i = 0; i < baseline.pixels.size(); ++i) {
            const int lowerDelta =
                baseline.pixels[i].windingLower - oldRender.pixels[i].windingLower;
            const int upperDelta =
                baseline.pixels[i].windingUpper - oldRender.pixels[i].windingUpper;
            if (lowerDelta != 0 || upperDelta != 0) {
                ++analysis.toleranceComparison.changedPixels;
            }
            analysis.toleranceComparison.lowerBoundDeltaSum += lowerDelta;
            analysis.toleranceComparison.upperBoundDeltaSum += upperDelta;
            analysis.toleranceComparison.lowerBoundHistogram[lowerDelta] += 1;
        }
        analysis.toleranceComparison.uncertainPixelDelta =
            baseline.metrics.uncertainPixels - oldRender.metrics.uncertainPixels;
    }
    return analysis;
}

}  // namespace fill
