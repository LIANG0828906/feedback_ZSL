#pragma once

#include "json.hpp"
#include "util.hpp"

#include <array>
#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

namespace sr {

struct ImageSpec {
    int width = 0;
    int height = 0;
    int tileWidth = 0;
    int tileHeight = 0;
    int pixelSamples = 2;
};

struct Ground {
    Vec3 center;
    Vec3 normal{0.0, 1.0, 0.0};
    Vec3 uAxis{1.0, 0.0, 0.0};
    Vec3 vAxis{0.0, 0.0, -1.0};
    double halfU = 0.0;
    double halfV = 0.0;
    Vec3 albedo{0.2, 0.2, 0.2};
};

struct Particle {
    uint32_t id = 0;
    int layer = 0;
    Vec3 center;
    double width = 0.0;
    double height = 0.0;
    double orientation = 0.0;
    double transmittance = 1.0;
    Vec3 color{0.6, 0.6, 0.62};
    Vec3 normal;
    Vec3 right;
    Vec3 up{0.0, 1.0, 0.0};
    std::array<Vec3, 4> corners{};
};

struct PointLight {
    uint32_t id = 0;
    Vec3 position;
    Vec3 color{1.0, 1.0, 1.0};
    double intensity = 1.0;
    double radius = 0.0;
};

struct CameraSample {
    double timestamp = 0.0;
    Vec3 position;
    Vec3 target;
    Vec3 up{0.0, 1.0, 0.0};
    double fovDegrees = 45.0;
};

struct ShadowProbe {
    std::string id;
    uint32_t lightId = 0;
    Vec2 groundUv;
};

struct RenderSettings {
    int layerCount = 0;
    int defaultFrame = 0;
    int shadowSamples = 9;
    double softnessScale = 0.04;
    double exposure = 1.0;
    Vec3 ambient{0.015, 0.018, 0.025};
    std::vector<ShadowProbe> probes;
};

struct Scene {
    ImageSpec image;
    RenderSettings render;
    Ground ground;
    std::vector<Particle> particles;
    std::vector<PointLight> lights;
    std::vector<CameraSample> cameras;
    std::vector<uint8_t> rawBytes;
    std::array<uint8_t, 32> digest{};
};

struct ElementLocation {
    std::string collection;
    int index = -1;
    std::string field;
};

struct ValidationError {
    std::string code;
    std::string path;
    std::string message;
    ElementLocation element;
};

bool loadScene(const std::filesystem::path& path, Scene& scene,
               std::vector<ValidationError>& errors);
bool validateSelectedFrame(const Scene& scene, int frame, std::vector<ValidationError>& errors);
int effectiveFrame(const Scene& scene, int overrideFrame);
const CameraSample& selectedCamera(const Scene& scene, int frame);

Vec3 groundPoint(const Ground& ground, Vec2 uv);
Vec2 groundCoordinates(const Ground& ground, Vec3 world, bool* onPlane = nullptr);
bool insideGround(const Ground& ground, Vec2 uv, double margin = 0.0);

std::string makeDeterministicSceneJson();
Json makeErrorReport(const std::vector<ValidationError>& errors,
                     const std::string& source = std::string());

}  // namespace sr

