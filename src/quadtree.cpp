#include "terrain/quadtree.h"

#include <algorithm>
#include <cmath>
#include <iomanip>
#include <limits>
#include <set>
#include <sstream>

#include "terrain/binary_io.h"
#include "terrain/hash.h"

namespace terrain {
namespace {

bool rectContains(const Rect& outer, const Rect& inner) {
    return outer.x <= inner.x && outer.y <= inner.y &&
           outer.right() >= inner.right() && outer.bottom() >= inner.bottom();
}

std::uint64_t areaKey(const Rect& rect) {
    return static_cast<std::uint64_t>(rect.width) * rect.height;
}

std::uint32_t strideForSize(std::uint32_t width) {
    std::uint32_t stride = 1;
    const std::uint32_t limit = std::max(1u, width / 8u);
    while (stride < 8u && stride * 2u <= limit) {
        stride *= 2u;
    }
    return stride;
}

std::uint32_t lodForStride(std::uint32_t stride) {
    std::uint32_t lod = 0;
    while (stride > 1u) {
        stride >>= 1u;
        ++lod;
    }
    return lod;
}

}  // namespace

std::uint64_t makeBlockId(const Rect& rect) {
    const std::uint64_t x0 = rect.x & 0xffffu;
    const std::uint64_t y0 = rect.y & 0xffffu;
    const std::uint64_t x1 = rect.right() & 0xffffu;
    const std::uint64_t y1 = rect.bottom() & 0xffffu;
    return (x0 << 48u) | (y0 << 32u) | (x1 << 16u) | y1;
}

std::string blockIdString(std::uint64_t id) {
    std::ostringstream stream;
    stream << "0x" << std::hex << std::setw(16) << std::setfill('0') << id;
    return stream.str();
}

QuadNode Quadtree::computeStats(const HeightField& field, const Rect& rect,
                                std::uint32_t depth) const {
    QuadNode node;
    node.id = makeBlockId(rect);
    node.rect = rect;
    node.depth = depth;
    node.min_height = std::numeric_limits<float>::infinity();
    node.max_height = -std::numeric_limits<float>::infinity();
    ByteWriter hash_writer;
    hash_writer.u32(rect.x);
    hash_writer.u32(rect.y);
    hash_writer.u32(rect.width);
    hash_writer.u32(rect.height);
    const std::uint32_t max_x = std::min(rect.right(), field.width - 1u);
    const std::uint32_t max_y = std::min(rect.bottom(), field.height - 1u);
    for (std::uint32_t y = rect.y; y <= max_y; ++y) {
        for (std::uint32_t x = rect.x; x <= max_x; ++x) {
            const float value = field.at(x, y);
            node.min_height = std::min(node.min_height, value);
            node.max_height = std::max(node.max_height, value);
            hash_writer.f32(value);
        }
    }
    if (!std::isfinite(node.min_height)) {
        node.min_height = 0.0f;
        node.max_height = 0.0f;
    }
    node.height_range = node.max_height - node.min_height;
    node.mesh_stride = strideForSize(rect.width);
    node.lod_level = lodForStride(node.mesh_stride);
    node.content_hash = toHex(sha256(hash_writer.data()));
    return node;
}

bool Quadtree::shouldSplit(const QuadNode& node) const {
    const Rect& rect = node.rect;
    const bool can_split =
        rect.width >= config_.min_block_size * 2u &&
        rect.height >= config_.min_block_size * 2u &&
        node.depth < config_.max_depth;
    if (!can_split) {
        return false;
    }
    if (rect.width > config_.block_size ||
        rect.height > config_.block_size) {
        return true;
    }
    return node.height_range > config_.error_threshold;
}

void Quadtree::buildSubtree(const HeightField& field, const Rect& rect,
                            std::uint32_t depth, BuildState* state) const {
    QuadNode node = computeStats(field, rect, depth);
    const std::uint64_t id = node.id;
    state->nodes[id] = node;
    if (!shouldSplit(node)) {
        state->nodes[id].leaf = true;
        state->leaves.push_back(id);
        return;
    }
    state->nodes[id].leaf = false;
    const std::uint32_t w0 = rect.width / 2u;
    const std::uint32_t h0 = rect.height / 2u;
    const std::uint32_t w1 = rect.width - w0;
    const std::uint32_t h1 = rect.height - h0;
    const Rect child_rects[4] = {
        Rect{rect.x, rect.y, w0, h0},
        Rect{rect.x + w0, rect.y, w1, h0},
        Rect{rect.x, rect.y + h0, w0, h1},
        Rect{rect.x + w0, rect.y + h0, w1, h1}};
    for (int i = 0; i < 4; ++i) {
        if (child_rects[i].empty()) continue;
        buildSubtree(field, child_rects[i], depth + 1u, state);
        state->nodes[id].children[static_cast<std::size_t>(i)] =
            makeBlockId(child_rects[i]);
    }
}

Status Quadtree::build(const HeightField& field, const QuadConfig& config) {
    if (field.width < 2 || field.height < 2) {
        return Status::error(E_HEIGHT_INVALID, "dimensions",
                             "terrain must be at least 2x2 vertices");
    }
    if (config.block_size < 2 || config.min_block_size < 2 ||
        config.min_block_size > config.block_size ||
        config.max_depth == 0 ||
        !std::isfinite(config.error_threshold) ||
        config.error_threshold < 0.0f) {
        return Status::error(E_INVALID_ARGUMENT, "quadtree.config",
                             "invalid quadtree configuration");
    }
    config_ = config;
    BuildState state;
    state.field = &field;
    const Rect root{0, 0, field.width - 1u, field.height - 1u};
    root_id_ = makeBlockId(root);
    buildSubtree(field, root, 0, &state);
    nodes_ = std::move(state.nodes);
    leaves_ = std::move(state.leaves);
    rebuildHierarchy(field);
    std::sort(leaves_.begin(), leaves_.end(), [this](std::uint64_t a,
                                                     std::uint64_t b) {
        const QuadNode& na = nodes_.at(a);
        const QuadNode& nb = nodes_.at(b);
        if (na.rect.y != nb.rect.y) return na.rect.y < nb.rect.y;
        if (na.rect.x != nb.rect.x) return na.rect.x < nb.rect.x;
        return na.rect.width < nb.rect.width;
    });
    return Status::success();
}

void Quadtree::rebuildHierarchy(const HeightField& field) {
    std::set<std::uint64_t> keep;
    for (std::uint64_t leaf : leaves_) {
        const auto leaf_it = nodes_.find(leaf);
        if (leaf_it == nodes_.end()) continue;
        keep.insert(leaf);
        for (const auto& entry : nodes_) {
            if (rectContains(entry.second.rect, leaf_it->second.rect)) {
                keep.insert(entry.first);
            }
        }
    }
    if (root_id_ != 0) keep.insert(root_id_);
    for (auto it = nodes_.begin(); it != nodes_.end();) {
        if (keep.find(it->first) == keep.end()) {
            it = nodes_.erase(it);
        } else {
            ++it;
        }
    }
    std::vector<std::uint64_t> ids;
    ids.reserve(nodes_.size());
    for (const auto& entry : nodes_) {
        ids.push_back(entry.first);
    }
    std::sort(ids.begin(), ids.end(), [this](std::uint64_t a, std::uint64_t b) {
        return areaKey(nodes_.at(a).rect) > areaKey(nodes_.at(b).rect);
    });

    for (auto& entry : nodes_) {
        entry.second.parent = 0;
        entry.second.children.fill(0);
        entry.second.leaf = false;
    }
    for (std::uint64_t leaf : leaves_) {
        const auto it = nodes_.find(leaf);
        if (it != nodes_.end()) it->second.leaf = true;
    }
    for (std::uint64_t id : ids) {
        QuadNode& node = nodes_.at(id);
        if (id == root_id_) {
            node.depth = 0;
            continue;
        }
        std::uint64_t best_parent = 0;
        std::uint64_t best_area = std::numeric_limits<std::uint64_t>::max();
        for (std::uint64_t candidate_id : ids) {
            if (candidate_id == id) continue;
            const QuadNode& candidate = nodes_.at(candidate_id);
            if (!rectContains(candidate.rect, node.rect)) continue;
            const std::uint64_t area = areaKey(candidate.rect);
            if (area < best_area ||
                (area == best_area &&
                 (best_parent == 0 ||
                  candidate.rect.width <
                      nodes_.at(best_parent).rect.width))) {
                best_parent = candidate_id;
                best_area = area;
            }
        }
        if (best_parent != 0) {
            node.parent = best_parent;
            QuadNode& parent = nodes_.at(best_parent);
            for (auto& child : parent.children) {
                if (child == 0) {
                    child = id;
                    break;
                }
            }
        }
    }
    for (std::uint64_t id : ids) {
        QuadNode& node = nodes_.at(id);
        std::uint32_t depth = 0;
        std::uint64_t parent = node.parent;
        std::uint32_t guard = 0;
        while (parent != 0 && guard++ < config_.max_depth + 2u) {
            ++depth;
            parent = nodes_.at(parent).parent;
        }
        node.depth = depth;
        const QuadNode fresh = computeStats(field, node.rect, depth);
        node.min_height = fresh.min_height;
        node.max_height = fresh.max_height;
        node.height_range = fresh.height_range;
        node.mesh_stride = fresh.mesh_stride;
        node.lod_level = fresh.lod_level;
        node.content_hash = fresh.content_hash;
    }
}

Status Quadtree::rebuildIncremental(const HeightField& field,
                                    const Rect& changed,
                                    bool include_neighbors,
                                    LodRebuildReport* report) {
    if (nodes_.empty() || leaves_.empty()) {
        return Status::error(E_INVALID_ARGUMENT, "quadtree",
                             "quadtree has not been built");
    }
    LodRebuildReport local;
    std::vector<std::uint64_t> dirty;
    for (std::uint64_t id : leaves_) {
        const QuadNode& leaf = nodes_.at(id);
        if (leaf.rect.intersects(changed)) {
            dirty.push_back(id);
            local.reasons.push_back("height range intersects patch for " +
                                    blockIdString(id));
        }
    }
    if (dirty.empty()) {
        std::vector<std::uint64_t> seeds = blocksIntersecting(changed);
        dirty.insert(dirty.end(), seeds.begin(), seeds.end());
    }
    if (include_neighbors) {
        std::vector<std::uint64_t> initial = dirty;
        for (std::uint64_t id : initial) {
            for (std::uint64_t neighbor : neighbors(id)) {
                if (std::find(dirty.begin(), dirty.end(), neighbor) ==
                    dirty.end()) {
                    dirty.push_back(neighbor);
                    local.reasons.push_back(
                        "shared-boundary invalidation for " +
                        blockIdString(neighbor));
                }
            }
        }
    }
    std::sort(dirty.begin(), dirty.end());
    dirty.erase(std::unique(dirty.begin(), dirty.end()), dirty.end());
    local.dirty_blocks = dirty;

    std::vector<std::uint64_t> roots;
    for (std::uint64_t id : dirty) {
        const QuadNode& leaf = nodes_.at(id);
        std::uint64_t root = id;
        if (leaf.parent != 0) {
            const auto parent_it = nodes_.find(leaf.parent);
            if (parent_it != nodes_.end() &&
                parent_it->second.id != root_id_) {
                root = leaf.parent;
            }
        }
        if (std::find(roots.begin(), roots.end(), root) == roots.end()) {
            roots.push_back(root);
        }
    }
    std::sort(roots.begin(), roots.end(), [this](std::uint64_t a,
                                                 std::uint64_t b) {
        return areaKey(nodes_.at(a).rect) > areaKey(nodes_.at(b).rect);
    });
    std::vector<std::uint64_t> maximal_roots;
    for (std::uint64_t root : roots) {
        bool contained = false;
        for (std::uint64_t existing : maximal_roots) {
            if (rectContains(nodes_.at(existing).rect,
                             nodes_.at(root).rect)) {
                contained = true;
                break;
            }
        }
        if (!contained) maximal_roots.push_back(root);
    }
    roots = std::move(maximal_roots);

    std::vector<std::uint64_t> preserved;
    std::vector<std::uint64_t> replaced_old_leaves;
    for (std::uint64_t id : leaves_) {
        bool replaced = false;
        for (std::uint64_t root : roots) {
            if (rectContains(nodes_.at(root).rect, nodes_.at(id).rect)) {
                replaced = true;
                break;
            }
        }
        if (replaced) {
            replaced_old_leaves.push_back(id);
        } else {
            preserved.push_back(id);
        }
    }
    std::unordered_map<std::uint64_t, QuadNode> next_nodes = nodes_;
    std::vector<std::uint64_t> next_leaves = preserved;
    std::vector<std::uint64_t> rebuilt_leaf_ids;
    for (std::uint64_t root_id : roots) {
        const QuadNode root = nodes_.at(root_id);
        for (auto it = next_nodes.begin(); it != next_nodes.end();) {
            if (it->first != root_id &&
                rectContains(root.rect, it->second.rect)) {
                it = next_nodes.erase(it);
            } else {
                ++it;
            }
        }
        BuildState state;
        state.field = &field;
        buildSubtree(field, root.rect, root.depth, &state);
        ++local.rebuilt_subtrees;
        for (auto& entry : state.nodes) {
            next_nodes[entry.first] = entry.second;
        }
        for (std::uint64_t leaf : state.leaves) {
            next_leaves.push_back(leaf);
            rebuilt_leaf_ids.push_back(leaf);
        }
    }
    std::sort(next_leaves.begin(), next_leaves.end());
    next_leaves.erase(std::unique(next_leaves.begin(), next_leaves.end()),
                      next_leaves.end());
    std::sort(rebuilt_leaf_ids.begin(), rebuilt_leaf_ids.end());
    rebuilt_leaf_ids.erase(
        std::unique(rebuilt_leaf_ids.begin(), rebuilt_leaf_ids.end()),
        rebuilt_leaf_ids.end());

    for (std::uint64_t id : next_leaves) {
        if (std::find(leaves_.begin(), leaves_.end(), id) == leaves_.end()) {
            local.new_blocks.push_back(id);
        }
    }
    for (std::uint64_t id : leaves_) {
        if (std::find(next_leaves.begin(), next_leaves.end(), id) ==
            next_leaves.end()) {
            local.removed_blocks.push_back(id);
        }
    }
    for (std::uint64_t id : replaced_old_leaves) {
        local.dirty_blocks.push_back(id);
    }
    for (std::uint64_t id : rebuilt_leaf_ids) {
        local.dirty_blocks.push_back(id);
    }
    std::sort(local.dirty_blocks.begin(), local.dirty_blocks.end());
    local.dirty_blocks.erase(
        std::unique(local.dirty_blocks.begin(), local.dirty_blocks.end()),
        local.dirty_blocks.end());

    nodes_ = std::move(next_nodes);
    leaves_ = std::move(next_leaves);
    for (std::uint64_t id : leaves_) {
        nodes_[id].leaf = true;
    }
    rebuildHierarchy(field);
    std::sort(leaves_.begin(), leaves_.end(), [this](std::uint64_t a,
                                                     std::uint64_t b) {
        const QuadNode& na = nodes_.at(a);
        const QuadNode& nb = nodes_.at(b);
        if (na.rect.y != nb.rect.y) return na.rect.y < nb.rect.y;
        if (na.rect.x != nb.rect.x) return na.rect.x < nb.rect.x;
        return na.rect.width < nb.rect.width;
    });
    std::vector<std::uint64_t> adjacency;
    for (std::uint64_t id : local.dirty_blocks) {
        if (nodes_.find(id) == nodes_.end()) continue;
        for (std::uint64_t neighbor : neighbors(id)) {
            adjacency.push_back(neighbor);
        }
    }
    std::sort(adjacency.begin(), adjacency.end());
    adjacency.erase(std::unique(adjacency.begin(), adjacency.end()),
                    adjacency.end());
    local.neighbor_blocks = adjacency;
    if (report != nullptr) {
        *report = std::move(local);
    }
    return Status::success();
}

const QuadNode* Quadtree::node(std::uint64_t id) const {
    const auto it = nodes_.find(id);
    return it == nodes_.end() ? nullptr : &it->second;
}

bool Quadtree::sharesEdge(const Rect& a, const Rect& b, bool* vertical) {
    const bool y_overlap =
        std::max(a.y, b.y) < std::min(a.bottom(), b.bottom());
    const bool x_overlap =
        std::max(a.x, b.x) < std::min(a.right(), b.right());
    if (y_overlap && (a.right() == b.x || b.right() == a.x)) {
        *vertical = true;
        return true;
    }
    if (x_overlap && (a.bottom() == b.y || b.bottom() == a.y)) {
        *vertical = false;
        return true;
    }
    return false;
}

std::vector<std::uint64_t> Quadtree::neighbors(std::uint64_t id) const {
    std::vector<std::uint64_t> result;
    const QuadNode* target = node(id);
    if (target == nullptr) return result;
    for (std::uint64_t other_id : leaves_) {
        if (other_id == id) continue;
        const QuadNode* other = node(other_id);
        if (other == nullptr) continue;
        bool vertical = true;
        if (sharesEdge(target->rect, other->rect, &vertical)) {
            result.push_back(other_id);
        }
    }
    return result;
}

std::vector<std::uint64_t> Quadtree::blocksIntersecting(const Rect& rect) const {
    std::vector<std::uint64_t> result;
    for (std::uint64_t id : leaves_) {
        const QuadNode& leaf = nodes_.at(id);
        if (leaf.rect.intersects(rect) || leaf.rect.touches(rect)) {
            result.push_back(id);
        }
    }
    return result;
}

SeamReport Quadtree::verifySeams(const HeightField& field,
                                 float tolerance) const {
    SeamReport report;
    report.tolerance = tolerance;
    if (!std::isfinite(tolerance) || tolerance < 0.0f) {
        report.status = Status::error(E_INVALID_ARGUMENT, "tolerance",
                                      "seam tolerance must be non-negative");
        return report;
    }
    for (std::size_t i = 0; i < leaves_.size(); ++i) {
        for (std::size_t j = i + 1; j < leaves_.size(); ++j) {
            const QuadNode& a = nodes_.at(leaves_[i]);
            const QuadNode& b = nodes_.at(leaves_[j]);
            bool vertical = true;
            if (!sharesEdge(a.rect, b.rect, &vertical)) continue;
            SeamCheck check;
            check.block_a = a.id;
            check.block_b = b.id;
            check.vertical = vertical;
            if (vertical) {
                check.overlap_begin = std::max(a.rect.y, b.rect.y);
                check.overlap_end = std::min(a.rect.bottom(), b.rect.bottom());
                const std::uint32_t x = a.rect.right() == b.rect.x
                                            ? a.rect.right()
                                            : b.rect.right();
                for (std::uint32_t y = check.overlap_begin;
                     y <= check.overlap_end; ++y) {
                    const float ha = field.at(std::min(x, field.width - 1u),
                                              std::min(y, field.height - 1u));
                    const float hb = field.at(std::min(x, field.width - 1u),
                                              std::min(y, field.height - 1u));
                    check.max_deviation =
                        std::max(check.max_deviation, std::fabs(ha - hb));
                    ++check.sample_count;
                }
            } else {
                check.overlap_begin = std::max(a.rect.x, b.rect.x);
                check.overlap_end = std::min(a.rect.right(), b.rect.right());
                const std::uint32_t y = a.rect.bottom() == b.rect.y
                                            ? a.rect.bottom()
                                            : b.rect.bottom();
                for (std::uint32_t x = check.overlap_begin;
                     x <= check.overlap_end; ++x) {
                    const float ha = field.at(std::min(x, field.width - 1u),
                                              std::min(y, field.height - 1u));
                    const float hb = field.at(std::min(x, field.width - 1u),
                                              std::min(y, field.height - 1u));
                    check.max_deviation =
                        std::max(check.max_deviation, std::fabs(ha - hb));
                    ++check.sample_count;
                }
            }
            report.max_deviation =
                std::max(report.max_deviation, check.max_deviation);
            if (check.max_deviation > tolerance) {
                if (report.status.ok()) {
                    report.status = Status::crack(
                        "seam vertical deviation exceeds tolerance",
                        check.block_a, check.block_b);
                }
            }
            report.seams.push_back(check);
        }
    }
    if (report.status.ok()) {
        report.status = Status::success();
    }
    return report;
}

}  // namespace terrain

