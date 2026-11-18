#pragma once

#include "scene.hpp"
#include "util.hpp"

#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace sr {

struct TileRect {
    int index = 0;
    int x = 0;
    int y = 0;
    int width = 0;
    int height = 0;
};

struct TileStats {
    uint64_t visibleFragments = 0;
    uint64_t transmittanceSamples = 0;
    double transmittanceSum = 0.0;
    std::map<uint32_t, uint64_t> shadowSamplePoints;
};

struct TileResult {
    TileRect rect;
    std::vector<uint8_t> finalRgba;
    std::vector<uint8_t> smokeRgba;
    std::vector<uint32_t> ownership;
    TileStats stats;
};

struct ShadowFootprint {
    uint32_t particleId = 0;
    int layer = 0;
    uint32_t lightId = 0;
    bool validProjection = false;
    bool insideGround = false;
    bool active = false;
    Vec2 projectedCenterUv;
    Vec3 projectedCenterWorld;
    std::vector<Vec2> hull;
    double alpha = 0.0;
    double groundDistance = 0.0;
    double lightDistance = 0.0;
    double transitionWidth = 0.0;
};

struct LightProbeContribution {
    uint32_t lightId = 0;
    double linearRadiance = 0.0;
    double visibility = 0.0;
    double share = 0.0;
};

struct ProbeEvaluation {
    std::string id;
    uint32_t lightId = 0;
    Vec2 groundUv;
    Vec3 world;
    bool insideGround = false;
    double linearLuminance = 0.0;
    double mappedLuminance = 0.0;
    double totalLightLinear = 0.0;
    double shareSum = 0.0;
    double superpositionExcess = 0.0;
    std::vector<LightProbeContribution> contributions;
};

struct EdgeProfileSample {
    double offset = 0.0;
    double normalizedOffset = 0.0;
    double coverage = 0.0;
    double linearLuminance = 0.0;
};

struct EdgeProfile {
    uint32_t lightId = 0;
    uint32_t particleId = 0;
    int layer = 0;
    double transitionWidth = 0.0;
    std::vector<EdgeProfileSample> samples;
};

class Renderer {
public:
    Renderer(const Scene& scene, int frame);

    TileRect tileRect(int tileIndex) const;
    int tileCount() const { return tileRows_ * tileCols_; }
    int tileColumns() const { return tileCols_; }
    int tileRows() const { return tileRows_; }

    TileResult renderTile(int tileIndex) const;

    const std::vector<ShadowFootprint>& footprints() const { return footprints_; }
    std::vector<ProbeEvaluation> evaluateProbes() const;
    std::vector<EdgeProfile> edgeProfiles() const;
    Vec3 toneMap(Vec3 linear) const;

    int frame() const { return frame_; }
    const CameraSample& camera() const { return camera_; }
    std::string renderDigest() const { return renderDigest_; }

private:
    struct CameraBasis {
        Vec3 origin;
        Vec3 forward;
        Vec3 right;
        Vec3 up;
        double tanHalfFov = 1.0;
        double aspect = 1.0;
    };

    struct OwnerScore {
        uint32_t id = 0;
        double weight = 0.0;
    };

    struct RayResult {
        Vec3 finalLinear;
        Vec3 smokePremul;
        double smokeAlpha = 0.0;
        bool hitGround = false;
        uint64_t visibleFragments = 0;
        double transmittanceSum = 0.0;
        std::vector<OwnerScore> ownerScores;
    };

    struct GroundShade {
        Vec3 linear;
        Vec3 lightLinear;
        Vec3 ambientLinear;
        std::vector<LightProbeContribution> contributions;
    };

    Vec3 rayDirection(double pixelX, double pixelY, double subX, double subY) const;
    Vec3 backgroundForRay(Vec3 direction, bool& hitGround) const;
    GroundShade shadeGround(Vec3 point) const;
    double shadowVisibility(Vec3 groundPoint, const PointLight& light,
                            const std::vector<const ShadowFootprint*>& lightFootprints) const;
    RayResult traceRay(Vec3 origin, Vec3 direction, bool collectOwner) const;
    void composeParticle(const Particle& particle, double cosIncident, Vec3& smoke,
                         double& alpha, std::vector<OwnerScore>& scores,
                         bool collectOwner) const;
    void buildFootprint(const Particle& particle, const PointLight& light);
    void buildSoftLut();

    static bool intersectParticle(const Particle& particle, Vec3 origin, Vec3 direction,
                                  double& t, double& localX, double& localY,
                                  double& cosIncident);
    static bool projectPointToGround(const Ground& ground, const PointLight& light, Vec3 point,
                                     Vec3& projected);
    static std::vector<Vec2> convexHull(std::vector<Vec2> points);
    static double signedDistanceToHull(const std::vector<Vec2>& hull, Vec2 p);
    static double distanceToSegment(Vec2 p, Vec2 a, Vec2 b);
    static Vec3 applyToneMap(Vec3 linear, double exposure);

    double sampleSoftCoverage(double normalized) const;
    double footprintCoverage(const ShadowFootprint& footprint, Vec2 uv) const;
    double lightDistanceForParticle(const Particle& particle, const PointLight& light) const;
    double groundDistanceForParticle(const Particle& particle) const;
    void addOwnerScore(std::vector<OwnerScore>& scores, uint32_t id, double weight) const;
    uint32_t bestOwner(const std::vector<OwnerScore>& scores) const;

    const Scene& scene_;
    int frame_ = 0;
    CameraSample camera_;
    CameraBasis cameraBasis_;
    int tileCols_ = 0;
    int tileRows_ = 0;
    std::vector<ShadowFootprint> footprints_;
    std::vector<std::vector<const ShadowFootprint*>> footprintsByLight_;
    std::vector<double> softLut_;
    std::string renderDigest_;
};

}  // namespace sr

