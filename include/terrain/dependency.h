#pragma once

#include <cstdint>
#include <map>
#include <string>
#include <unordered_map>
#include <unordered_set>
#include <vector>

#include "terrain/quadtree.h"
#include "terrain/types.h"

namespace terrain {

enum class DependencyKind {
    Height,
    Material,
    LodDecision,
    Normal,
    MaterialBlend,
    Seam,
    Offscreen
};

const char* dependencyKindName(DependencyKind kind);

struct DependencyNode {
    std::string id;
    DependencyKind kind = DependencyKind::Height;
    std::uint64_t block = 0;
    std::uint64_t other_block = 0;

    bool operator<(const DependencyNode& other) const {
        return id < other.id;
    }
};

struct InvalidationReport {
    std::vector<DependencyNode> invalid_nodes;
    std::vector<std::uint64_t> invalid_blocks;
    std::vector<std::uint64_t> reuse_blocks;
    std::vector<std::uint64_t> render_blocks;
    std::map<std::uint64_t, std::vector<std::string>> block_reasons;
    std::vector<std::string> flat_reasons;
    std::uint32_t dependency_edge_count = 0;
    bool fallback_full = false;
    bool conservative = true;

    std::string reasonFor(std::uint64_t block) const;
};

class DependencyGraph {
public:
    void build(const Quadtree& tree);
    void invalidate(const Quadtree& tree, const Rect& changed_height,
                    const Rect* changed_material,
                    const std::vector<std::uint64_t>& forced_blocks,
                    InvalidationReport* report) const;

    const std::unordered_map<std::string,
                             std::vector<std::string>>& edges() const {
        return edges_;
    }
    std::size_t nodeCount() const { return nodes_.size(); }

private:
    void addNode(const DependencyNode& node);
    void addEdge(const std::string& from, const std::string& to);
    static std::string makeId(DependencyKind kind, std::uint64_t block,
                              std::uint64_t other = 0);

    std::unordered_map<std::string, DependencyNode> nodes_;
    std::unordered_map<std::string, std::vector<std::string>> edges_;
};

}  // namespace terrain

