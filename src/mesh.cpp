#include "terrain/mesh.h"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <functional>
#include <limits>
#include <unordered_map>

#include "terrain/hash.h"

namespace terrain {
namespace {

std::uint64_t vertexKey(std::uint32_t x, std::uint32_t y) {
    return (static_cast<std::uint64_t>(x) << 32u) | y;
}

class Builder {
public:
    Builder(const HeightField& height, const SurfaceCache& surface)
        : height_(height), surface_(surface) {}

    std::uint32_t add(std::uint32_t x, std::uint32_t y) {
        const std::uint64_t key = vertexKey(x, y);
        const auto found = map_.find(key);
        if (found != map_.end()) return found->second;
        MeshVertex vertex;
        vertex.grid_x = x;
        vertex.grid_y = y;
        vertex.position = {static_cast<float>(x) * height_.cell_spacing,
                           height_.at(x, y),
                           static_cast<float>(y) * height_.cell_spacing};
        vertex.normal = surface_.normalAt(x, y);
        vertex.material_color = surface_.materialColorAt(x, y);
        const std::uint32_t index =
            static_cast<std::uint32_t>(vertices.size());
        vertices.push_back(vertex);
        map_[key] = index;
        return index;
    }

    void triangle(std::uint32_t a, std::uint32_t b, std::uint32_t c) {
        if (a == b || b == c || a == c) return;
        indices.push_back(a);
        indices.push_back(b);
        indices.push_back(c);
    }

