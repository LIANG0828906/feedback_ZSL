#include "terrain/surface.h"

#include <algorithm>
#include <cmath>
#include <unordered_set>

namespace terrain {

Vec3 normalize(const Vec3& v) {
    const float length =
        std::sqrt(v.x * v.x + v.y * v.y + v.z * v.z);
    if (!std::isfinite(length) || length <= 1e-20f) {
        return {0.0f, 1.0f, 0.0f};
    }
    return {v.x / length, v.y / length, v.z / length};
}

namespace {

Vec3 mix(const Vec3& a, const Vec3& b, float t) {
    return {a.x + (b.x - a.x) * t, a.y + (b.y - a.y) * t,
            a.z + (b.z - a.z) * t};
}

Vec3 palette(std::uint32_t index) {
    static const Vec3 colors[8] = {
        {0.24f, 0.42f, 0.18f}, {0.56f, 0.48f, 0.31f},
        {0.38f, 0.38f, 0.40f}, {0.75f, 0.72f, 0.58f},
        {0.22f, 0.36f, 0.52f}, {0.50f, 0.28f, 0.22f},
        {0.65f, 0.60f, 0.25f}, {0.30f, 0.55f, 0.48f}};
    return colors[index % 8u];
}

}  // namespace

Vec3 SurfaceCache::paletteColor(std::uint32_t index) {
    return palette(index);
}

Vec3 SurfaceCache::sampleMaterialBilinear(const MaterialImage& material,
                                          float x, float y) {
    const float clamped_x =
        std::max(0.0f, std::min(static_cast<float>(material.width - 1u), x));
    const float clamped_y =
        std::max(0.0f, std::min(static_cast<float>(material.height - 1u), y));
    const std::uint32_t x0 = static_cast<std::uint32_t>(std::floor(clamped_x));
    const std::uint32_t y0 = static_cast<std::uint32_t>(std::floor(clamped_y));
    const std::uint32_t x1 = std::min(x0 + 1u, material.width - 1u);
    const std::uint32_t y1 = std::min(y0 + 1u, material.height - 1u);
    const float tx = clamped_x - static_cast<float>(x0);
    const float ty = clamped_y - static_cast<float>(y0);
    const Vec3 c00 = palette(material.at(x0, y0));
    const Vec3 c10 = palette(material.at(x1, y0));
    const Vec3 c01 = palette(material.at(x0, y1));
    const Vec3 c11 = palette(material.at(x1, y1));
    return mix(mix(c00, c10, tx), mix(c01, c11, tx), ty);
}

Vec3 SurfaceCache::computeNormal(const HeightField& height, std::uint32_t x,
                                 std::uint32_t y) const {
    const std::uint32_t xm = x > 0 ? x - 1u : x;
    const std::uint32_t xp = std::min(x + 1u, height.width - 1u);
    const std::uint32_t ym = y > 0 ? y - 1u : y;
    const std::uint32_t yp = std::min(y + 1u, height.height - 1u);
    const float dx_span =
        static_cast<float>(xp - xm) * height.cell_spacing;
    const float dy_span =
        static_cast<float>(yp - ym) * height.cell_spacing;
    const float dhdx = dx_span > 0.0f
                           ? (height.at(xp, y) - height.at(xm, y)) / dx_span
                           : 0.0f;
    const float dhdy = dy_span > 0.0f
                           ? (height.at(x, yp) - height.at(x, ym)) / dy_span
                           : 0.0f;
    return normalize(Vec3{-dhdx, 1.0f, -dhdy});
}

void SurfaceCache::computeFull(const HeightField& height,
                               const MaterialImage& material) {
    width_ = height.width;
    height_ = height.height;
    normals_.resize(static_cast<std::size_t>(width_) * height_);
    material_colors_.resize(normals_.size());
    for (std::uint32_t y = 0; y < height_; ++y) {
        for (std::uint32_t x = 0; x < width_; ++x) {
            const std::size_t idx = static_cast<std::size_t>(y) * width_ + x;
            normals_[idx] = computeNormal(height, x, y);
            material_colors_[idx] =
                sampleMaterialBilinear(material, static_cast<float>(x) + 0.5f,
                                       static_cast<float>(y) + 0.5f);
        }
    }
}

void SurfaceCache::updateIncremental(
    const HeightField& height, const MaterialImage& material,
    const Quadtree& tree, const std::vector<std::uint64_t>& affected_blocks,
    const Rect* height_changed, const Rect* material_changed,
    bool height_changed_flag,
    SurfaceUpdateReport* report) {
    if (normals_.size() !=
        static_cast<std::size_t>(height.width) * height.height) {
        computeFull(height, material);
        if (report != nullptr) {
            report->recomputed_normal_vertices = normals_.size();
            report->recomputed_material_vertices = material_colors_.size();
        }
        return;
    }
    width_ = height.width;
    height_ = height.height;
    SurfaceUpdateReport local;
    std::vector<Rect> normal_rects;
    std::vector<Rect> material_rects;
    if (height_changed != nullptr) {
        normal_rects.push_back(
            height_changed->expanded(1, width_, height_));
    }
    for (std::uint64_t id : affected_blocks) {
        const QuadNode* node = tree.node(id);
        if (node == nullptr) continue;
        normal_rects.push_back(node->rect.expanded(1, width_, height_));
    }
    if (material_changed != nullptr) {
        material_rects.push_back(
            material_changed->expanded(1, width_, height_));
    }

    if (height_changed_flag && !normal_rects.empty()) {
        for (const Rect& rect : normal_rects) {
            for (std::uint32_t y = rect.y; y < rect.bottom(); ++y) {
                for (std::uint32_t x = rect.x; x < rect.right(); ++x) {
                    const std::size_t idx =
                        static_cast<std::size_t>(y) * width_ + x;
                    normals_[idx] = computeNormal(height, x, y);
                    ++local.recomputed_normal_vertices;
                }
            }
        }
    }
    if (material_changed != nullptr && !material_rects.empty()) {
        for (const Rect& rect : material_rects) {
            for (std::uint32_t y = rect.y; y < rect.bottom(); ++y) {
                for (std::uint32_t x = rect.x; x < rect.right(); ++x) {
                    const std::size_t idx =
                        static_cast<std::size_t>(y) * width_ + x;
                    material_colors_[idx] = sampleMaterialBilinear(
                        material, static_cast<float>(x) + 0.5f,
                        static_cast<float>(y) + 0.5f);
                    ++local.recomputed_material_vertices;
                }
            }
        }
    }

    SurfaceCache truth;
    truth.computeFull(height, material);
    for (std::size_t i = 0; i < normals_.size(); ++i) {
        const Vec3 cross_product = cross(normals_[i], truth.normals_[i]);
        const float cross_length =
            std::sqrt(dot(cross_product, cross_product));
        const float d = std::max(-1.0f, std::min(1.0f, dot(normals_[i],
                                                           truth.normals_[i])));
        const float angle =
            std::atan2(cross_length, d) * 180.0f /
            3.14159265358979323846f;
        if (angle > 1e-6f) {
            ++local.normal_mismatch_count;
        }
        if (angle > local.max_normal_angle_deg) {
            local.max_normal_x =
                static_cast<std::uint32_t>(i % width_);
            local.max_normal_y =
                static_cast<std::uint32_t>(i / width_);
        }
        local.max_normal_angle_deg =
            std::max(local.max_normal_angle_deg, angle);
        const Vec3& a = material_colors_[i];
        const Vec3& b = truth.material_colors_[i];
        local.max_material_delta =
            std::max(local.max_material_delta,
                     std::max(std::fabs(a.x - b.x),
                              std::max(std::fabs(a.y - b.y),
                                       std::fabs(a.z - b.z))));
    }
    if (local.recomputed_normal_vertices == 0 && height_changed_flag) {
        local.notes.push_back(
            "height changed without mapped block; conservative full normals");
        normals_ = truth.normals_;
        local.recomputed_normal_vertices = normals_.size();
    }
    if (material_changed == nullptr) {
        local.notes.push_back("material buffers reused");
    }
    if (report != nullptr) {
        *report = std::move(local);
    }
}

Vec3 SurfaceCache::normalAt(std::uint32_t x, std::uint32_t y) const {
    return normals_[static_cast<std::size_t>(y) * width_ + x];
}

Vec3 SurfaceCache::materialColorAt(std::uint32_t x, std::uint32_t y) const {
    return material_colors_[static_cast<std::size_t>(y) * width_ + x];
}

}  // namespace terrain

