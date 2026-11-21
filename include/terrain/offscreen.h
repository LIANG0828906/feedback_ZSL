#pragma once

#include <cstdint>
#include <map>
#include <string>
#include <vector>

#include "terrain/mesh.h"
#include "terrain/quadtree.h"
#include "terrain/types.h"

namespace terrain {

struct RenderSummary {
    std::uint64_t patch_sequence = 0;
    std::uint32_t invalid_blocks = 0;
    std::uint32_t rerendered_blocks = 0;
    std::uint32_t reused_blocks = 0;
    float error_upper_bound = 0.0f;
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    std::string color_hash;
    std::string heat_hash;
    bool full_frame = false;

    std::string stableString() const;
};

struct OffscreenFrame {
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    std::vector<std::uint8_t> color;  // RGBA8
    std::vector<std::uint16_t> heat;  // normalized deterministic heat
};

class SoftwareOffscreenRenderer {
public:
    explicit SoftwareOffscreenRenderer(Camera camera = {});

    RenderSummary renderFull(const Quadtree& tree, const HeightField& height,
                             const SurfaceCache& surface,
                             const std::map<std::uint64_t, BlockMesh>& meshes,
                             std::uint64_t patch_sequence);
    RenderSummary renderIncremental(
        const Quadtree& tree, const HeightField& height,
        const SurfaceCache& surface,
        const std::map<std::uint64_t, BlockMesh>& meshes,
        const std::vector<std::uint64_t>& dirty_blocks,
        std::uint32_t invalid_block_count, std::uint64_t patch_sequence);

    const OffscreenFrame& frame() const { return frame_; }
    const RenderSummary& lastSummary() const { return last_summary_; }

private:
    struct Tile {
        Rect rect;
        std::uint32_t scale = 1;
        std::vector<std::uint8_t> color;
        std::vector<std::uint16_t> heat;
        float max_error = 0.0f;
    };

    RenderSummary renderInternal(
        const Quadtree& tree, const HeightField& height,
        const SurfaceCache& surface,
        const std::map<std::uint64_t, BlockMesh>& meshes,
        const std::vector<std::uint64_t>& dirty_blocks,
        std::uint32_t invalid_block_count, std::uint64_t patch_sequence,
        bool full);
    Tile rasterizeTile(const BlockMesh& mesh, const HeightField& height) const;
    void composite(const Quadtree& tree);
    float sampleHeightBilinear(const HeightField& height, float x,
                               float y) const;

    Camera camera_;
    std::map<std::uint64_t, Tile> tiles_;
    OffscreenFrame frame_;
    RenderSummary last_summary_;
};

}  // namespace terrain