    std::vector<MeshVertex> vertices;
    std::vector<std::uint32_t> indices;

private:
    const HeightField& height_;
    const SurfaceCache& surface_;
    std::unordered_map<std::uint64_t, std::uint32_t> map_;
};

void connectStrips(const std::vector<std::uint32_t>& outer,
                   const std::vector<std::uint32_t>& inner, Builder* builder) {
    if (outer.size() < 2 || inner.empty()) return;
    const bool along_x =
        builder->vertices[outer.front()].grid_y ==
            builder->vertices[outer.back()].grid_y &&
        builder->vertices[outer.front()].grid_x !=
            builder->vertices[outer.back()].grid_x;
    const auto index_on_outer = [&](std::uint32_t inner_index) {
        const MeshVertex& inner_vertex = builder->vertices[inner_index];
        const std::uint32_t projected =
            along_x ? inner_vertex.grid_x : inner_vertex.grid_y;
        for (std::uint32_t i = 0; i < outer.size(); ++i) {
            const MeshVertex& outer_vertex = builder->vertices[outer[i]];
            const std::uint32_t candidate =
                along_x ? outer_vertex.grid_x : outer_vertex.grid_y;
            if (candidate == projected) {
                return i;
            }
        }
        return static_cast<std::uint32_t>(outer.size() - 1u);
    };

    const std::uint32_t first = index_on_outer(inner.front());
    for (std::uint32_t i = 0; i < first; ++i) {
        builder->triangle(outer[i], outer[i + 1u], inner.front());
    }
    for (std::size_t j = 0; j + 1u < inner.size(); ++j) {
        const std::uint32_t start = index_on_outer(inner[j]);
        const std::uint32_t end = index_on_outer(inner[j + 1u]);
        for (std::uint32_t i = start; i < end; ++i) {
            builder->triangle(outer[i], outer[i + 1u], inner[j]);
        }
        builder->triangle(outer[end], inner[j + 1u], inner[j]);
    }
    const std::uint32_t last = index_on_outer(inner.back());
    for (std::uint32_t i = last; i + 1u < outer.size(); ++i) {
        builder->triangle(outer[i], outer[i + 1u], inner.back());
    }
}

std::uint64_t meshHash(const MeshVertex& vertex) {
    std::uint64_t h = 1469598103934665603ull;
    const auto mix = [&h](std::uint32_t value) {
        h ^= value;
        h *= 1099511628211ull;
    };
    mix(vertex.grid_x);
    mix(vertex.grid_y);
    std::uint32_t bits = 0;
    std::memcpy(&bits, &vertex.position.y, sizeof(bits));
    mix(bits);
    std::memcpy(&bits, &vertex.normal.x, sizeof(bits));
    mix(bits);
    std::memcpy(&bits, &vertex.normal.y, sizeof(bits));
    mix(bits);
    std::memcpy(&bits, &vertex.normal.z, sizeof(bits));
    mix(bits);
    std::memcpy(&bits, &vertex.material_color.x, sizeof(bits));
    mix(bits);
    std::memcpy(&bits, &vertex.material_color.y, sizeof(bits));
    mix(bits);
    std::memcpy(&bits, &vertex.material_color.z, sizeof(bits));
    mix(bits);
    return h;
}

}  // namespace

BlockMesh buildBlockMesh(const QuadNode& node, const HeightField& height,
                         const SurfaceCache& surface) {
    BlockMesh mesh;
    mesh.block_id = node.id;
    mesh.rect = node.rect;
    mesh.stride = std::max(1u, node.mesh_stride);
    Builder builder(height, surface);
    const std::uint32_t x0 = node.rect.x;
    const std::uint32_t y0 = node.rect.y;
    const std::uint32_t x1 = std::min(node.rect.right(), height.width - 1u);
    const std::uint32_t y1 = std::min(node.rect.bottom(), height.height - 1u);
    const std::uint32_t s = mesh.stride;

    std::vector<std::uint32_t> bottom;
    std::vector<std::uint32_t> top;
    std::vector<std::uint32_t> left;
    std::vector<std::uint32_t> right;
    for (std::uint32_t x = x0; x <= x1; ++x) {
        bottom.push_back(builder.add(x, y0));
        top.push_back(builder.add(x, y1));
    }
    for (std::uint32_t y = y0 + 1u; y < y1; ++y) {
        left.push_back(builder.add(x0, y));
        right.push_back(builder.add(x1, y));
    }

    std::vector<std::uint32_t> bottom_inner;
    std::vector<std::uint32_t> top_inner;
    std::vector<std::uint32_t> left_inner;
    std::vector<std::uint32_t> right_inner;
    if (x1 > x0 + 2u * s && y1 > y0 + 2u * s) {
        bottom_inner.push_back(builder.add(x0, y0 + s));
        top_inner.push_back(builder.add(x0, y1 - s));
        for (std::uint32_t x = x0 + s; x <= x1 - s; x += s) {
            bottom_inner.push_back(builder.add(x, y0 + s));
            top_inner.push_back(builder.add(x, y1 - s));
        }
        bottom_inner.push_back(builder.add(x1, y0 + s));
        top_inner.push_back(builder.add(x1, y1 - s));
        for (std::uint32_t y = y0 + s; y <= y1 - s; y += s) {
            left_inner.push_back(builder.add(x0 + s, y));
            right_inner.push_back(builder.add(x1 - s, y));
        }
        connectStrips(bottom, bottom_inner, &builder);
        connectStrips(top, top_inner, &builder);
        std::vector<std::uint32_t> left_outer;
        for (std::uint32_t y = y0 + s; y <= y1 - s; ++y) {
            left_outer.push_back(builder.add(x0, y));
        }
        std::vector<std::uint32_t> right_outer;
        for (std::uint32_t y = y0 + s; y <= y1 - s; ++y) {
            right_outer.push_back(builder.add(x1, y));
        }
        connectStrips(left_outer, left_inner, &builder);
        connectStrips(right_outer, right_inner, &builder);

        for (std::uint32_t y = y0 + s; y + s <= y1 - s; y += s) {
            for (std::uint32_t x = x0 + s; x + s <= x1 - s; x += s) {
                const std::uint32_t a = builder.add(x, y);
                const std::uint32_t b = builder.add(x + s, y);
                const std::uint32_t c = builder.add(x, y + s);
                const std::uint32_t d = builder.add(x + s, y + s);
                builder.triangle(a, c, b);
                builder.triangle(b, c, d);
            }
        }
    } else {
        for (std::uint32_t y = y0; y < y1; ++y) {
            for (std::uint32_t x = x0; x < x1; ++x) {
                const std::uint32_t a = builder.add(x, y);
                const std::uint32_t b = builder.add(x + 1u, y);
                const std::uint32_t c = builder.add(x, y + 1u);
                const std::uint32_t d = builder.add(x + 1u, y + 1u);
                builder.triangle(a, c, b);
                builder.triangle(b, c, d);
            }
        }
    }
    mesh.vertices = std::move(builder.vertices);
    mesh.indices = std::move(builder.indices);
    std::uint64_t hash = 1469598103934665603ull;
    for (const MeshVertex& vertex : mesh.vertices) {
        hash ^= meshHash(vertex);
        hash *= 1099511628211ull;
    }
    for (std::uint32_t index : mesh.indices) {
        hash ^= index;
        hash *= 1099511628211ull;
    }
    mesh.content_hash = hash;
    return mesh;
}

SeamReport verifyMeshSeams(
    const Quadtree& tree,
    const std::map<std::uint64_t, BlockMesh>& meshes, float tolerance) {
    SeamReport report;
    report.tolerance = tolerance;
    if (!std::isfinite(tolerance) || tolerance < 0.0f) {
        report.status = Status::error(E_INVALID_ARGUMENT, "tolerance",
                                      "seam tolerance must be non-negative");
        return report;
    }
    const std::vector<std::uint64_t>& leaves = tree.leaves();
    for (std::size_t i = 0; i < leaves.size(); ++i) {
        const QuadNode* a = tree.node(leaves[i]);
        if (a == nullptr) continue;
        const auto mesh_a_it = meshes.find(leaves[i]);
        if (mesh_a_it == meshes.end()) continue;
        std::unordered_map<std::uint64_t, float> heights_a;
        for (const MeshVertex& vertex : mesh_a_it->second.vertices) {
            if (vertex.grid_x == a->rect.x ||
                vertex.grid_x == a->rect.right() ||
                vertex.grid_y == a->rect.y ||
                vertex.grid_y == a->rect.bottom()) {
                heights_a[vertexKey(vertex.grid_x, vertex.grid_y)] =
                    vertex.position.y;
            }
        }
        for (std::size_t j = i + 1; j < leaves.size(); ++j) {
            const QuadNode* b = tree.node(leaves[j]);
            const auto mesh_b_it = meshes.find(leaves[j]);
            if (b == nullptr || mesh_b_it == meshes.end()) continue;
            bool vertical = true;
            const bool y_overlap =
                std::max(a->rect.y, b->rect.y) <
                std::min(a->rect.bottom(), b->rect.bottom());
            const bool x_overlap =
                std::max(a->rect.x, b->rect.x) <
                std::min(a->rect.right(), b->rect.right());
            vertical = y_overlap && (a->rect.right() == b->rect.x ||
                                     b->rect.right() == a->rect.x);
            const bool horizontal = x_overlap && (a->rect.bottom() == b->rect.y ||
                                                  b->rect.bottom() == a->rect.y);
            if (!vertical && !horizontal) continue;
            SeamCheck check;
            check.block_a = a->id;
            check.block_b = b->id;
            check.vertical = vertical;
            if (vertical) {
                check.overlap_begin = std::max(a->rect.y, b->rect.y);
                check.overlap_end = std::min(a->rect.bottom(), b->rect.bottom());
                const std::uint32_t x =
                    a->rect.right() == b->rect.x ? a->rect.right()
                                                 : b->rect.right();
                for (std::uint32_t y = check.overlap_begin;
                     y <= check.overlap_end; ++y) {
                    const std::uint64_t key = vertexKey(x, y);
                    const auto ha = heights_a.find(key);
                    if (ha == heights_a.end()) {
                        check.closed_by_matched_vertices = false;
                        check.max_deviation = std::numeric_limits<float>::max();
                        break;
                    }
                    float hb = 0.0f;
                    bool found = false;
                    for (const MeshVertex& vertex :
                         mesh_b_it->second.vertices) {
                        if (vertex.grid_x == x && vertex.grid_y == y) {
                            hb = vertex.position.y;
                            found = true;
                            break;
                        }
                    }
                    if (!found) {
                        check.closed_by_matched_vertices = false;
                        check.max_deviation = std::numeric_limits<float>::max();
                        break;
                    }
                    check.max_deviation =
                        std::max(check.max_deviation, std::fabs(ha->second - hb));
                    ++check.sample_count;
                }
            } else {
                check.overlap_begin = std::max(a->rect.x, b->rect.x);
                check.overlap_end = std::min(a->rect.right(), b->rect.right());
                const std::uint32_t y =
                    a->rect.bottom() == b->rect.y ? a->rect.bottom()
                                                  : b->rect.bottom();
                for (std::uint32_t x = check.overlap_begin;
                     x <= check.overlap_end; ++x) {
                    const std::uint64_t key = vertexKey(x, y);
                    const auto ha = heights_a.find(key);
                    if (ha == heights_a.end()) {
                        check.closed_by_matched_vertices = false;
                        check.max_deviation = std::numeric_limits<float>::max();
                        break;
                    }
                    float hb = 0.0f;
                    bool found = false;
                    for (const MeshVertex& vertex :
                         mesh_b_it->second.vertices) {
                        if (vertex.grid_x == x && vertex.grid_y == y) {
                            hb = vertex.position.y;
                            found = true;
                            break;
                        }
                    }
                    if (!found) {
                        check.closed_by_matched_vertices = false;
                        check.max_deviation = std::numeric_limits<float>::max();
                        break;
                    }
                    check.max_deviation =
                        std::max(check.max_deviation, std::fabs(ha->second - hb));
                    ++check.sample_count;
                }
            }
            report.max_deviation =
                std::max(report.max_deviation, check.max_deviation);
            if (!check.closed_by_matched_vertices ||
                check.max_deviation > tolerance) {
                if (report.status.ok()) {
                    report.status = Status::crack(
                        "mesh seam is not closed by matched boundary vertices",
                        check.block_a, check.block_b);
                }
            }
            report.seams.push_back(check);
        }
    }
    return report;
}

}  // namespace terrain

