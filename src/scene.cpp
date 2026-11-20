#include "battle/scene.h"

#include "battle/binary.h"

#include <glm/geometric.hpp>

#include <algorithm>
#include <cmath>
#include <fstream>
#include <limits>
#include <set>
#include <sstream>

namespace battle {
namespace {

constexpr char kSceneMagic[8] = {'B', 'T', 'S', 'S', 'C', 'N', '0', '1'};
constexpr std::uint32_t kSceneEnd = 0x4E4F5442U;  // "BTON"
constexpr std::uint64_t kMaxGridCells = 32ULL * 1024ULL * 1024ULL;
constexpr std::uint32_t kMaxEntities = 4U * 1024U * 1024U;
constexpr float kMinExtent = 1.0e-5F;

Error scene_error(std::string message, std::string point,
                  EntityRef entity = {}, int vertex = -1) {
    Error error;
    error.code = StatusCode::SceneInvalid;
    error.message = std::move(message);
    error.injection_point = std::move(point);
    error.entity = entity;
    error.vertex_index = vertex;
    return error;
}

bool read_vec3(ByteReader& reader, glm::vec3& value) {
    return reader.vec3(value);
}

}  // namespace

bool is_finite(float value) noexcept {
    return std::isfinite(value) != 0;
}

bool is_finite(const glm::vec3& value) noexcept {
    return is_finite(value.x) && is_finite(value.y) && is_finite(value.z);
}

bool valid_aabb(const Aabb& box) noexcept {
    return is_finite(box.min) && is_finite(box.max) &&
           box.max.x - box.min.x > kMinExtent &&
           box.max.y - box.min.y > kMinExtent &&
           box.max.z - box.min.z > kMinExtent;
}

bool valid_capsule(const Capsule& capsule) noexcept {
    if (!is_finite(capsule.a) || !is_finite(capsule.b) || !is_finite(capsule.radius) ||
        capsule.radius <= kMinExtent) {
        return false;
    }
    const glm::vec3 axis = capsule.b - capsule.a;
    return is_finite(axis) && glm::dot(axis, axis) > kMinExtent * kMinExtent;
}

bool valid_triangle(const Triangle& triangle) noexcept {
    if (!is_finite(triangle.a) || !is_finite(triangle.b) || !is_finite(triangle.c)) {
        return false;
    }
    const glm::vec3 normal = glm::cross(triangle.b - triangle.a, triangle.c - triangle.a);
    return is_finite(normal) && glm::dot(normal, normal) > 1.0e-12F;
}

bool Grid::in_bounds_xz(float x, float z) const noexcept {
    if (!is_finite(x) || !is_finite(z) || cell_size <= 0.0F) {
        return false;
    }
    const float max_x = origin.x + static_cast<float>(width) * cell_size;
    const float max_z = origin.z + static_cast<float>(depth) * cell_size;
    return x >= origin.x && x <= max_x && z >= origin.z && z <= max_z;
}

bool Grid::walkable_at(std::uint32_t x, std::uint32_t y, std::uint32_t z) const {
    if (!contains_cell(x, y, z)) {
        return false;
    }
    const std::size_t i = index(x, y, z);
    return i < walkable.size() && walkable[i] != 0;
}

bool Grid::walkable_world_xz(float x, float z) const {
    if (!in_bounds_xz(x, z) || levels == 0 || walkable.empty()) {
        return false;
    }
    float gx = (x - origin.x) / cell_size;
    float gz = (z - origin.z) / cell_size;
    if (gx >= static_cast<float>(width)) {
        gx = static_cast<float>(width) - std::numeric_limits<float>::epsilon();
    }
    if (gz >= static_cast<float>(depth)) {
        gz = static_cast<float>(depth) - std::numeric_limits<float>::epsilon();
    }
    if (gx < 0.0F || gz < 0.0F) {
        return false;
    }
    const auto ix = static_cast<std::uint32_t>(gx);
    const auto iz = static_cast<std::uint32_t>(gz);
    return walkable_at(ix, 0, iz);
}

Aabb unit_bounds(const Unit& unit) {
    if (unit.shape == UnitShape::Aabb) {
        return unit.aabb;
    }
    const glm::vec3 radius(unit.capsule.radius);
    Aabb box;
    box.min = glm::min(unit.capsule.a, unit.capsule.b) - radius;
    box.max = glm::max(unit.capsule.a, unit.capsule.b) + radius;
    return box;
}

Aabb triangle_bounds(const Triangle& triangle) {
    Aabb box;
    box.min = glm::min(triangle.a, glm::min(triangle.b, triangle.c));
    box.max = glm::max(triangle.a, glm::max(triangle.b, triangle.c));
    return box;
}

Aabb merge_bounds(const Aabb& lhs, const Aabb& rhs) {
    Aabb result;
    result.min = glm::min(lhs.min, rhs.min);
    result.max = glm::max(lhs.max, rhs.max);
    return result;
}

glm::vec3 aabb_center(const Aabb& box) {
    return (box.min + box.max) * 0.5F;
}

Outcome<void> Scene::validate() const {
    if (grid.width == 0 || grid.levels == 0 || grid.depth == 0) {
        return Outcome<void>::failure(scene_error(
            "grid dimensions must be non-zero", "scene.grid.dimensions"));
    }
    if (!is_finite(grid.cell_size) || grid.cell_size <= 0.0F) {
        return Outcome<void>::failure(scene_error(
            "grid cell size must be finite and positive", "scene.grid.cell_size"));
    }
    if (!is_finite(grid.origin)) {
        return Outcome<void>::failure(scene_error(
            "grid origin must be finite", "scene.grid.origin"));
    }
    const std::uint64_t expected_cells = grid.cell_count();
    if (expected_cells == 0 || expected_cells > kMaxGridCells) {
        return Outcome<void>::failure(scene_error(
            "grid cell count is outside the supported range", "scene.grid.cell_count"));
    }
    if (grid.walkable.size() != static_cast<std::size_t>(expected_cells)) {
        return Outcome<void>::failure(scene_error(
            "walkability marker count does not match grid dimensions",
            "scene.grid.walkable.count"));
    }
    for (std::uint64_t i = 0; i < expected_cells; ++i) {
        const std::uint8_t marker = grid.walkable[static_cast<std::size_t>(i)];
        if (marker != 0 && marker != 1) {
            Error error = scene_error(
                "walkability marker must be 0 or 1", "scene.grid.walkable.marker");
            error.edge_index = static_cast<int>(i);
            return Outcome<void>::failure(std::move(error));
        }
    }

    std::set<std::uint32_t> ids;
    std::uint32_t previous_id = 0;
    for (std::size_t i = 0; i < units.size(); ++i) {
        const Unit& unit = units[i];
        const EntityRef ref{EntityKind::Unit, unit.id};
        if (unit.id == 0) {
            return Outcome<void>::failure(scene_error(
                "unit id must be non-zero", "scene.unit.id", ref));
        }
        if (!ids.insert(unit.id).second) {
            return Outcome<void>::failure(scene_error(
                "duplicate unit id", "scene.unit.id.duplicate", ref));
        }
        if (unit.id < previous_id) {
            return Outcome<void>::failure(scene_error(
                "unit ids must be in ascending order", "scene.unit.id.order", ref));
        }
        previous_id = unit.id;
        if (unit.shape != UnitShape::Aabb && unit.shape != UnitShape::Capsule) {
            return Outcome<void>::failure(scene_error(
                "unknown unit shape tag", "scene.unit.shape", ref));
        }
        if (unit.shape == UnitShape::Aabb && !valid_aabb(unit.aabb)) {
            return Outcome<void>::failure(scene_error(
                "unit AABB is non-finite or degenerate", "scene.unit.aabb", ref));
        }
        if (unit.shape == UnitShape::Capsule && !valid_capsule(unit.capsule)) {
            return Outcome<void>::failure(scene_error(
                "unit capsule is non-finite or degenerate", "scene.unit.capsule", ref));
        }
    }

    ids.clear();
    previous_id = 0;
    for (std::size_t i = 0; i < covers.size(); ++i) {
        const Cover& cover = covers[i];
        const EntityRef ref{EntityKind::Cover, cover.id};
        if (cover.id == 0) {
            return Outcome<void>::failure(scene_error(
                "cover id must be non-zero", "scene.cover.id", ref));
        }
        if (!ids.insert(cover.id).second) {
            return Outcome<void>::failure(scene_error(
                "duplicate cover id", "scene.cover.id.duplicate", ref));
        }
        if (cover.id < previous_id) {
            return Outcome<void>::failure(scene_error(
                "cover ids must be in ascending order", "scene.cover.id.order", ref));
        }
        previous_id = cover.id;
        if (!valid_triangle(cover.triangle)) {
            return Outcome<void>::failure(scene_error(
                "cover triangle is non-finite or degenerate",
                "scene.cover.triangle", ref));
        }
    }
    return Outcome<void>::success();
}

Outcome<Scene> Scene::load_binary(const std::string& path) {
    std::ifstream stream(path, std::ios::binary);
    if (!stream) {
        return Outcome<Scene>::failure(
            StatusCode::IoError, "cannot open scene file: " + path, "scene.file.open");
    }
    std::string bytes((std::istreambuf_iterator<char>(stream)),
                      std::istreambuf_iterator<char>());
    if (!stream.good() && !stream.eof()) {
        return Outcome<Scene>::failure(
            StatusCode::IoError, "cannot read scene file: " + path, "scene.file.read");
    }
    ByteReader reader(bytes);

    char magic[8]{};
    if (!reader.bytes(magic, sizeof(magic)) ||
        !std::equal(std::begin(kSceneMagic), std::end(kSceneMagic), std::begin(magic))) {
        return Outcome<Scene>::failure(scene_error(
            "bad scene magic", "scene.header.magic"));
    }

    std::uint32_t version = 0;
    std::uint32_t flags = 0;
    if (!reader.u32(version) || !reader.u32(flags)) {
        return Outcome<Scene>::failure(scene_error(
            "truncated scene header", "scene.header.version"));
    }
    if (version != kSceneFormatVersion) {
        return Outcome<Scene>::failure(scene_error(
            "unsupported scene format version", "scene.header.version"));
    }
    if (flags != 0) {
        return Outcome<Scene>::failure(scene_error(
            "unknown scene flags", "scene.header.flags"));
    }

    Scene scene;
    if (!reader.u32(scene.grid.width) || !reader.u32(scene.grid.levels) ||
        !reader.u32(scene.grid.depth) ||
        !reader.f32(scene.grid.cell_size) ||
        !read_vec3(reader, scene.grid.origin)) {
        return Outcome<Scene>::failure(scene_error(
            "truncated grid header", "scene.grid.header"));
    }
    if (scene.grid.width == 0 || scene.grid.levels == 0 || scene.grid.depth == 0 ||
        scene.grid.cell_count() > kMaxGridCells) {
        return Outcome<Scene>::failure(scene_error(
            "invalid grid dimensions", "scene.grid.dimensions"));
    }

    std::uint32_t walkable_count = 0;
    if (!reader.u32(walkable_count)) {
        return Outcome<Scene>::failure(scene_error(
            "missing walkability marker count", "scene.grid.walkable.count"));
    }
    const auto expected_cells = static_cast<std::uint32_t>(scene.grid.cell_count());
    if (walkable_count != expected_cells) {
        return Outcome<Scene>::failure(scene_error(
            "walkability marker count does not match grid dimensions",
            "scene.grid.walkable.count"));
    }
    scene.grid.walkable.resize(walkable_count);
    if (!scene.grid.walkable.empty() &&
        !reader.bytes(scene.grid.walkable.data(), scene.grid.walkable.size())) {
        return Outcome<Scene>::failure(scene_error(
            "walkability markers are truncated", "scene.grid.walkable.missing"));
    }
    for (std::uint32_t i = 0; i < walkable_count; ++i) {
        const std::uint8_t marker = scene.grid.walkable[i];
        if (marker != 0 && marker != 1) {
            Error error = scene_error(
                "walkability marker must be 0 or 1",
                "scene.grid.walkable.marker");
            error.edge_index = static_cast<int>(i);
            return Outcome<Scene>::failure(std::move(error));
        }
    }

    std::uint32_t unit_count = 0;
    if (!reader.u32(unit_count)) {
        return Outcome<Scene>::failure(scene_error(
            "missing unit count", "scene.unit.count"));
    }
    if (unit_count > kMaxEntities) {
        return Outcome<Scene>::failure(scene_error(
            "unit count is too large", "scene.unit.count"));
    }
    scene.units.reserve(unit_count);
    for (std::uint32_t i = 0; i < unit_count; ++i) {
        Unit unit;
        std::uint32_t record_index = 0;
        std::uint8_t shape = 0;
        std::uint8_t reserved[3]{};
        if (!reader.u32(unit.id) || !reader.u32(record_index) || !reader.u8(shape) ||
            !reader.bytes(reserved, sizeof(reserved))) {
            Error error = scene_error(
                "truncated unit record", "scene.unit.record");
            error.entity = {EntityKind::Unit, unit.id};
            return Outcome<Scene>::failure(std::move(error));
        }
        if (record_index != i) {
            return Outcome<Scene>::failure(scene_error(
                "unit record index is not contiguous", "scene.unit.index",
                {EntityKind::Unit, unit.id}));
        }
        if (reserved[0] != 0 || reserved[1] != 0 || reserved[2] != 0) {
            return Outcome<Scene>::failure(scene_error(
                "nonzero unit reserved bytes", "scene.unit.reserved",
                {EntityKind::Unit, unit.id}));
        }
        unit.shape = static_cast<UnitShape>(shape);
        if (unit.shape == UnitShape::Aabb) {
            if (!read_vec3(reader, unit.aabb.min) || !read_vec3(reader, unit.aabb.max)) {
                return Outcome<Scene>::failure(scene_error(
                    "truncated AABB unit", "scene.unit.aabb",
                    {EntityKind::Unit, unit.id}));
            }
        } else if (unit.shape == UnitShape::Capsule) {
            if (!read_vec3(reader, unit.capsule.a) ||
                !read_vec3(reader, unit.capsule.b) ||
                !reader.f32(unit.capsule.radius)) {
                return Outcome<Scene>::failure(scene_error(
                    "truncated capsule unit", "scene.unit.capsule",
                    {EntityKind::Unit, unit.id}));
            }
        } else {
            return Outcome<Scene>::failure(scene_error(
                "unknown unit shape tag", "scene.unit.shape",
                {EntityKind::Unit, unit.id}));
        }
        scene.units.push_back(unit);
    }

    std::uint32_t cover_count = 0;
    if (!reader.u32(cover_count)) {
        return Outcome<Scene>::failure(scene_error(
            "missing cover count", "scene.cover.count"));
    }
    if (cover_count > kMaxEntities) {
        return Outcome<Scene>::failure(scene_error(
            "cover count is too large", "scene.cover.count"));
    }
    scene.covers.reserve(cover_count);
    for (std::uint32_t i = 0; i < cover_count; ++i) {
        Cover cover;
        std::uint32_t record_index = 0;
        if (!reader.u32(cover.id) || !reader.u32(record_index) ||
            !read_vec3(reader, cover.triangle.a) ||
            !read_vec3(reader, cover.triangle.b) ||
            !read_vec3(reader, cover.triangle.c)) {
            Error error = scene_error(
                "truncated cover record", "scene.cover.record");
            error.entity = {EntityKind::Cover, cover.id};
            return Outcome<Scene>::failure(std::move(error));
        }
        if (record_index != i) {
            return Outcome<Scene>::failure(scene_error(
                "cover record index is not contiguous", "scene.cover.index",
                {EntityKind::Cover, cover.id}));
        }
        scene.covers.push_back(cover);
    }

    std::uint32_t end_marker = 0;
    if (!reader.u32(end_marker) || end_marker != kSceneEnd || !reader.finished()) {
        return Outcome<Scene>::failure(scene_error(
            "missing or invalid scene end marker", "scene.footer"));
    }

    std::sort(scene.units.begin(), scene.units.end(),
              [](const Unit& lhs, const Unit& rhs) { return lhs.id < rhs.id; });
    std::sort(scene.covers.begin(), scene.covers.end(),
              [](const Cover& lhs, const Cover& rhs) { return lhs.id < rhs.id; });

    Outcome<void> valid = scene.validate();
    if (!valid) {
        return Outcome<Scene>::failure(valid.error());
    }
    return Outcome<Scene>::success(std::move(scene));
}

std::string Scene::encode_binary() const {
    ByteWriter writer;
    writer.bytes(kSceneMagic, sizeof(kSceneMagic));
    writer.u32(kSceneFormatVersion);
    writer.u32(0);
    writer.u32(grid.width);
    writer.u32(grid.levels);
    writer.u32(grid.depth);
    writer.f32(grid.cell_size);
    writer.vec3(grid.origin);
    writer.u32(static_cast<std::uint32_t>(grid.walkable.size()));
    writer.bytes(grid.walkable.data(), grid.walkable.size());
    writer.u32(static_cast<std::uint32_t>(units.size()));
    for (std::size_t record_index = 0; record_index < units.size(); ++record_index) {
        const Unit& unit = units[record_index];
        writer.u32(unit.id);
        writer.u32(static_cast<std::uint32_t>(record_index));
        writer.u8(static_cast<std::uint8_t>(unit.shape));
        writer.u8(0);
        writer.u8(0);
        writer.u8(0);
        if (unit.shape == UnitShape::Aabb) {
            writer.vec3(unit.aabb.min);
            writer.vec3(unit.aabb.max);
        } else {
            writer.vec3(unit.capsule.a);
            writer.vec3(unit.capsule.b);
            writer.f32(unit.capsule.radius);
        }
    }
    writer.u32(static_cast<std::uint32_t>(covers.size()));
    for (std::size_t record_index = 0; record_index < covers.size(); ++record_index) {
        const Cover& cover = covers[record_index];
        writer.u32(cover.id);
        writer.u32(static_cast<std::uint32_t>(record_index));
        writer.vec3(cover.triangle.a);
        writer.vec3(cover.triangle.b);
        writer.vec3(cover.triangle.c);
    }
    writer.u32(kSceneEnd);
    return writer.take();
}

}  // namespace battle
