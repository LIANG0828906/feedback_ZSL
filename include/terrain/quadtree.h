#pragma once

#include <array>
#include <cstdint>
#include <map>
#include <string>
#include <unordered_map>
#include <vector>

#include "terrain/status.h"
#include "terrain/types.h"

namespace terrain {

struct QuadConfig {
    std::uint32_t block_size = 32;
    std::uint32_t min_block_size = 8;
    std::uint32_t max_depth = 8;
    float error_threshold = 3.0f;
};

struct QuadNode {
    std::uint64_t id = 0;
    Rect rect;
    std::uint32_t depth = 0;
    std::uint64_t parent = 0;
    std::array<std::uint64_t, 4> children{};
    bool leaf = true;
    float min_height = 0.0f;
    float max_height = 0.0f;
    float height_range = 0.0f;
    std::uint32_t mesh_stride = 1;
    std::uint32_t lod_level = 0;
    std::string content_hash;
};

struct SeamCheck {
    std::uint64_t block_a = 0;
    std::uint64_t block_b = 0;
    bool vertical = true;
    std::uint32_t overlap_begin = 0;
    std::uint32_t overlap_end = 0;
    float max_deviation = 0.0f;
    std::uint32_t sample_count = 0;
    bool closed_by_matched_vertices = true;
};

struct SeamReport {
    std::vector<SeamCheck> seams;
    float max_deviation = 0.0f;
    float tolerance = 0.0f;
    Status status;
    bool clean() const { return status.ok(); }
};

struct LodRebuildReport {
    std::vector<std::uint64_t> dirty_blocks;
    std::vector<std::uint64_t> neighbor_blocks;
    std::vector<std::uint64_t> new_blocks;
    std::vector<std::uint64_t> removed_blocks;
    std::uint32_t rebuilt_subtrees = 0;
    bool used_full_rebuild = false;
    std::vector<std::string> reasons;
};

std::uint64_t makeBlockId(const Rect& rect);
std::string blockIdString(std::uint64_t id);

class Quadtree {
public:
    Status build(const HeightField& field, const QuadConfig& config);
    Status rebuildIncremental(const HeightField& field, const Rect& changed,
                              bool include_neighbors,
                              LodRebuildReport* report);
    SeamReport verifySeams(const HeightField& field, float tolerance) const;

    const std::vector<std::uint64_t>& leaves() const { return leaves_; }
    const std::unordered_map<std::uint64_t, QuadNode>& nodes() const {
        return nodes_;
    }
    const QuadConfig& config() const { return config_; }
    bool containsBlock(std::uint64_t id) const {
        return nodes_.find(id) != nodes_.end();
    }
    const QuadNode* node(std::uint64_t id) const;
    std::vector<std::uint64_t> neighbors(std::uint64_t id) const;
    std::vector<std::uint64_t> blocksIntersecting(const Rect& rect) const;
    std::uint64_t rootId() const { return root_id_; }

private:
    struct BuildState {
        const HeightField* field = nullptr;
        std::unordered_map<std::uint64_t, QuadNode> nodes;
        std::vector<std::uint64_t> leaves;
    };

    QuadNode computeStats(const HeightField& field, const Rect& rect,
                          std::uint32_t depth) const;
    bool shouldSplit(const QuadNode& node) const;
    void buildSubtree(const HeightField& field, const Rect& rect,
                      std::uint32_t depth, BuildState* state) const;
    void rebuildHierarchy(const HeightField& field);
    static bool sharesEdge(const Rect& a, const Rect& b, bool* vertical);

    QuadConfig config_;
    std::unordered_map<std::uint64_t, QuadNode> nodes_;
    std::vector<std::uint64_t> leaves_;
    std::uint64_t root_id_ = 0;
};

}  // namespace terrain

