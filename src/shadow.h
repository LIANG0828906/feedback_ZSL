#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "scene.h"

namespace caisson {

struct ShadowTriangle {
    Vec3 a;
    Vec3 b;
    Vec3 c;
    Vec3 minimum;
    Vec3 maximum;
};

class ShadowSampler {
public:
    explicit ShadowSampler(const Scene& scene);

    int sampleCount() const { return static_cast<int>(directions_.size()); }
    double visibility(const Vec3& receiverPoint, const Lamp& lamp) const;
    bool segmentBlocked(const Vec3& origin, const Vec3& end) const;

private:
    std::vector<Vec3> directions_;
    std::vector<ShadowTriangle> triangles_;
};

struct ShadowProfilePoint {
    int offsetMm = 0;
    int gray = 0;
};

struct ShadowProfile {
    std::string lampId;
    std::string occluderId;
    int layerId = 0;
    int occluderDepthMm = 0;
    int occluderToReceiverMm = 0;
    Vec3 landing{};
    Vec3 axis{};
    bool crossed = false;
    int leftEdgeWidthMm = 0;
    int rightEdgeWidthMm = 0;
    int transitionWidthMm = 0;
    std::vector<ShadowProfilePoint> points;
};

struct ShadowLanding {
    std::string lampId;
    std::string occluderId;
    int vertexIndex = 0;
    int layerId = 0;
    Vec3 point{};
};

struct ShadowReportData {
    int samplesPerAxis = 0;
    int samplesPerLamp = 0;
    int probeSamples = 0;
    int probeHalfSpanMm = 0;
    std::string fingerprint;
    std::vector<ShadowProfile> profiles;
    std::vector<ShadowLanding> landings;
};

ShadowReportData buildShadowReport(const Scene& scene, const ShadowSampler& sampler);
std::string shadowLandingsCsv(const ShadowReportData& data);

}  // namespace caisson

