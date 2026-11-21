#pragma once

#include <cstdint>
#include <map>
#include <unordered_map>
#include <vector>

#include "terrain/quadtree.h"
#include "terrain/status.h"
#include "terrain/surface.h"
#include "terrain/types.h"

namespace terrain {

struct MeshVertex {
    std::uint32_t grid_x = 0;
    std::uint32_t grid_y = 0;
    Vec3 position;
    Vec3 normal;
    Vec3 material_color;
};

struct BlockMesh {
    std::uint64_t block_id = 0;
    Rect rect;
    std::uint32_t stride = 1;
    std::vector<MeshVertex> vertices;
    std::vector<std::uint32_t> indices;
    std::uint64_t content_hash = 0;
};

BlockMesh buildBlockMesh(const QuadNode& node, const HeightField& height,
                         const SurfaceCache& surface);
SeamReport verifyMeshSeams(const Quadtree& tree,
                           const std::map<std::uint64_t, BlockMesh>& meshes,
                           float tolerance);

}  // namespace terrain

