#include "terrain/dependency.h"

#include <algorithm>
#include <deque>
#include <set>
#include <sstream>

namespace terrain {

const char* dependencyKindName(DependencyKind kind) {
    switch (kind) {
        case DependencyKind::Height:
            return "height";
        case DependencyKind::Material:
            return "material";
        case DependencyKind::LodDecision:
            return "lod";
        case DependencyKind::Normal:
            return "normal";
        case DependencyKind::MaterialBlend:
            return "material-blend";
        case DependencyKind::Seam:
            return "seam";
        case DependencyKind::Offscreen:
            return "offscreen";
    }
    return "unknown";
}

std::string InvalidationReport::reasonFor(std::uint64_t block) const {
    const auto it = block_reasons.find(block);
    if (it == block_reasons.end() || it->second.empty()) {
        return "dependency closure";
    }
    std::string out;
    for (const std::string& reason : it->second) {
        if (!out.empty()) out += "; ";
        out += reason;
    }
    return out;
}

std::string DependencyGraph::makeId(DependencyKind kind,
                                    std::uint64_t block,
                                    std::uint64_t other) {
    std::ostringstream stream;
    stream << dependencyKindName(kind) << ":" << std::hex << block;
    if (other != 0) {
        stream << ":" << other;
    }
    return stream.str();
}

void DependencyGraph::addNode(const DependencyNode& node) {
    nodes_.emplace(node.id, node);
    edges_.emplace(node.id, std::vector<std::string>{});
}

void DependencyGraph::addEdge(const std::string& from, const std::string& to) {
    if (nodes_.find(from) == nodes_.end() || nodes_.find(to) == nodes_.end()) {
        return;
    }
    auto& targets = edges_[from];
    if (std::find(targets.begin(), targets.end(), to) == targets.end()) {
        targets.push_back(to);
    }
}

void DependencyGraph::build(const Quadtree& tree) {
    nodes_.clear();
    edges_.clear();
    const auto& leaves = tree.leaves();
    for (std::uint64_t id : leaves) {
        addNode({makeId(DependencyKind::Height, id), DependencyKind::Height,
                 id, 0});
        addNode({makeId(DependencyKind::Material, id),
                 DependencyKind::Material, id, 0});
        addNode({makeId(DependencyKind::LodDecision, id),
                 DependencyKind::LodDecision, id, 0});
        addNode({makeId(DependencyKind::Normal, id), DependencyKind::Normal, id,
                 0});
        addNode({makeId(DependencyKind::MaterialBlend, id),
                 DependencyKind::MaterialBlend, id, 0});
        addNode({makeId(DependencyKind::Offscreen, id),
                 DependencyKind::Offscreen, id, 0});

        addEdge(makeId(DependencyKind::Height, id),
                makeId(DependencyKind::LodDecision, id));
        addEdge(makeId(DependencyKind::Height, id),
                makeId(DependencyKind::Normal, id));
        addEdge(makeId(DependencyKind::LodDecision, id),
                makeId(DependencyKind::Normal, id));
        addEdge(makeId(DependencyKind::Normal, id),
                makeId(DependencyKind::Offscreen, id));
        addEdge(makeId(DependencyKind::Material, id),
                makeId(DependencyKind::MaterialBlend, id));
        addEdge(makeId(DependencyKind::MaterialBlend, id),
                makeId(DependencyKind::Offscreen, id));
        addEdge(makeId(DependencyKind::LodDecision, id),
                makeId(DependencyKind::Offscreen, id));
    }
    for (std::uint64_t id : leaves) {
        const std::vector<std::uint64_t> adjacent = tree.neighbors(id);
        for (std::uint64_t other : adjacent) {
            const std::uint64_t a = std::min(id, other);
            const std::uint64_t b = std::max(id, other);
            const std::string seam =
                makeId(DependencyKind::Seam, a, b);
            if (nodes_.find(seam) == nodes_.end()) {
                addNode({seam, DependencyKind::Seam, a, b});
            }
            addEdge(makeId(DependencyKind::LodDecision, a), seam);
            addEdge(makeId(DependencyKind::LodDecision, b), seam);
            addEdge(seam, makeId(DependencyKind::Offscreen, a));
            addEdge(seam, makeId(DependencyKind::Offscreen, b));
            addEdge(makeId(DependencyKind::Height, a),
                    makeId(DependencyKind::Normal, b));
            addEdge(makeId(DependencyKind::Height, b),
                    makeId(DependencyKind::Normal, a));
        }
    }
}

void DependencyGraph::invalidate(
    const Quadtree& tree, const Rect& changed_height,
    const Rect* changed_material,
    const std::vector<std::uint64_t>& forced_blocks,
    InvalidationReport* report) const {
    InvalidationReport local;
    std::deque<std::string> queue;
    std::unordered_set<std::string> visited;
    std::map<std::string, std::string> reason_by_id;
    auto enqueue = [&](const std::string& id, const std::string& reason) {
        if (nodes_.find(id) == nodes_.end()) return;
        if (visited.find(id) != visited.end()) return;
        visited.insert(id);
        reason_by_id[id] = reason;
        queue.push_back(id);
    };

    for (std::uint64_t block : tree.leaves()) {
        const QuadNode* node = tree.node(block);
        if (node == nullptr) continue;
        if (node->rect.intersects(changed_height)) {
            enqueue(makeId(DependencyKind::Height, block),
                    "height rectangle intersects block");
        }
        if (changed_material != nullptr &&
            node->rect.intersects(*changed_material)) {
            enqueue(makeId(DependencyKind::Material, block),
                    "material rectangle intersects block");
        }
    }
    for (std::uint64_t block : forced_blocks) {
        enqueue(makeId(DependencyKind::Height, block),
                "conservative geometry/neighbor rebuild");
        enqueue(makeId(DependencyKind::LodDecision, block),
                "conservative geometry/neighbor rebuild");
    }

    while (!queue.empty()) {
        const std::string current = queue.front();
        queue.pop_front();
        const auto edge_it = edges_.find(current);
        if (edge_it == edges_.end()) continue;
        for (const std::string& dependent : edge_it->second) {
            if (visited.find(dependent) != visited.end()) continue;
            visited.insert(dependent);
            reason_by_id[dependent] =
                "dependency closure through " + current;
            queue.push_back(dependent);
        }
    }
    std::vector<DependencyNode> invalid_nodes;
    invalid_nodes.reserve(visited.size());
    for (const std::string& id : visited) {
        const auto it = nodes_.find(id);
        if (it != nodes_.end()) {
            invalid_nodes.push_back(it->second);
        }
    }
    std::sort(invalid_nodes.begin(), invalid_nodes.end());

    std::set<std::uint64_t> blocks;
    for (const DependencyNode& node : invalid_nodes) {
        blocks.insert(node.block);
        if (node.other_block != 0) {
            blocks.insert(node.other_block);
        }
        const std::string reason = reason_by_id[node.id];
        local.block_reasons[node.block].push_back(
            std::string(dependencyKindName(node.kind)) + ": " + reason);
        if (node.other_block != 0) {
            local.block_reasons[node.other_block].push_back(
                std::string(dependencyKindName(node.kind)) + ": " + reason);
        }
        local.flat_reasons.push_back(node.id + " <- " + reason);
    }
    for (auto& entry : local.block_reasons) {
        std::sort(entry.second.begin(), entry.second.end());
        entry.second.erase(std::unique(entry.second.begin(),
                                       entry.second.end()),
                           entry.second.end());
    }
    std::sort(local.flat_reasons.begin(), local.flat_reasons.end());
    local.flat_reasons.erase(
        std::unique(local.flat_reasons.begin(), local.flat_reasons.end()),
        local.flat_reasons.end());
    local.invalid_blocks.assign(blocks.begin(), blocks.end());
    local.render_blocks = local.invalid_blocks;
    local.fallback_full = false;
    local.conservative = true;
    local.dependency_edge_count = 0;
    for (const auto& entry : edges_) {
        local.dependency_edge_count +=
            static_cast<std::uint32_t>(entry.second.size());
    }
    local.invalid_nodes = std::move(invalid_nodes);
    *report = std::move(local);
}

}  // namespace terrain

