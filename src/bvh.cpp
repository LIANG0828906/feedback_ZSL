#include "battle/bvh.h"

#include <glm/geometric.hpp>

#include <algorithm>
#include <array>
#include <cmath>
#include <limits>
#include <numeric>

namespace battle {
namespace {

constexpr std::uint32_t kLeafSize = 4;
constexpr float kRayEpsilon = 1.0e-20F;

bool contains(const Aabb& box, const glm::vec3& point) {
    return point.x >= box.min.x && point.x <= box.max.x &&
           point.y >= box.min.y && point.y <= box.max.y &&
           point.z >= box.min.z && point.z <= box.max.z;
}

}  // namespace

bool ray_aabb(const Ray& ray, const Aabb& box, float& near_t, float& far_t,
              glm::vec3& near_normal) {
    if (!is_finite(ray.origin) || !is_finite(ray.direction)) {
        return false;
    }
    float t_enter = -std::numeric_limits<float>::infinity();
    float t_exit = std::numeric_limits<float>::infinity();
    glm::vec3 normal(0.0F);

    const float origin[3] = {ray.origin.x, ray.origin.y, ray.origin.z};
    const float direction[3] = {
        ray.direction.x, ray.direction.y, ray.direction.z};
    const float lower[3] = {box.min.x, box.min.y, box.min.z};
    const float upper[3] = {box.max.x, box.max.y, box.max.z};

    for (int axis = 0; axis < 3; ++axis) {
        if (std::fabs(direction[axis]) <= kRayEpsilon) {
            if (origin[axis] < lower[axis] || origin[axis] > upper[axis]) {
                return false;
            }
            continue;
        }
        const float inverse = 1.0F / direction[axis];
        float axis_near = (lower[axis] - origin[axis]) * inverse;
        float axis_far = (upper[axis] - origin[axis]) * inverse;
        glm::vec3 axis_near_normal(0.0F);
        axis_near_normal[axis] = -1.0F;
        glm::vec3 axis_far_normal(0.0F);
        axis_far_normal[axis] = 1.0F;
        if (axis_near > axis_far) {
            std::swap(axis_near, axis_far);
            std::swap(axis_near_normal, axis_far_normal);
        }
        if (axis_near > t_enter) {
            t_enter = axis_near;
            normal = axis_near_normal;
        }
        t_exit = std::min(t_exit, axis_far);
        if (t_enter > t_exit) {
            return false;
        }
    }
    if (t_exit < 0.0F) {
        return false;
    }
    if (t_enter < 0.0F) {
        t_enter = 0.0F;
        if (contains(box, ray.origin)) {
            normal = -ray.direction;
            const float length = glm::length(normal);
            if (length > 0.0F) {
                normal /= length;
            }
        }
    }
    near_t = t_enter;
    far_t = t_exit;
    near_normal = normal;
    return true;
}

Bvh Bvh::build(const std::vector<Aabb>& primitive_bounds) {
    Bvh bvh;
    bvh.primitive_bounds_ = primitive_bounds;
    if (primitive_bounds.empty()) {
        return bvh;
    }

    bvh.order_.resize(primitive_bounds.size());
    std::iota(bvh.order_.begin(), bvh.order_.end(), 0U);
    bvh.nodes_.reserve(primitive_bounds.size() * 2U);
    const std::uint32_t root = bvh.build_node(0, static_cast<std::uint32_t>(
        primitive_bounds.size()));
    (void)root;
    return bvh;
}

std::uint32_t Bvh::build_node(std::uint32_t start, std::uint32_t count) {
    Aabb bounds = primitive_bounds_[order_[start]];
    Aabb centroids;
    const glm::vec3 first_center = aabb_center(bounds);
    centroids.min = first_center;
    centroids.max = first_center;
    for (std::uint32_t i = 1; i < count; ++i) {
        const Aabb& primitive = primitive_bounds_[order_[start + i]];
        bounds = merge_bounds(bounds, primitive);
        const glm::vec3 center = aabb_center(primitive);
        centroids.min = glm::min(centroids.min, center);
        centroids.max = glm::max(centroids.max, center);
    }

    const std::uint32_t node_index = static_cast<std::uint32_t>(nodes_.size());
    nodes_.push_back(Node{});
    nodes_[node_index].bounds = bounds;

    if (count <= kLeafSize) {
        nodes_[node_index].leaf = true;
        nodes_[node_index].start = start;
        nodes_[node_index].count = count;
        return node_index;
    }

    const glm::vec3 extent = centroids.max - centroids.min;
    int axis = 0;
    if (extent.y > extent.x) {
        axis = 1;
    }
    if ((axis == 0 ? extent.x : extent.y) < extent.z) {
        axis = 2;
    }

    const auto begin = order_.begin() + start;
    const auto end = begin + count;
    std::stable_sort(begin, end, [this, axis](std::uint32_t lhs, std::uint32_t rhs) {
        const glm::vec3 left = aabb_center(primitive_bounds_[lhs]);
        const glm::vec3 right = aabb_center(primitive_bounds_[rhs]);
        const float lv = axis == 0 ? left.x : (axis == 1 ? left.y : left.z);
        const float rv = axis == 0 ? right.x : (axis == 1 ? right.y : right.z);
        if (lv < rv) {
            return true;
        }
        if (lv > rv) {
            return false;
        }
        return lhs < rhs;
    });

    const std::uint32_t left_count = count / 2U;
    nodes_[node_index].leaf = false;
    nodes_[node_index].left = build_node(start, left_count);
    nodes_[node_index].right = build_node(start + left_count, count - left_count);
    nodes_[node_index].start = 0;
    nodes_[node_index].count = 0;
    return node_index;
}

void Bvh::query(const Ray& ray,
                const std::function<void(std::uint32_t)>& visitor) const {
    if (nodes_.empty()) {
        return;
    }
    std::array<std::uint32_t, 128> stack{};
    std::size_t stack_size = 0;
    stack[stack_size++] = 0;
    while (stack_size > 0) {
        const std::uint32_t node_index = stack[--stack_size];
        const Node& node = nodes_[node_index];
        float near_t = 0.0F;
        float far_t = 0.0F;
        glm::vec3 normal(0.0F);
        if (!ray_aabb(ray, node.bounds, near_t, far_t, normal)) {
            continue;
        }
        if (node.leaf) {
            for (std::uint32_t i = 0; i < node.count; ++i) {
                visitor(order_[node.start + i]);
            }
        } else {
            if (stack_size + 2 > stack.size()) {
                continue;
            }
            stack[stack_size++] = node.left;
            stack[stack_size++] = node.right;
        }
    }
}

}  // namespace battle
