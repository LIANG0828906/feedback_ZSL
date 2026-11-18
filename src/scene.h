#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "math3d.h"
#include "util.h"

namespace caisson {

struct Mesh {
    std::vector<Vec3> positions;
    std::vector<Vec2> uvs;
    std::vector<std::uint32_t> indices;
    int columns = 0;
    int rows = 0;
};

struct QuadrantMesh {
    int quadrant = 0;
    Mesh mesh;
};

struct Layer {
    int id = 0;
    Vec3 woodColor{0.7, 0.3, 0.18};
    double transmittance = 0.5;
    double reflectance = 0.1;
    double latticeWidth = 0.07;
    std::vector<QuadrantMesh> quadrants;
    Vec3 planePoint{};
    Vec3 planeNormal{0.0, 1.0, 0.0};
};

struct Lamp {
    std::string id;
    Vec3 position{};
    Vec3 color{1.0, 0.72, 0.40};
    double intensity = 4.0;
    double radius = 0.08;
};

struct Occluder {
    std::string id;
    Mesh mesh;
    Vec3 color{0.23, 0.12, 0.06};
    Vec3 centroid{};
};

struct Camera {
    Vec3 position{};
    Vec3 target{};
    Vec3 up{0.0, 1.0, 0.0};
    double verticalFovDegrees = 50.0;
    double nearZ = 0.05;
    double farZ = 100.0;

    Vec3 forward{};
    Vec3 right{};
    Vec3 correctedUp{};
};

struct ImageConfig {
    int width = 640;
    int height = 480;
    int tileSize = 40;
    int aaSamples = 2;
    Vec3 background{0.018, 0.013, 0.010};
};

struct ShadowConfig {
    int samplesPerAxis = 4;
    int probeSamples = 33;
    double probeHalfSpan = 0.65;
};

struct Scene {
    int schemaVersion = 1;
    std::int64_t seed = 0;
    ImageConfig image;
    Camera camera;
    std::vector<Layer> layers;
    std::vector<Lamp> lamps;
    std::vector<Occluder> occluders;
    ShadowConfig shadow;
    Vec3 ambient{0.025, 0.020, 0.014};
    std::string digest;

    int blockColumns() const {
        return (image.width + image.tileSize - 1) / image.tileSize;
    }
    int blockRows() const {
        return (image.height + image.tileSize - 1) / image.tileSize;
    }
    int blockCount() const { return blockColumns() * blockRows(); }
};

bool loadScene(
    const std::filesystem::path& path,
    Scene& scene,
    StructuredError& error);

void validateSceneInvariants(const Scene& scene, StructuredError& error);
std::string computeSceneDigest(const Scene& scene);

}  // namespace caisson

