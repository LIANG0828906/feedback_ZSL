#pragma once

#include "common.hpp"
#include "json.hpp"

#include <array>
#include <cstdint>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

namespace mosaic {

struct Triangle {
    std::array<std::uint32_t, 3> vertices{};
};

struct Mesh {
    std::vector<Vec3> vertices;
    std::vector<Triangle> triangles;
};

struct Piece {
    std::int32_t layerIndex = 0;
    std::uint32_t pieceId = 0;
    Rgb transmission{};
    double thickness = 0.0;
    double edgeIor = 1.0;
    Mesh mesh;
};

struct Light {
    std::uint32_t id = 0;
    Vec3 position{};
    Rgb color{};
    double intensity = 0.0;
};

struct Camera {
    Vec3 position{};
    Vec3 target{};
    Vec3 up{};
    double fovYDeg = 45.0;
    double nearPlane = 0.01;
    double farPlane = 100.0;
};

struct OutputSettings {
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    std::uint32_t chunkWidth = 0;
    std::uint32_t chunkHeight = 0;
};

struct TextureSettings {
    std::uint32_t bubblesPerPiece = 0;
    std::uint32_t flowBandsPerPiece = 0;
};

struct CausticSettings {
    std::uint32_t samplesPerEdge = 0;
    double penumbraBase = 0.0;
    double penumbraThickness = 0.0;
    double penumbraLayerDistance = 0.0;
    double intensity = 0.0;
    double edgeBand = 0.0;
    double splatSupport = 2.5;
};

struct GlareSettings {
    double reflectionScale = 0.0;
    double edgePower = 4.0;
};

struct Settings {
    OutputSettings output;
    TextureSettings texture;
    CausticSettings caustic;
    GlareSettings glare;
    std::array<std::string, 2> compositionOrder{"transmission", "reflection"};
};

struct Scene {
    std::uint64_t seed = 0;
    Camera camera;
    Mesh floor;
    std::vector<Piece> pieces;
    std::vector<Light> lights;
    Settings settings;
};

struct ValidationError {
    std::string code;
    std::string path;
    std::string message;
};

struct SceneParseResult {
    std::optional<Scene> scene;
    std::vector<ValidationError> errors;
    std::optional<JsonParseError> jsonError;
};

SceneParseResult parseScene(std::string_view text);

std::string sceneDigest(const Scene& scene);

std::string validationErrorsJson(const std::vector<ValidationError>& errors);

std::string sceneSummaryJson(const Scene& scene);

double triangleAreaSquaredTimesFour(const Vec3& a, const Vec3& b, const Vec3& c);

bool pointInsideMesh(const Mesh& mesh, const Vec3& point);

}  // namespace mosaic
