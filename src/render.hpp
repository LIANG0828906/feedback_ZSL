#pragma once

#include "scene.hpp"

#include <array>
#include <cstdint>
#include <string>
#include <vector>

namespace sl {

struct BlockRect {
    int id = 0;
    int x = 0;
    int y = 0;
    int width = 0;
    int height = 0;
};

struct LayerMetric {
    int layerIndex = 0;
    std::string separationId;
    double transmitted = 0.0;
    double reflected = 0.0;
    double coverage = 0.0;
    double samples = 0.0;
};

struct BlockResult {
    BlockRect rect;
    std::vector<std::uint8_t> rgba;
    std::vector<std::uint8_t> owner;
    std::vector<LayerMetric> metrics;
    std::string rgbaDigest;
    std::string ownerDigest;
};

struct MoirePairReport {
    int layerA = 0;
    int layerB = 0;
    std::string separationA;
    std::string separationB;
    double angleA = 0.0;
    double angleB = 0.0;
    double angleDifference = 0.0;
    double threshold = 0.0;
    double weight = 0.0;
    bool triggered = false;
    std::array<double, 2> amplitudeBeforeByLight{0.0, 0.0};
    std::array<double, 2> amplitudeAfterByLight{0.0, 0.0};
    double amplitudeBefore = 0.0;
    double amplitudeAfter = 0.0;
    double reductionPercent = 0.0;
    double dotCoverageA = 0.0;
    double dotCoverageB = 0.0;
};

struct ProjectionPoint {
    int layerIndex = 0;
    std::string separationId;
    int lightIndex = 0;
    Vec2 holeUV;
    Vec3 holeWorld;
    Vec3 boardPoint;
    Vec2 boardUV;
    bool insideBackboard = false;
    int adjacentLayer = 0;
    bool adjacentHit = false;
    Vec3 adjacentPoint;
    Vec2 adjacentUV;
};

struct CameraBasis {
    Vec3 position;
    Vec3 forward;
    Vec3 right;
    Vec3 up;
    double tanHalfFov = 0.0;
    double aspect = 1.0;
};

class Renderer {
public:
    explicit Renderer(const Scene& scene);

    const Scene& scene() const { return scene_; }
    const CameraBasis& cameraBasis() const { return camera_; }
    int blockCount() const { return static_cast<int>(blocks_.size()); }
    const std::vector<BlockRect>& blocks() const { return blocks_; }
    BlockRect blockRect(int id) const;
    BlockResult renderBlock(int id) const;

    const std::vector<MoirePairReport>& moirePairs() const { return moirePairs_; }
    const std::vector<ProjectionPoint>& projectionPoints() const { return projections_; }

    sj::Value projectionPointsJson() const;
    sj::Value baseReport() const;
    sj::Value metricsJson(const std::vector<LayerMetric>& metrics) const;
    static std::vector<LayerMetric> combineMetrics(const std::vector<LayerMetric>& a,
                                                   const std::vector<LayerMetric>& b);

    static std::string rgbaDigest(const std::vector<std::uint8_t>& bytes);

private:
    const Scene& scene_;
    CameraBasis camera_;
    std::vector<BlockRect> blocks_;
    std::vector<MoirePairReport> moirePairs_;
    std::vector<ProjectionPoint> projections_;
    std::vector<double> pairMeanApertureA_;
    std::vector<double> pairMeanApertureB_;

    struct Hit {
        bool valid = false;
        int layerSlot = -1;
        double t = 0.0;
        Vec3 point;
        Vec3 normal;
    };

    Ray pixelRay(int x, int y) const;
    Vec3 illuminateBoard(const Vec3& point) const;
    Vec3 transmissionAlongRay(const Vec3& origin, const Vec3& target) const;
    double layerAperture(const ScreenLayer& layer,
                         const Vec3& point,
                         double tau,
                         double filterWeight) const;
    bool meshContains(const ScreenLayer& layer, const Vec3& point) const;
    double directLight(const Vec3& point, const Vec3& normal, Vec3& tint) const;
    double filterWeightFor(const ScreenLayer& layer,
                           const Hit& hit,
                           int hitCount) const;
    Vec3 layerFilterColor(const ScreenLayer& layer, double edge) const;
    void renderPixel(int globalX,
                     int globalY,
                     int localX,
                     int localY,
                     std::vector<std::uint8_t>& rgba,
                     std::vector<std::uint8_t>& owner,
                     std::vector<LayerMetric>& metrics,
                     int localWidth) const;

    void buildBlocks();
    void buildMoirePairs();
    void buildProjectionPoints();
    double pairAmplitude(int layerA,
                         int layerB,
                         int lightIndex,
                         bool applyMask,
                         double weight) const;
};

void writeRaw(const std::string& path, const std::vector<std::uint8_t>& bytes);

} // namespace sl

