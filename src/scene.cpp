#include "shadowpipe/scene.hpp"

#include <algorithm>
#include <cstring>
#include <fstream>
#include <limits>
#include <sstream>
#include <utility>

#include "shadowpipe/hash.hpp"

namespace shadowpipe {
namespace {

constexpr std::uint32_t kMaxMaterials = 65536u;
constexpr std::uint32_t kMaxMeshes = 65536u;
constexpr std::uint32_t kMaxNodes = 100000u;
constexpr std::uint32_t kMaxVertices = 10000000u;
constexpr std::uint32_t kMaxIndices = 30000000u;
constexpr std::uint32_t kMaxNameLength = 1024u;

class Reader {
public:
    Reader(const std::vector<std::uint8_t>& data) : data_(data) {}

    bool readBytes(void* destination, std::size_t count) {
        if (count > data_.size() - offset_) {
            return false;
        }
        if (count != 0) {
            std::memcpy(destination, data_.data() + offset_, count);
        }
        offset_ += count;
        return true;
    }

    bool u32(std::uint32_t& value) { return readBytes(&value, sizeof(value)); }
    bool i32(std::int32_t& value) { return readBytes(&value, sizeof(value)); }
    bool f32(float& value) { return readBytes(&value, sizeof(value)); }

    bool vec3(Vec3& value) {
        return f32(value.x) && f32(value.y) && f32(value.z);
    }

    bool mat4(Mat4& value) {
        for (int column = 0; column < 4; ++column) {
            for (int row = 0; row < 4; ++row) {
                if (!f32(value[column][row])) {
                    return false;
                }
            }
        }
        return true;
    }

