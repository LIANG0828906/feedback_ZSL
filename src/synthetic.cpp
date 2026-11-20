#include "shadowpipe/synthetic.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <string>
#include <utility>
#include <vector>

#include <glm/gtc/matrix_transform.hpp>

namespace shadowpipe {
namespace {

class Writer {
public:
    void bytes(const void* data, std::size_t size) {
        const auto* p = static_cast<const std::uint8_t*>(data);
        data_.insert(data_.end(), p, p + size);
    }

    void u32(std::uint32_t value) { bytes(&value, sizeof(value)); }
    void i32(std::int32_t value) { bytes(&value, sizeof(value)); }
    void f32(float value) { bytes(&value, sizeof(value)); }
    void vec3(const Vec3& value) {
        f32(value.x);
        f32(value.y);
        f32(value.z);
    }
    void mat4(const Mat4& value) {
        for (int column = 0; column < 4; ++column) {
            for (int row = 0; row < 4; ++row) {
                f32(value[column][row]);
            }
        }
    }
    void string(const std::string& value) {
        u32(static_cast<std::uint32_t>(value.size()));
        bytes(value.data(), value.size());
    }

    const std::vector<std::uint8_t>& data() const { return data_; }

private:
    std::vector<std::uint8_t> data_;
};

Mesh makeCubeMesh(std::uint32_t material) {
    Mesh mesh;
    mesh.material_slot = material;
    mesh.positions = {
        {-0.5f, -0.5f, -0.5f}, {0.5f, -0.5f, -0.5f}, {0.5f, 0.5f, -0.5f},
        {-0.5f, 0.5f, -0.5f},  {-0.5f, -0.5f, 0.5f}, {0.5f, -0.5f, 0.5f},
        {0.5f, 0.5f, 0.5f},    {-0.5f, 0.5f, 0.5f},
    };
    mesh.indices = {
        4, 5, 6, 4, 6, 7,  // +Z
        1, 0, 3, 1, 3, 2,  // -Z
        0, 4, 7, 0, 7, 3,  // -X
        5, 1, 2, 5, 2, 6,  // +X
        0, 1, 5, 0, 5, 4,  // -Y
        3, 7, 6, 3, 6, 2,  // +Y
    };
    mesh.local_bounds.min = Vec3(-0.5f);
    mesh.local_bounds.max = Vec3(0.5f);
    return mesh;
}

Mesh makeGroundMesh() {
    Mesh mesh;
    mesh.material_slot = 0;
    mesh.positions = {
        {-14.0f, 0.0f, -14.0f},
        {-14.0f, 0.0f, 14.0f},
        {14.0f, 0.0f, 14.0f},
        {14.0f, 0.0f, -14.0f},
    };
    mesh.indices = {0, 1, 2, 0, 2, 3};
    mesh.local_bounds.min = Vec3(-14.0f, 0.0f, -14.0f);
    mesh.local_bounds.max = Vec3(14.0f, 0.0f, 14.0f);
    return mesh;
}

Mesh makeWedgeMesh() {
    Mesh mesh;
    mesh.material_slot = 2;
    mesh.positions = {
        {-0.5f, -0.5f, -0.5f}, {0.5f, -0.5f, -0.5f}, {0.5f, -0.5f, 0.5f},
        {-0.5f, -0.5f, 0.5f},  {0.0f, 0.5f, -0.5f},  {0.0f, 0.5f, 0.5f},
    };
    mesh.indices = {
        0, 4, 5, 0, 5, 3,  // left roof
        1, 2, 5, 1, 5, 4,  // right roof
        0, 1, 4,           // back gable
        3, 5, 2,           // front gable
        0, 3, 2, 0, 2, 1,  // bottom
    };
    mesh.local_bounds.min = Vec3(-0.5f, -0.5f, -0.5f);
    mesh.local_bounds.max = Vec3(0.5f, 0.5f, 0.5f);
    return mesh;
}

AABB computeContentBounds(Scene& scene, std::uint32_t node_index) {
    Node& node = scene.nodes[node_index];
    AABB content;
    if (node.mesh_index != kNoMesh) {
        content = transformAABB(scene.meshes[node.mesh_index].local_bounds, node.world);
    }
    for (const std::uint32_t child : node.children) {
        content.expand(computeContentBounds(scene, child));
    }
    node.content_bounds = content;
    return content;
}

void assignBounds(Scene& scene) {
    computeContentBounds(scene, 0);
    for (Node& node : scene.nodes) {
        node.declared_bounds =
            node.content_bounds.valid() ? node.content_bounds : AABB{Vec3(0.0f), Vec3(0.0f)};
        node.declared_bounds.pad(0.015f);
    }
}

Mat4 transformAt(const Vec3& translation, const Vec3& scale,
                 const Vec3& rotation_degrees = Vec3(0.0f)) {
    Mat4 matrix = glm::translate(Mat4(1.0f), translation);
    matrix = glm::rotate(matrix, glm::radians(rotation_degrees.y), Vec3(0.0f, 1.0f, 0.0f));
    matrix = glm::rotate(matrix, glm::radians(rotation_degrees.x), Vec3(1.0f, 0.0f, 0.0f));
    matrix = glm::rotate(matrix, glm::radians(rotation_degrees.z), Vec3(0.0f, 0.0f, 1.0f));
    matrix = glm::scale(matrix, scale);
    return matrix;
}

Scene makeBaseScene() {
    Scene scene;
    scene.materials = {
        {Vec3(0.54f, 0.55f, 0.57f), 0.92f},
        {Vec3(0.83f, 0.43f, 0.20f), 0.62f},
        {Vec3(0.22f, 0.48f, 0.76f), 0.48f},
    };
    scene.meshes = {makeGroundMesh(), makeCubeMesh(1), makeWedgeMesh()};

    auto makeNode = [](std::string name, std::int32_t parent, std::uint32_t mesh,
                       const Mat4& world) {
        Node node;
        node.name = std::move(name);
        node.parent = parent;
        node.mesh_index = mesh;
        node.world = world;
        return node;
    };

    scene.nodes.push_back(makeNode("root", -1, kNoMesh, Mat4(1.0f)));
    scene.nodes.push_back(makeNode("ground", 0, 0, Mat4(1.0f)));
    scene.nodes.push_back(makeNode("props", 0, kNoMesh, Mat4(1.0f)));
    scene.nodes.push_back(
        makeNode("box_a", 2, 1, transformAt(Vec3(-4.3f, 1.35f, 2.2f), Vec3(2.4f, 2.7f, 2.4f))));
    scene.nodes.push_back(
        makeNode("box_b", 2, 1, transformAt(Vec3(1.8f, 1.0f, 0.2f), Vec3(2.0f, 2.0f, 2.0f))));
    scene.nodes.push_back(
        makeNode("tower", 2, 1, transformAt(Vec3(6.2f, 2.4f, -2.1f), Vec3(1.2f, 4.8f, 1.2f))));
    scene.nodes.push_back(makeNode(
        "sloped_roof", 2, 2,
        transformAt(Vec3(-4.5f, 0.9f, -2.3f), Vec3(3.4f, 2.4f, 2.0f), Vec3(0.0f, 18.0f, 0.0f))));
    scene.nodes.push_back(makeNode(
        "occluder_wall", 0, 1,
        transformAt(Vec3(0.0f, 5.0f, -4.0f), Vec3(14.0f, 10.0f, 1.2f))));
    scene.nodes.push_back(makeNode(
        "hidden_box", 0, 1,
        transformAt(Vec3(-3.0f, 0.5f, -5.2f), Vec3(1.0f, 1.0f, 1.0f))));
    scene.nodes.push_back(makeNode(
        "far_box", 0, 1,
        transformAt(Vec3(3.6f, 0.8f, -7.0f), Vec3(1.4f, 1.6f, 1.4f))));
    scene.nodes.push_back(makeNode(
        "behind_camera", 0, 1,
        transformAt(Vec3(0.0f, 1.0f, 22.0f), Vec3(1.0f, 2.0f, 1.0f))));

    for (std::uint32_t i = 1; i < scene.nodes.size(); ++i) {
        const std::int32_t parent = scene.nodes[i].parent;
        if (parent >= 0) {
            scene.nodes[static_cast<std::size_t>(parent)].children.push_back(i);
        }
    }
    for (Node& node : scene.nodes) {
        if (std::abs(glm::determinant(node.world)) > 1.0e-12f) {
            node.inverse_world = glm::inverse(node.world);
        }
    }
    assignBounds(scene);
    scene.content_hash = computeSceneHash(scene);
    return scene;
}

Status writeSceneData(const std::string& path, const Scene& scene) {
    Writer writer;
    writer.bytes("RSCN", 4);
    writer.u32(kSceneFormatVersion);
    writer.u32(static_cast<std::uint32_t>(scene.materials.size()));
    writer.u32(static_cast<std::uint32_t>(scene.meshes.size()));
    writer.u32(static_cast<std::uint32_t>(scene.nodes.size()));
    for (const Material& material : scene.materials) {
        writer.vec3(material.albedo);
        writer.f32(material.roughness);
    }
    for (const Mesh& mesh : scene.meshes) {
        writer.u32(mesh.material_slot);
        writer.u32(static_cast<std::uint32_t>(mesh.positions.size()));
        writer.u32(static_cast<std::uint32_t>(mesh.indices.size()));
        writer.vec3(mesh.local_bounds.min);
        writer.vec3(mesh.local_bounds.max);
        for (const Vec3& position : mesh.positions) {
            writer.vec3(position);
        }
        for (const std::uint32_t index : mesh.indices) {
            writer.u32(index);
        }
    }
    for (const Node& node : scene.nodes) {
        writer.string(node.name);
        writer.i32(node.parent);
        writer.u32(node.mesh_index);
        writer.mat4(node.world);
        writer.vec3(node.declared_bounds.min);
        writer.vec3(node.declared_bounds.max);
    }

    std::ofstream out(path, std::ios::binary | std::ios::trunc);
    if (!out) {
        return Status::error(ErrorCode::IoError, "cannot create scene file: " + path);
    }
    const auto& bytes = writer.data();
    out.write(reinterpret_cast<const char*>(bytes.data()),
              static_cast<std::streamsize>(bytes.size()));
    out.flush();
    if (!out) {
        return Status::error(ErrorCode::IoError, "failed to write scene file: " + path);
    }
    return Status::success();
}

}  // namespace

Scene makeSyntheticScene() {
    return makeBaseScene();
}

Status writeSyntheticScene(const std::string& path, SyntheticVariant variant) {
    Scene scene = makeBaseScene();
    switch (variant) {
        case SyntheticVariant::Valid:
            break;
        case SyntheticVariant::BadIndex:
            scene.meshes[1].indices[0] = 9999;
            break;
        case SyntheticVariant::BadTransform:
            scene.nodes[3].world = Mat4(0.0f);
            break;
        case SyntheticVariant::BadBounds:
            scene.nodes[3].declared_bounds.min = Vec3(100.0f);
            scene.nodes[3].declared_bounds.max = Vec3(101.0f);
            break;
    }
    return writeSceneData(path, scene);
}

Status generateAllSyntheticSamples(const std::string& directory) {
    std::error_code ec;
    std::filesystem::create_directories(directory, ec);
    if (ec) {
        return Status::error(ErrorCode::IoError,
                             "cannot create sample directory: " + directory);
    }
    const std::pair<SyntheticVariant, std::string> samples[] = {
        {SyntheticVariant::Valid, "synthetic.rscene"},
        {SyntheticVariant::BadIndex, "invalid_index.rscene"},
        {SyntheticVariant::BadTransform, "invalid_transform.rscene"},
        {SyntheticVariant::BadBounds, "invalid_bounds.rscene"},
    };
    for (const auto& sample : samples) {
        const std::string path = (std::filesystem::path(directory) / sample.second).string();
        const Status status = writeSyntheticScene(path, sample.first);
        if (!status.ok()) {
            return status;
        }
    }
    return Status::success();
}

}  // namespace shadowpipe

