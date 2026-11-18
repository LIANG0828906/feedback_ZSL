#pragma once

#include "halftone.hpp"
#include "json.hpp"
#include "math.hpp"

#include <array>
#include <cstdint>
#include <string>
#include <vector>

namespace sl {

struct Diagnostic {
    std::string code = "E_SCENE_INVALID";
    int layer = 0;
    std::string separationId;
    std::string path;
    std::string message;
};

struct ScreenLayer {
    int layerIndex = 0;
    std::string separationId;
    std::vector<Vec3> vertices;
    std::vector<std::array<int, 3>> triangles;
    std::vector<Triangle> triangleData;
    double inkTransmittance = 0.5;
    Vec3 inkColor{1.0, 1.0, 1.0};
    int meshLineCount = 1;
    double meshAngleDegrees = 0.0;
    std::uint64_t seed = 0;
    double airGapAbove = 0.0;
    Plane plane;
    HalftoneTable halftone;
    double minU = 0.0;
    double maxU = 0.0;
    double minV = 0.0;
    double maxV = 0.0;
    Vec3 boundsMin;
    Vec3 boundsMax;
};

struct StripLight {
    Vec3 a;
    Vec3 b;
    Vec3 color{1.0, 1.0, 1.0};
    double intensity = 0.0;
    Vec3 midpoint;
};

struct ImageConfig {
    int width = 320;
    int height = 240;
    int blockWidth = 32;
    int blockHeight = 32;
    double exposure = 1.0;
};

struct CameraConfig {
    Vec3 position;
    Vec3 target;
    Vec3 up{0.0, 1.0, 0.0};
    double fovYDegrees = 40.0;
    double nearPlane = 0.01;
    double farPlane = 10000.0;
};

struct MoireConfig {
    double thresholdDegrees = 3.0;
    double maskStrength = 0.78;
};

struct Scene {
    int version = 1;
    std::string name;
    ImageConfig image;
    std::vector<ScreenLayer> layers;
    std::vector<double> airGaps;
    std::array<StripLight, 2> lights;
    Plane backboard;
    CameraConfig camera;
    MoireConfig moire;
    std::string sourceCanonical;
    std::string sceneDigest;
    std::string sceneSummaryJson;
};

bool loadScene(const std::string& path, Scene& scene, std::vector<Diagnostic>& diagnostics);
bool parseScene(const sj::Value& root, Scene& scene, std::vector<Diagnostic>& diagnostics);

sj::Value makeSampleScene(std::uint64_t seed = 20261118ULL);
sj::Value diagnosticsJson(const std::vector<Diagnostic>& diagnostics);
void writeJsonFile(const std::string& path, const sj::Value& value, int indent = 2);

double angleDifferenceDegrees(double a, double b);
bool layerContainsPoint(const ScreenLayer& layer, const Vec3& point, double tolerance);
double sampleLayerCoverage(const Scene& scene,
                           const ScreenLayer& layer,
                           const Vec3& point,
                           double filterWeight);

} // namespace sl