    std::size_t remaining() const { return data_.size() - offset_; }

private:
    const std::vector<std::uint8_t>& data_;
    std::size_t offset_ = 0;
};

std::int64_t firstNodeUsingMesh(const Scene& scene, std::uint32_t mesh_index) {
    for (std::size_t i = 0; i < scene.nodes.size(); ++i) {
        if (scene.nodes[i].mesh_index == mesh_index) {
            return static_cast<std::int64_t>(i);
        }
    }
    return -1;
}

float containmentEpsilon(const AABB& box) {
    const Vec3 extent = box.extent();
    const float scale = std::max({1.0f, std::abs(box.min.x), std::abs(box.min.y),
                                  std::abs(box.min.z), std::abs(box.max.x),
                                  std::abs(box.max.y), std::abs(box.max.z),
                                  extent.x, extent.y, extent.z});
    return scale * 2.0e-5f;
}

Status invalidScene(std::string message, std::int64_t node_index = -1,
                    const std::string& node_name = {}) {
    return Status::errorAt(ErrorCode::SceneInvalid, std::move(message), node_index,
                           node_name);
}

Status validateParsedScene(Scene& scene) {
    if (scene.materials.empty()) {
        return invalidScene("scene has no materials");
    }

    for (std::size_t mesh_index = 0; mesh_index < scene.meshes.size(); ++mesh_index) {
        Mesh& mesh = scene.meshes[mesh_index];
        const std::int64_t owner = firstNodeUsingMesh(scene, static_cast<std::uint32_t>(mesh_index));
        const std::string owner_name =
            owner >= 0 ? scene.nodes[static_cast<std::size_t>(owner)].name : std::string();
        if (mesh.material_slot >= scene.materials.size()) {
            return invalidScene("mesh " + std::to_string(mesh_index) +
                                    " references material slot " +
                                    std::to_string(mesh.material_slot) +
                                    " outside the material table",
                                owner, owner_name);
        }
        if (mesh.positions.empty() || mesh.indices.empty()) {
            return invalidScene("mesh " + std::to_string(mesh_index) + " is empty", owner,
                                owner_name);
        }
        if (mesh.indices.size() % 3u != 0u) {
            return invalidScene("mesh " + std::to_string(mesh_index) +
                                    " index count is not a multiple of three",
                                owner, owner_name);
        }
        if (!mesh.local_bounds.valid()) {
            return invalidScene("mesh " + std::to_string(mesh_index) +
                                    " has an invalid local AABB",
                                owner, owner_name);
        }
        const float mesh_epsilon = containmentEpsilon(mesh.local_bounds);
        for (std::size_t i = 0; i < mesh.positions.size(); ++i) {
            if (!finiteVec(mesh.positions[i])) {
                return invalidScene("mesh " + std::to_string(mesh_index) +
                                        " contains a non-finite vertex",
                                    owner, owner_name);
            }
            if (!mesh.local_bounds.contains(mesh.positions[i], mesh_epsilon)) {
                return invalidScene("mesh " + std::to_string(mesh_index) + " vertex " +
                                        std::to_string(i) +
                                        " lies outside its declared local AABB",
                                    owner, owner_name);
            }
        }
        for (std::size_t i = 0; i < mesh.indices.size(); ++i) {
            if (mesh.indices[i] >= mesh.positions.size()) {
                return invalidScene("mesh " + std::to_string(mesh_index) + " index " +
                                        std::to_string(i) + " has value " +
                                        std::to_string(mesh.indices[i]) +
                                        " outside vertex range [0, " +
                                        std::to_string(mesh.positions.size()) + ")",
                                    owner, owner_name);
            }
        }
    }

    std::vector<std::uint8_t> state(scene.nodes.size(), 0);
    std::vector<std::uint32_t> order;
    order.reserve(scene.nodes.size());
    std::vector<std::uint32_t> roots;

    for (std::size_t i = 0; i < scene.nodes.size(); ++i) {
        Node& node = scene.nodes[i];
        if (node.name.empty()) {
            return invalidScene("node " + std::to_string(i) + " has an empty name",
                                static_cast<std::int64_t>(i));
        }
        if (!node.declared_bounds.valid()) {
            return invalidScene("node '" + node.name + "' has an invalid declared AABB",
                                static_cast<std::int64_t>(i), node.name);
        }
        if (node.mesh_index != kNoMesh && node.mesh_index >= scene.meshes.size()) {
            return invalidScene("node '" + node.name + "' references mesh " +
                                    std::to_string(node.mesh_index) +
                                    " outside the mesh table",
                                static_cast<std::int64_t>(i), node.name);
        }
        if (!finiteMat(node.world)) {
            return invalidScene("node '" + node.name + "' has a non-finite world matrix",
                                static_cast<std::int64_t>(i), node.name);
        }
        const float determinant = glm::determinant(node.world);
        if (!std::isfinite(determinant) || std::abs(determinant) < 1.0e-12f) {
            return invalidScene("node '" + node.name +
                                    "' has a singular/non-invertible world matrix",
                                static_cast<std::int64_t>(i), node.name);
        }
        node.inverse_world = glm::inverse(node.world);
        if (!finiteMat(node.inverse_world)) {
            return invalidScene("node '" + node.name +
                                    "' inverse world matrix is not finite",
                                static_cast<std::int64_t>(i), node.name);
        }

        if (node.parent < -1 ||
            (node.parent >= 0 &&
             static_cast<std::size_t>(node.parent) >= scene.nodes.size())) {
            return invalidScene("node '" + node.name + "' has invalid parent index " +
                                    std::to_string(node.parent),
                                static_cast<std::int64_t>(i), node.name);
        }
        if (node.parent == static_cast<std::int32_t>(i)) {
            return invalidScene("node '" + node.name + "' is its own parent",
                                static_cast<std::int64_t>(i), node.name);
        }
        if (node.parent < 0) {
            roots.push_back(static_cast<std::uint32_t>(i));
        } else {
            scene.nodes[static_cast<std::size_t>(node.parent)].children.push_back(
                static_cast<std::uint32_t>(i));
        }
    }

    if (roots.empty()) {
        return invalidScene("scene hierarchy has no root node");
    }

    // Iterative post-order traversal; state 1 in the stack denotes "children visited".
    std::vector<std::pair<std::uint32_t, bool>> stack;
    for (const std::uint32_t root : roots) {
        stack.emplace_back(root, false);
        while (!stack.empty()) {
            const auto [node_index, expanded] = stack.back();
            stack.pop_back();
            if (expanded) {
                state[node_index] = 2;
                order.push_back(node_index);
                continue;
            }
            if (state[node_index] == 1) {
                return invalidScene("cycle detected in hierarchy at node " +
                                        std::to_string(node_index),
                                    node_index, scene.nodes[node_index].name);
            }
            if (state[node_index] == 2) {
                continue;
            }
            state[node_index] = 1;
            stack.emplace_back(node_index, true);
            const auto& children = scene.nodes[node_index].children;
            for (auto it = children.rbegin(); it != children.rend(); ++it) {
                if (state[*it] == 1) {
                    return invalidScene("cycle detected in hierarchy between nodes " +
                                            std::to_string(node_index) + " and " +
                                            std::to_string(*it),
                                        node_index, scene.nodes[node_index].name);
                }
                stack.emplace_back(*it, false);
            }
        }
    }
    if (order.size() != scene.nodes.size()) {
        return invalidScene("scene hierarchy does not reach every node");
    }

    for (const std::uint32_t node_index : order) {
        Node& node = scene.nodes[node_index];
        AABB content;
        if (node.mesh_index != kNoMesh) {
            content = transformAABB(scene.meshes[node.mesh_index].local_bounds,
                                    node.world);
        }
        for (const std::uint32_t child_index : node.children) {
            const Node& child = scene.nodes[child_index];
            const AABB child_bounds =
                child.content_bounds.valid() ? child.content_bounds : child.declared_bounds;
            content.expand(child_bounds);
        }
        node.content_bounds = content;

        if (content.valid()) {
            const float epsilon =
                std::max(containmentEpsilon(node.declared_bounds),
                         containmentEpsilon(content));
            if (!node.declared_bounds.contains(content, epsilon)) {
                std::ostringstream message;
                message << "node '" << node.name
                        << "' declared AABB does not contain its mesh/child content";
                return invalidScene(message.str(), static_cast<std::int64_t>(node_index),
                                    node.name);
            }
        }

        for (const std::uint32_t child_index : node.children) {
            const Node& child = scene.nodes[child_index];
            const AABB child_bounds =
                child.content_bounds.valid() ? child.content_bounds : child.declared_bounds;
            const float epsilon =
                std::max(containmentEpsilon(node.declared_bounds),
                         containmentEpsilon(child_bounds));
            if (!node.declared_bounds.contains(child_bounds, epsilon)) {
                return invalidScene("parent node '" + node.name +
                                        "' does not contain child node '" + child.name +
                                        "' (child index " +
                                        std::to_string(child_index) + ")",
                                    static_cast<std::int64_t>(node_index), node.name);
            }
            const float declared_epsilon =
                std::max(containmentEpsilon(node.declared_bounds),
                         containmentEpsilon(child.declared_bounds));
            if (!node.declared_bounds.contains(child.declared_bounds,
                                               declared_epsilon)) {
                return invalidScene("parent node '" + node.name +
                                        "' declared AABB does not contain child '" +
                                        child.name + "' declared AABB",
                                    static_cast<std::int64_t>(node_index), node.name);
            }
        }
    }
    return Status::success();
}

}  // namespace

SceneLoadResult loadSceneFromFile(const std::string& path) {
    std::ifstream in(path, std::ios::binary | std::ios::ate);
    if (!in) {
        return {Status::error(ErrorCode::IoError, "cannot open scene file: " + path), {}};
    }
    const std::streamoff size = in.tellg();
    if (size < 0 || size > 512LL * 1024LL * 1024LL) {
        return {Status::error(ErrorCode::SceneInvalid,
                              "scene file is empty, unreadable, or exceeds 512 MiB"),
                {}};
    }
    in.seekg(0, std::ios::beg);
    std::vector<std::uint8_t> bytes(static_cast<std::size_t>(size));
    if (!bytes.empty()) {
        in.read(reinterpret_cast<char*>(bytes.data()), size);
        if (!in) {
            return {Status::error(ErrorCode::IoError,
                                  "failed while reading scene file: " + path),
                    {}};
        }
    }
    return parseSceneBytes(bytes);
}

SceneLoadResult parseSceneBytes(const std::vector<std::uint8_t>& bytes) {
    Reader reader(bytes);
    char magic[4] = {};
    std::uint32_t version = 0;
    std::uint32_t material_count = 0;
    std::uint32_t mesh_count = 0;
    std::uint32_t node_count = 0;

    if (!reader.readBytes(magic, sizeof(magic)) ||
        std::memcmp(magic, "RSCN", sizeof(magic)) != 0) {
        return {invalidScene("invalid scene magic; expected RSCN"), {}};
    }
    if (!reader.u32(version) || version != kSceneFormatVersion) {
        return {invalidScene("unsupported scene format version"), {}};
    }
    if (!reader.u32(material_count) || !reader.u32(mesh_count) ||
        !reader.u32(node_count)) {
        return {invalidScene("truncated scene header"), {}};
    }
    if (material_count == 0 || material_count > kMaxMaterials || mesh_count > kMaxMeshes ||
        node_count == 0 || node_count > kMaxNodes) {
        return {invalidScene("scene table count is outside the supported range"), {}};
    }

    Scene scene;
    scene.materials.resize(material_count);
    for (std::uint32_t i = 0; i < material_count; ++i) {
        Material material;
        if (!reader.vec3(material.albedo) || !reader.f32(material.roughness) ||
            !finiteVec(material.albedo) || !std::isfinite(material.roughness)) {
            return {invalidScene("truncated or non-finite material record " +
                                 std::to_string(i)),
                    {}};
        }
        scene.materials[i] = material;
    }

    scene.meshes.resize(mesh_count);
    for (std::uint32_t mesh_index = 0; mesh_index < mesh_count; ++mesh_index) {
        Mesh& mesh = scene.meshes[mesh_index];
        std::uint32_t vertex_count = 0;
        std::uint32_t index_count = 0;
        Vec3 bounds_min;
        Vec3 bounds_max;
        if (!reader.u32(mesh.material_slot) || !reader.u32(vertex_count) ||
            !reader.u32(index_count) || !reader.vec3(bounds_min) ||
            !reader.vec3(bounds_max)) {
            return {invalidScene("truncated mesh header for mesh " +
                                 std::to_string(mesh_index)),
                    {}};
        }
        if (vertex_count == 0 || vertex_count > kMaxVertices || index_count == 0 ||
            index_count > kMaxIndices) {
            return {invalidScene("mesh " + std::to_string(mesh_index) +
                                 " count is outside the supported range"),
                    {}};
        }
        const std::uint64_t vertex_bytes = static_cast<std::uint64_t>(vertex_count) * 12ULL;
        const std::uint64_t index_bytes = static_cast<std::uint64_t>(index_count) * 4ULL;
        if (vertex_bytes + index_bytes > reader.remaining()) {
            return {invalidScene("mesh " + std::to_string(mesh_index) +
                                 " payload is truncated"),
                    {}};
        }
        mesh.local_bounds.min = bounds_min;
        mesh.local_bounds.max = bounds_max;
        mesh.positions.resize(vertex_count);
        for (Vec3& position : mesh.positions) {
            if (!reader.vec3(position)) {
                return {invalidScene("truncated vertex data in mesh " +
                                     std::to_string(mesh_index)),
                        {}};
            }
        }
        mesh.indices.resize(index_count);
        for (std::uint32_t& index : mesh.indices) {
            if (!reader.u32(index)) {
                return {invalidScene("truncated index data in mesh " +
                                     std::to_string(mesh_index)),
                        {}};
            }
        }
    }

    scene.nodes.resize(node_count);
    for (std::uint32_t node_index = 0; node_index < node_count; ++node_index) {
        Node& node = scene.nodes[node_index];
        std::uint32_t name_length = 0;
        if (!reader.u32(name_length) || name_length == 0 ||
            name_length > kMaxNameLength || name_length > reader.remaining()) {
            return {invalidScene("invalid name length for node " +
                                 std::to_string(node_index)),
                    {}};
        }
        node.name.resize(name_length);
        if (!reader.readBytes(node.name.data(), name_length)) {
            return {invalidScene("truncated node name for node " +
                                 std::to_string(node_index)),
                    {}};
        }
        if (!reader.i32(node.parent) || !reader.u32(node.mesh_index) ||
            !reader.mat4(node.world)) {
            return {invalidScene("truncated transform record for node '" + node.name + "'",
                                 node_index, node.name),
                    {}};
        }
        if (!reader.vec3(node.declared_bounds.min) ||
            !reader.vec3(node.declared_bounds.max)) {
            return {invalidScene("truncated AABB record for node '" + node.name + "'",
                                 node_index, node.name),
                    {}};
        }
    }

    if (reader.remaining() != 0) {
        return {invalidScene("unexpected trailing bytes after scene records"), {}};
    }

    Status status = validateParsedScene(scene);
    if (!status.ok()) {
        return {std::move(status), {}};
    }
    scene.content_hash = computeSceneHash(scene);
    return {Status::success(), std::move(scene)};
}

std::uint64_t computeSceneHash(const Scene& scene) {
    Hasher hasher;
    hasher.u32(kSceneFormatVersion);
    hasher.u64(scene.materials.size());
    for (const Material& material : scene.materials) {
        hasher.f32(material.albedo.x);
        hasher.f32(material.albedo.y);
        hasher.f32(material.albedo.z);
        hasher.f32(material.roughness);
    }
    hasher.u64(scene.meshes.size());
    for (const Mesh& mesh : scene.meshes) {
        hasher.u32(mesh.material_slot);
        hasher.u64(mesh.positions.size());
        for (const Vec3& position : mesh.positions) {
            hasher.f32(position.x);
            hasher.f32(position.y);
            hasher.f32(position.z);
        }
        hasher.u64(mesh.indices.size());
        for (const std::uint32_t index : mesh.indices) {
            hasher.u32(index);
        }
        hasher.f32(mesh.local_bounds.min.x);
        hasher.f32(mesh.local_bounds.min.y);
        hasher.f32(mesh.local_bounds.min.z);
        hasher.f32(mesh.local_bounds.max.x);
        hasher.f32(mesh.local_bounds.max.y);
        hasher.f32(mesh.local_bounds.max.z);
    }
    hasher.u64(scene.nodes.size());
    for (const Node& node : scene.nodes) {
        hasher.string(node.name);
        hasher.i32(node.parent);
        hasher.u32(node.mesh_index);
        for (int column = 0; column < 4; ++column) {
            for (int row = 0; row < 4; ++row) {
                hasher.f32(node.world[column][row]);
            }
        }
        hasher.f32(node.declared_bounds.min.x);
        hasher.f32(node.declared_bounds.min.y);
        hasher.f32(node.declared_bounds.min.z);
        hasher.f32(node.declared_bounds.max.x);
        hasher.f32(node.declared_bounds.max.y);
        hasher.f32(node.declared_bounds.max.z);
    }
    return hasher.value();
}

std::vector<WorldTriangle> buildWorldTriangles(const Scene& scene) {
    std::vector<WorldTriangle> triangles;
    for (std::size_t node_index = 0; node_index < scene.nodes.size(); ++node_index) {
        const Node& node = scene.nodes[node_index];
        if (node.mesh_index == kNoMesh) {
            continue;
        }
        const Mesh& mesh = scene.meshes[node.mesh_index];
        triangles.reserve(triangles.size() + mesh.indices.size() / 3u);
        for (std::size_t i = 0; i + 2 < mesh.indices.size(); i += 3) {
            WorldTriangle triangle;
            const Vec3 p0 = Vec3(node.world * Vec4(mesh.positions[mesh.indices[i]], 1.0f));
            const Vec3 p1 =
                Vec3(node.world * Vec4(mesh.positions[mesh.indices[i + 1]], 1.0f));
            const Vec3 p2 =
                Vec3(node.world * Vec4(mesh.positions[mesh.indices[i + 2]], 1.0f));
            const Vec3 cross = glm::cross(p1 - p0, p2 - p0);
            const float length = glm::length(cross);
            triangle.p0 = p0;
            triangle.p1 = p1;
            triangle.p2 = p2;
            triangle.normal = length > 1.0e-12f ? cross / length : Vec3(0.0f);
            triangle.node_index = static_cast<std::uint32_t>(node_index);
            triangle.triangle_index = static_cast<std::uint32_t>(i / 3u);
            triangles.push_back(triangle);
        }
    }
    return triangles;
}

}  // namespace shadowpipe

