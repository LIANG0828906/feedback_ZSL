#include "shadowpipe/raster.hpp"

#include <algorithm>
#include <cmath>

namespace shadowpipe {
namespace {

float edgeFunction(float ax, float ay, float bx, float by, float px, float py) {
    return (px - ax) * (by - ay) - (py - ay) * (bx - ax);
}

}  // namespace

void rasterizeDepth(const std::vector<WorldTriangle>& triangles,
                    const LightFrame& frame, std::uint32_t width,
                    std::uint32_t height, std::vector<float>& depth_buffer,
                    const std::vector<std::uint8_t>* allowed_nodes,
                    std::vector<std::uint32_t>* owner_buffer) {
    depth_buffer.assign(static_cast<std::size_t>(width) * height, 1.0f);
    if (owner_buffer != nullptr) {
        owner_buffer->assign(static_cast<std::size_t>(width) * height, kNoMesh);
    }
    if (width == 0 || height == 0) {
        return;
    }
    const float x_range = frame.x_max - frame.x_min;
    const float y_range = frame.y_max - frame.y_min;
    const float z_range = frame.z_max - frame.z_min;
    if (x_range <= 0.0f || y_range <= 0.0f || z_range <= 0.0f) {
        return;
    }

    for (const WorldTriangle& triangle : triangles) {
        if (allowed_nodes != nullptr &&
            triangle.node_index < allowed_nodes->size() &&
            (*allowed_nodes)[triangle.node_index] == 0) {
            continue;
        }
        const Vec3 a = frame.toLightSpace(triangle.p0);
        const Vec3 b = frame.toLightSpace(triangle.p1);
        const Vec3 c = frame.toLightSpace(triangle.p2);
        const float ax = (a.x - frame.x_min) / x_range * static_cast<float>(width);
        const float ay =
            (1.0f - (a.y - frame.y_min) / y_range) * static_cast<float>(height);
        const float bx = (b.x - frame.x_min) / x_range * static_cast<float>(width);
        const float by =
            (1.0f - (b.y - frame.y_min) / y_range) * static_cast<float>(height);
        const float cx = (c.x - frame.x_min) / x_range * static_cast<float>(width);
        const float cy =
            (1.0f - (c.y - frame.y_min) / y_range) * static_cast<float>(height);
        const float area = edgeFunction(ax, ay, bx, by, cx, cy);
        if (std::abs(area) < 1.0e-8f) {
            continue;
        }
        const int min_x =
            std::max(0, static_cast<int>(std::floor(std::min({ax, bx, cx}))));
        const int max_x = std::min(
            static_cast<int>(width) - 1,
            static_cast<int>(std::ceil(std::max({ax, bx, cx}))));
        const int min_y =
            std::max(0, static_cast<int>(std::floor(std::min({ay, by, cy}))));
        const int max_y = std::min(
            static_cast<int>(height) - 1,
            static_cast<int>(std::ceil(std::max({ay, by, cy}))));
        if (min_x > max_x || min_y > max_y) {
            continue;
        }

        const float za = clamp01((a.z - frame.z_min) / z_range);
        const float zb = clamp01((b.z - frame.z_min) / z_range);
        const float zc = clamp01((c.z - frame.z_min) / z_range);
        for (int y = min_y; y <= max_y; ++y) {
            for (int x = min_x; x <= max_x; ++x) {
                const float px = static_cast<float>(x) + 0.5f;
                const float py = static_cast<float>(y) + 0.5f;
                float w0 = edgeFunction(bx, by, cx, cy, px, py) / area;
                float w1 = edgeFunction(cx, cy, ax, ay, px, py) / area;
                float w2 = edgeFunction(ax, ay, bx, by, px, py) / area;
                constexpr float edge_epsilon = -1.0e-6f;
                if (w0 < edge_epsilon || w1 < edge_epsilon || w2 < edge_epsilon) {
                    continue;
                }
                const float depth = w0 * za + w1 * zb + w2 * zc;
                const std::size_t index =
                    static_cast<std::size_t>(y) * width + static_cast<std::size_t>(x);
                if (depth < depth_buffer[index]) {
                    depth_buffer[index] = depth;
                    if (owner_buffer != nullptr) {
                        (*owner_buffer)[index] = triangle.node_index;
                    }
                }
            }
        }
    }
}

}  // namespace shadowpipe

