#include "core/Deviation.h"

#include <algorithm>
#include <cmath>
#include <fstream>
#include <iomanip>
#include <limits>
#include <locale>
#include <queue>
#include <set>
#include <sstream>
#include <tuple>

namespace meshedit {
namespace {

struct Triangle {
    Vec3 a;
    Vec3 b;
    Vec3 c;
    Vec3 normal;
    Aabb bounds;
    int face = -1;
};

struct BvhNode {
    Aabb bounds;
    int left = -1;
    int right = -1;
    int begin = 0;
    int count = 0;
    bool leaf() const { return left < 0 && right < 0; }
};

struct NearestResult {
    double distanceSquared = std::numeric_limits<double>::infinity();
    int face = -1;
};

class TriangleBvh {
public:
    explicit TriangleBvh(const Mesh& mesh) {
        faceNormals_.assign(mesh.faces.size(), Vec3{});
        for (std::size_t fi = 0; fi < mesh.faces.size(); ++fi) {
            const Face& face = mesh.faces[fi];
            const int ia = face.corners[0].position;
            const int ib = face.corners[1].position;
            const int ic = face.corners[2].position;
            if (ia < 0 || ib < 0 || ic < 0 ||
                static_cast<std::size_t>(ia) >= mesh.positions.size() ||
                static_cast<std::size_t>(ib) >= mesh.positions.size() ||
                static_cast<std::size_t>(ic) >= mesh.positions.size()) {
                continue;
            }
            Triangle triangle;
            triangle.a = mesh.positions[static_cast<std::size_t>(ia)];
            triangle.b = mesh.positions[static_cast<std::size_t>(ib)];
            triangle.c = mesh.positions[static_cast<std::size_t>(ic)];
            triangle.normal = normalized(cross(triangle.b - triangle.a, triangle.c - triangle.a));
            triangle.bounds.expand(triangle.a);
            triangle.bounds.expand(triangle.b);
            triangle.bounds.expand(triangle.c);
            triangle.face = static_cast<int>(fi);
            faceNormals_[fi] = triangle.normal;
            triangles_.push_back(triangle);
            ids_.push_back(static_cast<int>(triangles_.size() - 1));
        }
        if (!ids_.empty()) {
            nodes_.reserve(ids_.size() * 2);
            root_ = build(0, static_cast<int>(ids_.size()));
        }
    }

    bool empty() const { return ids_.empty(); }

    NearestResult nearest(const Vec3& point) const {
        NearestResult result;
        if (root_ < 0) return result;
        using QueueEntry = std::pair<double, int>;
        std::priority_queue<QueueEntry, std::vector<QueueEntry>, std::greater<QueueEntry>> queue;
        queue.emplace(pointAabbDistanceSquared(point, nodes_[static_cast<std::size_t>(root_)].bounds),
                      root_);
        while (!queue.empty()) {
            const QueueEntry entry = queue.top();
            queue.pop();
            if (entry.first > result.distanceSquared) continue;
            const BvhNode& node = nodes_[static_cast<std::size_t>(entry.second)];
            if (node.leaf()) {
                for (int i = node.begin; i < node.begin + node.count; ++i) {
                    const Triangle& triangle = triangles_[static_cast<std::size_t>(ids_[static_cast<std::size_t>(i)])];
                    const Vec3 closest = closestPointOnTriangle(point, triangle.a, triangle.b,
                                                                triangle.c);
                    const double distance = lengthSquared(point - closest);
                    if (distance < result.distanceSquared) {
                        result.distanceSquared = distance;
                        result.face = triangle.face;
                    }
                }
            } else {
                for (int child : {node.left, node.right}) {
                    if (child < 0) continue;
                    const double distance = pointAabbDistanceSquared(
                        point, nodes_[static_cast<std::size_t>(child)].bounds);
                    if (distance <= result.distanceSquared) queue.emplace(distance, child);
                }
            }
        }
        return result;
    }

