#pragma once

#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "shadowpipe/error.hpp"
#include "shadowpipe/math.hpp"

namespace shadowpipe {

constexpr std::uint32_t kNoMesh = 0xffffffffu;
constexpr std::uint32_t kSceneFormatVersion = 1;

struct Material {
    Vec3 albedo{0.8f};
    float roughness = 0.5f;
};

struct Mesh {
    std::vector<Vec3> positions;
    std::vector<std::uint32_t> indices;
    std::uint32_t material_slot = 0;
    AABB local_bounds;
};

struct Node {
    std::string name;
    std::int32_t parent = -1;
    std::uint32_t mesh_index = kNoMesh;
    Mat4 world{1.0f};
    Mat4 inverse_world{1.0f};
    AABB declared_bounds;
    AABB content_bounds;
    std::vector<std::uint32_t> children;
};

struct Scene {
    std::vector<Material> materials;
    std::vector<Mesh> meshes;
    std::vector<Node> nodes;
    std::uint64_t content_hash = 0;
};

struct WorldTriangle {
    Vec3 p0;
    Vec3 p1;
    Vec3 p2;
    Vec3 normal;
    std::uint32_t node_index = 0;
    std::uint32_t triangle_index = 0;
};

struct SceneLoadResult {
    Status status;
    Scene scene;
};

SceneLoadResult loadSceneFromFile(const std::string& path);
SceneLoadResult parseSceneBytes(const std::vector<std::uint8_t>& bytes);
std::uint64_t computeSceneHash(const Scene& scene);
std::vector<WorldTriangle> buildWorldTriangles(const Scene& scene);

}  // namespace shadowpipe

