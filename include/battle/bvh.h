#pragma once

#include <glm/vec3.hpp>

#include "battle/scene.h"

#include <cstdint>
#include <functional>
#include <vector>

namespace battle {

struct Ray {
    glm::vec3 origin{0.0F};
    glm::vec3 direction{0.0F, 0.0F, -1.0F};
};

class Bvh {
public:
    Bvh() = default;

    struct Node {
        Aabb bounds;
        std::uint32_t left = 0;
        std::uint32_t right = 0;
        std::uint32_t start = 0;
        std::uint32_t count = 0;
        bool leaf = true;
    };

    static Bvh build(const std::vector<Aabb>& primitive_bounds);

    void query(const Ray& ray, const std::function<void(std::uint32_t)>& visitor) const;
    bool empty() const noexcept { return nodes_.empty(); }
    const std::vector<Aabb>& primitive_bounds() const noexcept { return primitive_bounds_; }
    const std::vector<Node>& nodes() const noexcept { return nodes_; }

private:
    std::uint32_t build_node(std::uint32_t start, std::uint32_t count);

    std::vector<Aabb> primitive_bounds_;
    std::vector<std::uint32_t> order_;
    std::vector<Node> nodes_;
};

bool ray_aabb(const Ray& ray, const Aabb& box, float& near_t, float& far_t,
              glm::vec3& near_normal);

}  // namespace battle

