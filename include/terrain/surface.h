#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "terrain/quadtree.h"
#include "terrain/types.h"

namespace terrain {

struct SurfaceUpdateReport {
    std::uint64_t recomputed_normal_vertices = 0;
    std::uint64_t recomputed_material_vertices = 0;
    float max_normal_angle_deg = 0.0f;
    float max_material_delta = 0.0f;
    std::uint32_t max_normal_x = 0;
    std::uint32_t max_normal_y = 0;
    std::uint64_t normal_mismatch_count = 0;
    std::vector<std::string> notes;
};

class SurfaceCache {
public:
    void computeFull(const HeightField& height, const MaterialImage& material);
    void updateIncremental(const HeightField& height,
                           const MaterialImage& material,
                           const Quadtree& tree,
                           const std::vector<std::uint64_t>& affected_blocks,
                           const Rect* height_changed,
                           const Rect* material_changed,
                           bool height_changed_flag,
                           SurfaceUpdateReport* report);

    Vec3 normalAt(std::uint32_t x, std::uint32_t y) const;
    Vec3 materialColorAt(std::uint32_t x, std::uint32_t y) const;
    const std::vector<Vec3>& normals() const { return normals_; }
    const std::vector<Vec3>& material_colors() const {
        return material_colors_;
    }

    static Vec3 paletteColor(std::uint32_t index);
    static Vec3 sampleMaterialBilinear(const MaterialImage& material,
                                       float x, float y);

private:
    Vec3 computeNormal(const HeightField& height, std::uint32_t x,
                       std::uint32_t y) const;

    std::uint32_t width_ = 0;
    std::uint32_t height_ = 0;
    std::vector<Vec3> normals_;
    std::vector<Vec3> material_colors_;
};

}  // namespace terrain

