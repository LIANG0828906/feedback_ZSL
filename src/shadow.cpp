#include "shadow.h"

#include <algorithm>
#include <cmath>
#include <iomanip>
#include <limits>
#include <sstream>

#include "deterministic.h"
#include "sha256.h"

namespace caisson {
namespace {

bool segmentIntersectsAabb(
    const Vec3& origin,
    const Vec3& direction,
    const Vec3& minimum,
    const Vec3& maximum,
    double maxT) {
    double tMin = 0.0;
    double tMax = maxT;
    for (int axis = 0; axis < 3; ++axis) {
        const double originValue = origin[axis];
        const double directionValue = direction[axis];
        const double minValue = minimum[axis];
        const double maxValue = maximum[axis];
        if (std::abs(directionValue) <= 1.0e-15) {
            if (originValue < minValue || originValue > maxValue) return false;
            continue;
        }
        const double inverse = 1.0 / directionValue;
        double t0 = (minValue - originValue) * inverse;
        double t1 = (maxValue - originValue) * inverse;
        if (t0 > t1) std::swap(t0, t1);
        tMin = std::max(tMin, t0);
        tMax = std::min(tMax, t1);
        if (tMin > tMax) return false;
    }
    return tMax >= 0.0;
}

ShadowTriangle makeTriangle(const Vec3& a, const Vec3& b, const Vec3& c) {
    ShadowTriangle triangle;
    triangle.a = a;
    triangle.b = b;
    triangle.c = c;
    triangle.minimum = {
        std::min({a.x, b.x, c.x}),
        std::min({a.y, b.y, c.y}),
        std::min({a.z, b.z, c.z}),
    };
    triangle.maximum = {
        std::max({a.x, b.x, c.x}),
        std::max({a.y, b.y, c.y}),
        std::max({a.z, b.z, c.z}),
    };
    return triangle;
}

int lerpCrossingMm(
    int offsetA,
    int grayA,
    int offsetB,
    int grayB,
    int threshold) {
    const int delta = grayB - grayA;
    if (delta == 0) return offsetB;
    const double t = static_cast<double>(threshold - grayA) /
                     static_cast<double>(delta);
    return static_cast<int>(std::lround(
        static_cast<double>(offsetA) +
        (static_cast<double>(offsetB) - static_cast<double>(offsetA)) * t));
}

int measureEdge(
    const std::vector<ShadowProfilePoint>& points,
    int minimumIndex,
    int direction,
    int lowThreshold,
    int highThreshold,
    bool& valid) {
    valid = false;
    int lowOffset = points[static_cast<std::size_t>(minimumIndex)].offsetMm;
    int lowGray = points[static_cast<std::size_t>(minimumIndex)].gray;
    int highOffset = lowOffset;
    int highGray = lowGray;
    for (int index = minimumIndex + direction;
         index >= 0 && index < static_cast<int>(points.size());
         index += direction) {
        const ShadowProfilePoint& point = points[static_cast<std::size_t>(index)];
        if (point.gray <= lowThreshold) {
            lowOffset = point.offsetMm;
            lowGray = point.gray;
        }
        if (point.gray >= highThreshold) {
            highOffset = point.offsetMm;
            highGray = point.gray;
            valid = true;
            break;
        }
    }
    if (!valid) return 0;
    const int lowCross = lerpCrossingMm(
        lowOffset, lowGray, highOffset, highGray, lowThreshold + 1);
    const int highCross = lerpCrossingMm(
        lowOffset, lowGray, highOffset, highGray, highThreshold);
    return std::abs(highCross - lowCross);
}

}  // namespace

ShadowSampler::ShadowSampler(const Scene& scene) {
    const int count = scene.shadow.samplesPerAxis * scene.shadow.samplesPerAxis;
    directions_.reserve(static_cast<std::size_t>(count));
    for (int index = 0; index < count; ++index) {
        directions_.push_back(sphereSample(index, count));
    }
    for (const Occluder& occluder : scene.occluders) {
        for (std::size_t i = 0; i < occluder.mesh.indices.size(); i += 3) {
            triangles_.push_back(makeTriangle(
                occluder.mesh.positions[occluder.mesh.indices[i]],
                occluder.mesh.positions[occluder.mesh.indices[i + 1]],
                occluder.mesh.positions[occluder.mesh.indices[i + 2]]));
        }
    }
}

bool ShadowSampler::segmentBlocked(const Vec3& origin, const Vec3& end) const {
    const Vec3 direction = end - origin;
    const double maxT = length(direction);
    if (maxT <= 1.0e-12) return false;
    for (const ShadowTriangle& triangle : triangles_) {
        if (!segmentIntersectsAabb(
                origin, direction, triangle.minimum, triangle.maximum, 1.0)) {
            continue;
        }
        double hit = 0.0;
        if (intersectRayTriangle(
                origin, direction, triangle.a, triangle.b, triangle.c, hit) &&
            hit > 1.0e-8 && hit < 1.0 - 1.0e-8) {
            return true;
        }
    }
    return false;
}

double ShadowSampler::visibility(
    const Vec3& receiverPoint,
    const Lamp& lamp) const {
    if (directions_.empty()) return 1.0;
    int visible = 0;
    for (const Vec3& direction : directions_) {
        const Vec3 samplePoint = lamp.position + direction * lamp.radius;
        if (!segmentBlocked(receiverPoint, samplePoint)) ++visible;
    }
    return static_cast<double>(visible) / static_cast<double>(directions_.size());
}

ShadowReportData buildShadowReport(const Scene& scene, const ShadowSampler& sampler) {
    ShadowReportData report;
    report.samplesPerAxis = scene.shadow.samplesPerAxis;
    report.samplesPerLamp = sampler.sampleCount();
    report.probeSamples = scene.shadow.probeSamples;
    report.probeHalfSpanMm =
        static_cast<int>(std::lround(scene.shadow.probeHalfSpan * 1000.0));

    std::ostringstream fingerprintInput;
    for (const Occluder& occluder : scene.occluders) {
        for (const Lamp& lamp : scene.lamps) {
            for (const Layer& layer : scene.layers) {
                const Vec3 toOccluder = occluder.centroid - lamp.position;
                const double denominator = dot(toOccluder, layer.planeNormal);
                if (std::abs(denominator) <= 1.0e-12) continue;
                const double t =
                    dot(layer.planePoint - lamp.position, layer.planeNormal) /
                    denominator;
                if (!std::isfinite(t) || t <= 1.0) continue;
                const Vec3 landing = lamp.position + toOccluder * t;
                Vec3 across = cross(layer.planeNormal, landing - lamp.position);
                if (lengthSquared(across) <= 1.0e-16) {
                    across = cross(layer.planeNormal, {1.0, 0.0, 0.0});
                }
                if (lengthSquared(across) <= 1.0e-16) {
                    across = cross(layer.planeNormal, {0.0, 0.0, 1.0});
                }
                const Vec3 axis = normalize(across);

                ShadowProfile profile;
                profile.lampId = lamp.id;
                profile.occluderId = occluder.id;
                profile.layerId = layer.id;
                profile.occluderDepthMm =
                    static_cast<int>(std::lround(length(toOccluder) * 1000.0));
                profile.occluderToReceiverMm =
                    static_cast<int>(std::lround(length(landing - occluder.centroid) * 1000.0));
                profile.landing = landing;
                profile.axis = axis;

                int minimumGray = 256;
                int minimumIndex = 0;
                const int count = scene.shadow.probeSamples;
                for (int index = 0; index < count; ++index) {
                    const double normalized =
                        count == 1 ? 0.0 :
                        (2.0 * static_cast<double>(index) /
                             static_cast<double>(count - 1) -
                         1.0);
                    const double offset = normalized * scene.shadow.probeHalfSpan;
                    const Vec3 point = landing + axis * offset;
                    const double visibilityValue = sampler.visibility(point, lamp);
                    const int gray = static_cast<int>(
                        std::lround(std::clamp(visibilityValue, 0.0, 1.0) * 255.0));
                    profile.points.push_back({
                        static_cast<int>(std::lround(offset * 1000.0)),
                        gray,
                    });
                    if (gray < minimumGray) {
                        minimumGray = gray;
                        minimumIndex = index;
                    }
                }
                if (minimumGray <= 217) {
                    bool leftValid = false;
                    bool rightValid = false;
                    profile.leftEdgeWidthMm = measureEdge(
                        profile.points, minimumIndex, -1, 25, 229, leftValid);
                    profile.rightEdgeWidthMm = measureEdge(
                        profile.points, minimumIndex, 1, 25, 229, rightValid);
                    profile.crossed = leftValid && rightValid;
                    profile.transitionWidthMm =
                        std::max(profile.leftEdgeWidthMm, profile.rightEdgeWidthMm);
                }

                fingerprintInput << profile.lampId << '|' << profile.occluderId << '|'
                                 << profile.layerId << '|' << profile.occluderDepthMm << '|'
                                 << profile.occluderToReceiverMm << '|'
                                 << profile.transitionWidthMm << '|';
                for (const ShadowProfilePoint& point : profile.points) {
                    fingerprintInput << point.offsetMm << ':' << point.gray << ',';
                }
                fingerprintInput << '\n';
                report.profiles.push_back(std::move(profile));
            }
        }
    }

    for (const Lamp& lamp : scene.lamps) {
        for (const Occluder& occluder : scene.occluders) {
            for (int vertexIndex = 0;
                 vertexIndex < static_cast<int>(occluder.mesh.positions.size());
                 ++vertexIndex) {
                const Vec3& vertex =
                    occluder.mesh.positions[static_cast<std::size_t>(vertexIndex)];
                const Vec3 direction = vertex - lamp.position;
                for (const Layer& layer : scene.layers) {
                    const double denominator = dot(direction, layer.planeNormal);
                    if (std::abs(denominator) <= 1.0e-12) continue;
                    const double t =
                        dot(layer.planePoint - lamp.position, layer.planeNormal) /
                        denominator;
                    if (!std::isfinite(t) || t <= 1.0) continue;
                    const Vec3 point = lamp.position + direction * t;
                    report.landings.push_back({
                        lamp.id,
                        occluder.id,
                        vertexIndex,
                        layer.id,
                        point,
                    });
                    fingerprintInput << lamp.id << '|' << occluder.id << '|'
                                     << vertexIndex << '|' << layer.id << '|'
                                     << std::llround(point.x * 1000.0) << ','
                                     << std::llround(point.y * 1000.0) << ','
                                     << std::llround(point.z * 1000.0) << '\n';
                }
            }
        }
    }

    report.fingerprint = sha256Hex(fingerprintInput.str());
    return report;
}

std::string shadowLandingsCsv(const ShadowReportData& data) {
    std::ostringstream out;
    out << "lamp_id,dougong_id,vertex_index,layer_id,x_mm,y_mm,z_mm\n";
    for (const ShadowLanding& landing : data.landings) {
        out << landing.lampId << ',' << landing.occluderId << ','
            << landing.vertexIndex << ',' << landing.layerId << ','
            << std::llround(landing.point.x * 1000.0) << ','
            << std::llround(landing.point.y * 1000.0) << ','
            << std::llround(landing.point.z * 1000.0) << '\n';
    }
    return out.str();
}

}  // namespace caisson

