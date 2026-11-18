#pragma once

#include "json.h"
#include "math.h"

#include <array>
#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace cr {

struct Warning {
    std::string code;
    std::string message;
    int layer = -1;
    std::string componentId;
    std::string kind;

    json::Value toJson() const;
};

struct Triangle {
    std::array<int, 3> indices{{0, 0, 0}};
    std::array<Vec2, 3> uv{};
};

struct Mesh {
    std::vector<Vec3> vertices;
    std::vector<Triangle> triangles;
};

enum class ComponentType { Board = 0, Bracket = 1 };

struct Board {
    int layer = -1;
    std::string id;
    Mesh mesh;
    std::uint32_t seed = 0;
    double transmittance = 0.0;
    Vec3 color{0.68, 0.43, 0.18};
    int ownerCode = 0;
};

struct Bracket {
    int layer = -1;
    std::string id;
    Mesh mesh;
    double reflectance = 0.0;
    double thickness = 0.0;
    Vec3 color{0.22, 0.25, 0.23};
    int ownerCode = 0;
};

struct Layer {
    int index = -1;
    double z = 0.0;
    double thickness = 0.0;
    std::vector<Board> boards;
    std::vector<Bracket> brackets;
};

struct Light {
    std::string id;
    Vec3 position{};
    Vec3 color{1.0, 1.0, 1.0};
    double intensity = 1.0;
};

struct Receiver {
    double z = 0.0;
    double minX = -1.0;
    double maxX = 1.0;
    double minY = -1.0;
    double maxY = 1.0;
    Vec3 color{0.5, 0.5, 0.5};
};

struct Camera {
    Vec3 position{0.0, 0.0, -1.0};
    Vec3 target{0.0, 0.0, 0.0};
    Vec3 up{0.0, 1.0, 0.0};
    double fovDeg = 55.0;
    double nearZ = 0.02;
    double farZ = 50.0;
};

struct RenderConfig {
    int width = 256;
    int height = 192;
    int tileSize = 32;
    int textureResolution = 64;
    int projectionSamples = 4;
    int shadowMapResolution = 384;
    double penumbraScale = 0.045;
    double exposure = 1.0;
    double ambient = 0.12;
    Vec3 background{0.005, 0.007, 0.009};
};

struct Scene {
    std::string schema;
    std::string name;
    RenderConfig render;
    std::vector<Layer> layers;
    std::vector<Light> lights;
    Receiver receiver;
    Camera camera;
    std::string sourceCanonical;
    std::string digest;
    std::vector<Warning> warnings;

    bool valid() const { return warnings.empty(); }
    int totalBlocks() const;
    int blocksX() const;
    int blocksY() const;
};

struct LoadResult {
    Scene scene;
    bool parsed = false;
    std::string fatalError;
};

LoadResult loadScene(const std::string& path);
LoadResult parseScene(const std::string& text, const std::string& sourceLabel);

json::Value warningsToJson(const std::vector<Warning>& warnings);

}  // namespace cr