    Vec3 faceNormal(int faceId) const {
        if (faceId < 0 || static_cast<std::size_t>(faceId) >= faceNormals_.size()) return {};
        return faceNormals_[static_cast<std::size_t>(faceId)];
    }

private:
    int build(int begin, int end) {
        const int index = static_cast<int>(nodes_.size());
        nodes_.push_back(BvhNode{});
        BvhNode node;
        node.begin = begin;
        node.count = end - begin;
        for (int i = begin; i < end; ++i) {
            node.bounds.expand(triangles_[static_cast<std::size_t>(ids_[static_cast<std::size_t>(i)])].bounds.min);
            node.bounds.expand(triangles_[static_cast<std::size_t>(ids_[static_cast<std::size_t>(i)])].bounds.max);
        }
        if (end - begin <= 8) {
            nodes_[static_cast<std::size_t>(index)] = node;
            return index;
        }
        const Vec3 extent = node.bounds.extent();
        const int axis = extent.x >= extent.y && extent.x >= extent.z ? 0 : (extent.y >= extent.z ? 1 : 2);
        std::sort(ids_.begin() + begin, ids_.begin() + end, [&](int left, int right) {
            const Triangle& a = triangles_[static_cast<std::size_t>(left)];
            const Triangle& b = triangles_[static_cast<std::size_t>(right)];
            const double ac = (a.bounds.min[static_cast<std::size_t>(axis)] +
                               a.bounds.max[static_cast<std::size_t>(axis)]) * 0.5;
            const double bc = (b.bounds.min[static_cast<std::size_t>(axis)] +
                               b.bounds.max[static_cast<std::size_t>(axis)]) * 0.5;
            if (ac != bc) return ac < bc;
            return left < right;
        });
        const int middle = begin + (end - begin) / 2;
        node.left = build(begin, middle);
        node.right = build(middle, end);
        node.count = 0;
        nodes_[static_cast<std::size_t>(index)] = node;
        return index;
    }

