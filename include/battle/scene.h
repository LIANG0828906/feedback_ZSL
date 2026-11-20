#pragma once

#include <glm/vec3.hpp>

#include "battle/status.h"

#include <cstdint>
#include <string>
#include <vector>

namespace battle {

constexpr std::uint32_t kSceneFormatVersion = 1;
constexpr std::uint64_t kSnapshotFormatVersion = 1;

struct Grid {
    std::uint32_t width = 0;
    std::uint32_t levels = 0;
    std::uint32_t depth = 0;
    float cell_size = 1.0F;
    glm::vec3 origin{0.0F};
    std::vector<std::uint8_t> walkable;

    std::uint64_t cell_count() const noexcept {
        return static_cast<std::uint64_t>(width) *
               static_cast<std::uint64_t>(levels) *
               static_cast<std::uint64_t>(depth);
    }

    std::size_t index(std::uint32_t x, std::uint32_t y, std::uint32_t z) const {
        return (static_cast<std::size_t>(y) * depth + z) * width + x;
    }

    bool contains_cell(std::uint32_t x, std::uint32_t y, std::uint32_t z) const noexcept {
        return x < width && y < levels && z < depth;
    }

    bool in_bounds_xz(float x, float z) const noexcept;
    bool walkable_at(std::uint32_t x, std::uint32_t y, std::uint32_t z) const;
    bool walkable_world_xz(float x, float z) const;
    float ground_y() const noexcept { return origin.y; }
};

enum class UnitShape : std::uint8_t {
    Aabb = 0,
    Capsule = 1
};

struct Aabb {
    glm::vec3 min{0.0F};
    glm::vec3 max{0.0F};
};

struct Capsule {
    glm::vec3 a{0.0F};
    glm::vec3 b{0.0F};
    float radius = 0.0F;
};

struct Unit {
    std::uint32_t id = 0;
    UnitShape shape = UnitShape::Aabb;
    Aabb aabb;
    Capsule capsule;
};

struct Triangle {
    glm::vec3 a{0.0F};
    glm::vec3 b{0.0F};
    glm::vec3 c{0.0F};
};

struct Cover {
    std::uint32_t id = 0;
    Triangle triangle;
    bool present = true;
};

struct Scene {
    Grid grid;
    std::vector<Unit> units;
    std::vector<Cover> covers;

    static Outcome<Scene> load_binary(const std::string& path);
    Outcome<void> validate() const;
    std::string encode_binary() const;
};

struct GridChange {
    std::uint32_t x = 0;
    std::uint32_t y = 0;
    std::uint32_t z = 0;
    bool walkable = false;
};

struct UnitMove {
    std::uint32_t id = 0;
    glm::vec3 delta{0.0F};
};

struct CoverChange {
    std::uint32_t id = 0;
    bool present = false;
};

bool is_finite(const glm::vec3& value) noexcept;
bool is_finite(float value) noexcept;
bool valid_aabb(const Aabb& box) noexcept;
bool valid_capsule(const Capsule& capsule) noexcept;
bool valid_triangle(const Triangle& triangle) noexcept;
Aabb unit_bounds(const Unit& unit);
Aabb triangle_bounds(const Triangle& triangle);
Aabb merge_bounds(const Aabb& lhs, const Aabb& rhs);
glm::vec3 aabb_center(const Aabb& box);

}  // namespace battle