    std::vector<Triangle> triangles_;
    std::vector<int> ids_;
    std::vector<BvhNode> nodes_;
    std::vector<Vec3> faceNormals_;
    int root_ = -1;
};

std::vector<Vec3> sampleTriangle(const Vec3& a, const Vec3& b, const Vec3& c, int density) {
    std::vector<Vec3> points;
    const int d = std::max(2, density + 1);
    points.reserve(static_cast<std::size_t>((d - 1) * (d - 2) / 2 + 1));
    // Strictly interior barycentric samples avoid treating a point on a shared
    // edge as simultaneously belonging to two differently oriented faces.
    for (int i = 1; i < d; ++i) {
        for (int j = 1; j < d - i; ++j) {
            const int k = d - i - j;
            if (k <= 0) continue;
            const double wa = static_cast<double>(i) / d;
            const double wb = static_cast<double>(j) / d;
            const double wc = static_cast<double>(k) / d;
            points.push_back(a * wa + b * wb + c * wc);
        }
    }
    if (points.empty()) points.push_back((a + b + c) / 3.0);
    return points;
}

DeviationStats makeStats(const std::vector<double>& values) {
    DeviationStats stats;
    if (values.empty()) return stats;
    stats.samples = values.size();
    stats.minimum = values.front();
    stats.maximum = values.back();
    double sum = 0.0;
    double sumSquares = 0.0;
    for (double value : values) {
        sum += value;
        sumSquares += value * value;
    }
    stats.mean = sum / static_cast<double>(values.size());
    stats.rms = std::sqrt(sumSquares / static_cast<double>(values.size()));
    auto quantile = [&](double q) {
        if (values.size() == 1) return values.front();
        const double position = q * static_cast<double>(values.size() - 1);
        const std::size_t lower = static_cast<std::size_t>(std::floor(position));
        const std::size_t upper = static_cast<std::size_t>(std::ceil(position));
        const double fraction = position - static_cast<double>(lower);
        return values[lower] * (1.0 - fraction) + values[upper] * fraction;
    };
    stats.p50 = quantile(0.50);
    stats.p90 = quantile(0.90);
    stats.p95 = quantile(0.95);
    stats.p99 = quantile(0.99);
    return stats;
}

struct DirectionResult {
    std::vector<double> distanceValues;
    std::vector<double> angleValues;
    DeviationStats distances;
    DeviationStats angles;
    std::vector<int> violatingSourceFaces;
    std::vector<int> violatingTargetFaces;
};

DirectionResult compareDirection(const Mesh& source, const TriangleBvh& targetBvh,
                                 const ValidationConfig& config) {
    DirectionResult result;
    std::vector<double> distances;
    std::vector<double> angles;
    std::set<int> sourceViolations;
    std::set<int> targetViolations;
    for (std::size_t sourceFace = 0; sourceFace < source.faces.size(); ++sourceFace) {
        const Face& face = source.faces[sourceFace];
        const int ia = face.corners[0].position;
        const int ib = face.corners[1].position;
        const int ic = face.corners[2].position;
        if (ia < 0 || ib < 0 || ic < 0 ||
            static_cast<std::size_t>(ia) >= source.positions.size() ||
            static_cast<std::size_t>(ib) >= source.positions.size() ||
            static_cast<std::size_t>(ic) >= source.positions.size()) {
            continue;
        }
        const Vec3& a = source.positions[static_cast<std::size_t>(ia)];
        const Vec3& b = source.positions[static_cast<std::size_t>(ib)];
        const Vec3& c = source.positions[static_cast<std::size_t>(ic)];
        const Vec3 sourceNormal = normalized(cross(b - a, c - a));
        for (const Vec3& point : sampleTriangle(a, b, c, config.samplingDensity)) {
            const NearestResult nearest = targetBvh.nearest(point);
            if (nearest.face < 0) continue;
            const double distance = std::sqrt(nearest.distanceSquared);
            distances.push_back(distance);
            if (distance > config.distanceTolerance) {
                sourceViolations.insert(static_cast<int>(sourceFace));
                targetViolations.insert(nearest.face);
            }
            const Vec3 targetNormal = targetBvh.faceNormal(nearest.face);
            if (lengthSquared(sourceNormal) > 0.0 && lengthSquared(targetNormal) > 0.0) {
                const double cosine = std::clamp(dot(sourceNormal, targetNormal), -1.0, 1.0);
                angles.push_back(std::acos(cosine) * 180.0 / 3.14159265358979323846);
            }
        }
    }
    std::sort(distances.begin(), distances.end());
    std::sort(angles.begin(), angles.end());
    result.distanceValues = distances;
    result.angleValues = angles;
    result.distances = makeStats(distances);
    result.angles = makeStats(angles);
    result.violatingSourceFaces.assign(sourceViolations.begin(), sourceViolations.end());
    result.violatingTargetFaces.assign(targetViolations.begin(), targetViolations.end());
    return result;
}

std::string statsJson(const DeviationStats& stats, int indent) {
    const std::string pad(static_cast<std::size_t>(indent), ' ');
    std::ostringstream out;
    out.imbue(std::locale::classic());
    out << pad << "{\"samples\":" << stats.samples
        << ",\"min\":" << formatDouble(stats.minimum)
        << ",\"max\":" << formatDouble(stats.maximum)
        << ",\"mean\":" << formatDouble(stats.mean)
        << ",\"rms\":" << formatDouble(stats.rms)
        << ",\"p50\":" << formatDouble(stats.p50)
        << ",\"p90\":" << formatDouble(stats.p90)
        << ",\"p95\":" << formatDouble(stats.p95)
        << ",\"p99\":" << formatDouble(stats.p99) << '}';
    return out.str();
}

std::string intArrayJson(const std::vector<int>& values) {
    std::ostringstream out;
    out << '[';
    for (std::size_t i = 0; i < values.size(); ++i) {
        if (i) out << ',';
        out << values[i];
    }
    out << ']';
    return out.str();
}

double scaledTolerance(const ValidationConfig& config, double scale) {
    return config.distanceTolerance * std::max(1.0, scale);
}

} // namespace

std::string ValidationReport::toJson() const {
    std::ostringstream out;
    out.imbue(std::locale::classic());
    const double extent = truthMetrics.bounds.valid() ? length(truthMetrics.bounds.extent()) : 1.0;
    const double tol = scaledTolerance(config, extent);
    out << "{\n"
        << "  \"config\":{\"distanceTolerance\":" << formatDouble(config.distanceTolerance)
        << ",\"samplingDensity\":" << config.samplingDensity
        << ",\"normalAngleToleranceDegrees\":"
        << formatDouble(config.normalAngleToleranceDegrees) << "},\n"
        << "  \"truthMetrics\":" << metricsJson(truthMetrics, 2) << ",\n"
        << "  \"candidateMetrics\":" << metricsJson(candidateMetrics, 2) << ",\n"
        << "  \"metricsWithinTolerance\":" << (metricsWithinTolerance ? "true" : "false")
        << ",\n"
        << "  \"metricTolerance\":" << formatDouble(tol) << ",\n"
        << "  \"forward\":" << statsJson(forward, 2) << ",\n"
        << "  \"reverse\":" << statsJson(reverse, 2) << ",\n"
        << "  \"combined\":" << statsJson(combined, 2) << ",\n"
        << "  \"normalAngleDegrees\":" << statsJson(normalAngleDegrees, 2) << ",\n"
        << "  \"violatingTruthFaces\":" << intArrayJson(violatingTruthFaces) << ",\n"
        << "  \"violatingCandidateFaces\":" << intArrayJson(violatingCandidateFaces) << ",\n"
        << "  \"passed\":" << (passed ? "true" : "false") << "\n}\n";
    return out.str();
}

std::string ValidationReport::toText() const {
    std::ostringstream out;
    out.imbue(std::locale::classic());
    out << "Mesh accuracy validation\n";
    out << "distance tolerance: " << formatDouble(config.distanceTolerance) << '\n';
    out << "sampling density: " << config.samplingDensity << '\n';
    out << "forward  min/mean/p95/max: " << formatDouble(forward.minimum) << " / "
        << formatDouble(forward.mean) << " / " << formatDouble(forward.p95) << " / "
        << formatDouble(forward.maximum) << '\n';
    out << "reverse  min/mean/p95/max: " << formatDouble(reverse.minimum) << " / "
        << formatDouble(reverse.mean) << " / " << formatDouble(reverse.p95) << " / "
        << formatDouble(reverse.maximum) << '\n';
    out << "normal angle p95/max (deg): " << formatDouble(normalAngleDegrees.p95) << " / "
        << formatDouble(normalAngleDegrees.maximum) << '\n';
    out << "violating truth faces: " << violatingTruthFaces.size()
        << ", candidate faces: " << violatingCandidateFaces.size() << '\n';
    out << "result: " << (passed ? "PASS" : "FAIL") << '\n';
    return out.str();
}

Result<ValidationReport> validateMeshes(const Mesh& truth, const Mesh& candidate,
                                        const ValidationConfig& config) {
    if (!std::isfinite(config.distanceTolerance) || config.distanceTolerance < 0.0) {
        return Error{Code::InvalidArgument,
                     "distance tolerance must be finite and non-negative", 0, {}};
    }
    if (config.samplingDensity < 1 || config.samplingDensity > 64) {
        return Error{Code::InvalidArgument,
                     "sampling density must be between 1 and 64", 0, {}};
    }
    if (truth.faces.empty() || candidate.faces.empty()) {
        return Error{Code::InvalidArgument, "both meshes must contain faces", 0, {}};
    }

    TriangleBvh truthBvh(truth);
    TriangleBvh candidateBvh(candidate);
    if (truthBvh.empty() || candidateBvh.empty()) {
        return Error{Code::InvalidArgument, "mesh contains no valid triangles", 0, {}};
    }

    ValidationReport report;
    report.config = config;
    report.truthMetrics = computeMetrics(truth);
    report.candidateMetrics = computeMetrics(candidate);
    const DirectionResult forward = compareDirection(truth, candidateBvh, config);
    const DirectionResult reverse = compareDirection(candidate, truthBvh, config);
    report.forward = forward.distances;
    report.reverse = reverse.distances;
    std::vector<double> combinedDistances = forward.distanceValues;
    combinedDistances.insert(combinedDistances.end(), reverse.distanceValues.begin(),
                             reverse.distanceValues.end());
    std::vector<double> combinedAngles = forward.angleValues;
    combinedAngles.insert(combinedAngles.end(), reverse.angleValues.begin(),
                          reverse.angleValues.end());
    std::sort(combinedDistances.begin(), combinedDistances.end());
    std::sort(combinedAngles.begin(), combinedAngles.end());
    report.combined = makeStats(combinedDistances);
    report.normalAngleDegrees = makeStats(combinedAngles);

    std::set<int> truthFaces(forward.violatingSourceFaces.begin(),
                             forward.violatingSourceFaces.end());
    truthFaces.insert(reverse.violatingTargetFaces.begin(), reverse.violatingTargetFaces.end());
    std::set<int> candidateFaces(forward.violatingTargetFaces.begin(),
                                 forward.violatingTargetFaces.end());
    candidateFaces.insert(reverse.violatingSourceFaces.begin(),
                          reverse.violatingSourceFaces.end());
    report.violatingTruthFaces.assign(truthFaces.begin(), truthFaces.end());
    report.violatingCandidateFaces.assign(candidateFaces.begin(), candidateFaces.end());

    const double extent = report.truthMetrics.bounds.valid()
        ? length(report.truthMetrics.bounds.extent()) : 1.0;
    const double metricTolerance = scaledTolerance(config, extent);
    auto closeEnough = [metricTolerance](double a, double b) {
        return std::fabs(a - b) <= metricTolerance;
    };
    report.metricsWithinTolerance =
        report.truthMetrics.faceCount == report.candidateMetrics.faceCount &&
        closeEnough(report.truthMetrics.volume, report.candidateMetrics.volume) &&
        closeEnough(report.truthMetrics.surfaceArea, report.candidateMetrics.surfaceArea) &&
        closeEnough(report.truthMetrics.bounds.min.x, report.candidateMetrics.bounds.min.x) &&
        closeEnough(report.truthMetrics.bounds.min.y, report.candidateMetrics.bounds.min.y) &&
        closeEnough(report.truthMetrics.bounds.min.z, report.candidateMetrics.bounds.min.z) &&
        closeEnough(report.truthMetrics.bounds.max.x, report.candidateMetrics.bounds.max.x) &&
        closeEnough(report.truthMetrics.bounds.max.y, report.candidateMetrics.bounds.max.y) &&
        closeEnough(report.truthMetrics.bounds.max.z, report.candidateMetrics.bounds.max.z);
    report.passed = report.forward.maximum <= config.distanceTolerance &&
        report.reverse.maximum <= config.distanceTolerance &&
        report.metricsWithinTolerance &&
        report.normalAngleDegrees.maximum <= config.normalAngleToleranceDegrees;
    return Result<ValidationReport>(std::move(report));
}

Result<bool> writeValidationReport(const ValidationReport& report,
                                   const std::string& path) {
    std::ofstream out(path, std::ios::binary | std::ios::trunc);
    if (!out) return Error{Code::IoError, "cannot create report: " + path, 0, {}};
    out << report.toJson();
    out.flush();
    if (!out) return Error{Code::IoError, "failed while writing report: " + path, 0, {}};
    return success();
}

} // namespace meshedit
