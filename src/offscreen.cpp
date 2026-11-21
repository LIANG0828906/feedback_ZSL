#include "terrain/offscreen.h"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <limits>
#include <sstream>

#include "terrain/hash.h"

namespace terrain {
namespace {

struct RasterVertex {
    float x = 0.0f;
    float y = 0.0f;
    float z = 0.0f;
    float world_x = 0.0f;
    float world_z = 0.0f;
    Vec3 normal;
    Vec3 color;
};

float edge(const RasterVertex& a, const RasterVertex& b, float px, float py) {
    return (px - a.x) * (b.y - a.y) -
           (py - a.y) * (b.x - a.x);
}

float clamp01(float value) {
    return std::max(0.0f, std::min(1.0f, value));
}

std::uint8_t toByte(float value) {
    return static_cast<std::uint8_t>(
        std::lround(clamp01(value) * 255.0f));
}

std::uint16_t toHeat(float value) {
    return static_cast<std::uint16_t>(
        std::lround(clamp01(value) * 65535.0f));
}

std::string colorHash(const std::vector<std::uint8_t>& color) {
    return toHex(sha256(color));
}

std::string heatHash(const std::vector<std::uint16_t>& heat) {
    std::vector<std::uint8_t> bytes;
    bytes.reserve(heat.size() * 2u);
    for (std::uint16_t value : heat) {
        bytes.push_back(static_cast<std::uint8_t>(value & 0xffu));
        bytes.push_back(static_cast<std::uint8_t>((value >> 8u) & 0xffu));
    }
    return toHex(sha256(bytes));
}

}  // namespace

std::string RenderSummary::stableString() const {
    std::ostringstream stream;
    stream.setf(std::ios::fixed);
    stream.precision(6);
    stream << "patch=" << patch_sequence << " invalid_blocks=" << invalid_blocks
           << " rerendered_blocks=" << rerendered_blocks
           << " reused_blocks=" << reused_blocks
           << " error_upper_bound=" << error_upper_bound
           << " size=" << width << "x" << height
           << " color_hash=" << color_hash << " heat_hash=" << heat_hash;
    return stream.str();
}

SoftwareOffscreenRenderer::SoftwareOffscreenRenderer(Camera camera)
    : camera_(camera) {
    const float length =
        std::sqrt(camera_.light_direction.x * camera_.light_direction.x +
                  camera_.light_direction.y * camera_.light_direction.y +
                  camera_.light_direction.z * camera_.light_direction.z);
    if (length > 0.0f) {
        camera_.light_direction.x /= length;
        camera_.light_direction.y /= length;
        camera_.light_direction.z /= length;
    } else {
        camera_.light_direction = {0.0f, 1.0f, 0.0f};
    }
    camera_.pixels_per_cell =
        std::max(1u, std::min(16u, camera_.pixels_per_cell));
}

float SoftwareOffscreenRenderer::sampleHeightBilinear(
    const HeightField& height, float x, float y) const {
    const float cx =
        std::max(0.0f, std::min(static_cast<float>(height.width - 1u), x));
    const float cy =
        std::max(0.0f, std::min(static_cast<float>(height.height - 1u), y));
    const std::uint32_t x0 = static_cast<std::uint32_t>(std::floor(cx));
    const std::uint32_t y0 = static_cast<std::uint32_t>(std::floor(cy));
    const std::uint32_t x1 = std::min(x0 + 1u, height.width - 1u);
    const std::uint32_t y1 = std::min(y0 + 1u, height.height - 1u);
    const float tx = cx - static_cast<float>(x0);
    const float ty = cy - static_cast<float>(y0);
    const float a = height.at(x0, y0) * (1.0f - tx) +
                    height.at(x1, y0) * tx;
    const float b = height.at(x0, y1) * (1.0f - tx) +
                    height.at(x1, y1) * tx;
    return a * (1.0f - ty) + b * ty;
}

SoftwareOffscreenRenderer::Tile SoftwareOffscreenRenderer::rasterizeTile(
    const BlockMesh& mesh, const HeightField& height) const {
    Tile tile;
    tile.rect = mesh.rect;
    tile.scale = camera_.pixels_per_cell;
    const std::uint32_t tile_width = mesh.rect.width * tile.scale;
    const std::uint32_t tile_height = mesh.rect.height * tile.scale;
    tile.color.assign(static_cast<std::size_t>(tile_width) * tile_height * 4u,
                      0u);
    tile.heat.assign(static_cast<std::size_t>(tile_width) * tile_height, 0u);
    std::vector<float> depth(static_cast<std::size_t>(tile_width) * tile_height,
                             -std::numeric_limits<float>::infinity());
    std::vector<RasterVertex> vertices(mesh.vertices.size());
    for (std::size_t i = 0; i < mesh.vertices.size(); ++i) {
        const MeshVertex& source = mesh.vertices[i];
        RasterVertex& target = vertices[i];
        target.world_x = source.position.x;
        target.world_z = source.position.z;
        target.x = (source.position.x /
                        std::max(1e-8f, height.cell_spacing) -
                    static_cast<float>(mesh.rect.x)) *
                   static_cast<float>(tile.scale);
        target.y = (source.position.z /
                        std::max(1e-8f, height.cell_spacing) -
                    static_cast<float>(mesh.rect.y)) *
                   static_cast<float>(tile.scale);
        target.z = source.position.y;
        target.normal = source.normal;
        target.color = source.material_color;
    }
    const float valid_range =
        std::max(0.01f, height.valid_max - height.valid_min);
    const float heat_scale = std::max(0.25f, valid_range * 0.08f);
    for (std::size_t i = 0; i + 2u < mesh.indices.size(); i += 3u) {
        const RasterVertex& a = vertices[mesh.indices[i]];
        const RasterVertex& b = vertices[mesh.indices[i + 1u]];
        const RasterVertex& c = vertices[mesh.indices[i + 2u]];
        const float area = edge(a, b, c.x, c.y);
        if (std::fabs(area) < 1e-12f) continue;
        const float min_x = std::min({a.x, b.x, c.x});
        const float max_x = std::max({a.x, b.x, c.x});
        const float min_y = std::min({a.y, b.y, c.y});
        const float max_y = std::max({a.y, b.y, c.y});
        const int px_begin =
            std::max(0, static_cast<int>(std::floor(min_x)));
        const int px_end =
            std::min(static_cast<int>(tile_width) - 1,
                     static_cast<int>(std::ceil(max_x)));
        const int py_begin =
            std::max(0, static_cast<int>(std::floor(min_y)));
        const int py_end =
            std::min(static_cast<int>(tile_height) - 1,
                     static_cast<int>(std::ceil(max_y)));
        for (int py = py_begin; py <= py_end; ++py) {
            for (int px = px_begin; px <= px_end; ++px) {
                const float sample_x = static_cast<float>(px) + 0.5f;
                const float sample_y = static_cast<float>(py) + 0.5f;
                float w0 = edge(b, c, sample_x, sample_y) / area;
                float w1 = edge(c, a, sample_x, sample_y) / area;
                float w2 = edge(a, b, sample_x, sample_y) / area;
                constexpr float kEpsilon = -1e-6f;
                if (w0 < kEpsilon || w1 < kEpsilon || w2 < kEpsilon) {
                    continue;
                }
                const float sum = w0 + w1 + w2;
                if (std::fabs(sum) < 1e-12f) continue;
                w0 /= sum;
                w1 /= sum;
                w2 /= sum;
                const float z = a.z * w0 + b.z * w1 + c.z * w2;
                const std::size_t idx =
                    static_cast<std::size_t>(py) * tile_width +
                    static_cast<std::size_t>(px);
                if (z < depth[idx]) continue;
                const float world_x = a.world_x * w0 + b.world_x * w1 +
                                      c.world_x * w2;
                const float world_z = a.world_z * w0 + b.world_z * w1 +
                                      c.world_z * w2;
                const float full_height = sampleHeightBilinear(
                    height, world_x / height.cell_spacing,
                    world_z / height.cell_spacing);
                const float lod_error = std::fabs(z - full_height);
                tile.max_error = std::max(tile.max_error, lod_error);
                depth[idx] = z;
                Vec3 normal;
                normal.x = a.normal.x * w0 + b.normal.x * w1 +
                           c.normal.x * w2;
                normal.y = a.normal.y * w0 + b.normal.y * w1 +
                           c.normal.y * w2;
                normal.z = a.normal.z * w0 + b.normal.z * w1 +
                           c.normal.z * w2;
                normal = normalize(normal);
                const Vec3 color{
                    a.color.x * w0 + b.color.x * w1 + c.color.x * w2,
                    a.color.y * w0 + b.color.y * w1 + c.color.y * w2,
                    a.color.z * w0 + b.color.z * w1 + c.color.z * w2};
                const float light =
                    std::max(0.0f, dot(normal, camera_.light_direction));
                const float shade = 0.34f + 0.66f * light;
                const float height_tint =
                    clamp01((z - height.valid_min) / valid_range);
                const float heat = clamp01(lod_error / heat_scale);
                const float r =
                    color.x * shade * (0.92f + 0.08f * height_tint) +
                    heat * 0.55f;
                const float g =
                    color.y * shade * (0.90f + 0.10f * height_tint) +
                    heat * 0.18f;
                const float bl =
                    color.z * shade * (0.88f + 0.12f * height_tint) +
                    heat * 0.08f;
                tile.color[idx * 4u] = toByte(r);
                tile.color[idx * 4u + 1u] = toByte(g);
                tile.color[idx * 4u + 2u] = toByte(bl);
                tile.color[idx * 4u + 3u] = 255u;
                tile.heat[idx] = toHeat(heat);
            }
        }
    }
    for (std::size_t i = 0; i < tile.heat.size(); ++i) {
        if (tile.color[i * 4u + 3u] == 0u) {
            tile.color[i * 4u] = 16u;
            tile.color[i * 4u + 1u] = 20u;
            tile.color[i * 4u + 2u] = 26u;
            tile.color[i * 4u + 3u] = 255u;
        }
    }
    return tile;
}

void SoftwareOffscreenRenderer::composite(const Quadtree& tree) {
    const std::uint32_t cell_width =
        tree.node(tree.rootId()) ? tree.node(tree.rootId())->rect.width : 0;
    const std::uint32_t cell_height =
        tree.node(tree.rootId()) ? tree.node(tree.rootId())->rect.height : 0;
    frame_.width = cell_width * camera_.pixels_per_cell;
    frame_.height = cell_height * camera_.pixels_per_cell;
    frame_.color.assign(
        static_cast<std::size_t>(frame_.width) * frame_.height * 4u, 0u);
    frame_.heat.assign(
        static_cast<std::size_t>(frame_.width) * frame_.height, 0u);
    for (std::uint64_t id : tree.leaves()) {
        const auto tile_it = tiles_.find(id);
        if (tile_it == tiles_.end()) continue;
        const Tile& tile = tile_it->second;
        const std::uint32_t x_begin = tile.rect.x * tile.scale;
        const std::uint32_t y_begin = tile.rect.y * tile.scale;
        const std::uint32_t tile_width = tile.rect.width * tile.scale;
        const std::uint32_t tile_height = tile.rect.height * tile.scale;
        for (std::uint32_t y = 0; y < tile_height; ++y) {
            const std::uint32_t dst_y = y_begin + y;
            if (dst_y >= frame_.height) break;
            for (std::uint32_t x = 0; x < tile_width; ++x) {
                const std::uint32_t dst_x = x_begin + x;
                if (dst_x >= frame_.width) break;
                const std::size_t src =
                    static_cast<std::size_t>(y) * tile_width + x;
                const std::size_t dst =
                    static_cast<std::size_t>(dst_y) * frame_.width + dst_x;
                frame_.color[dst * 4u] = tile.color[src * 4u];
                frame_.color[dst * 4u + 1u] = tile.color[src * 4u + 1u];
                frame_.color[dst * 4u + 2u] = tile.color[src * 4u + 2u];
                frame_.color[dst * 4u + 3u] = tile.color[src * 4u + 3u];
                frame_.heat[dst] = tile.heat[src];
            }
        }
    }
}

RenderSummary SoftwareOffscreenRenderer::renderInternal(
    const Quadtree& tree, const HeightField& height,
    const SurfaceCache& surface,
    const std::map<std::uint64_t, BlockMesh>& meshes,
    const std::vector<std::uint64_t>& dirty_blocks,
    std::uint32_t invalid_block_count, std::uint64_t patch_sequence,
    bool full) {
    (void)surface;
    RenderSummary summary;
    summary.patch_sequence = patch_sequence;
    summary.invalid_blocks = invalid_block_count;
    summary.full_frame = full;
    std::map<std::uint64_t, bool> dirty;
    for (std::uint64_t id : dirty_blocks) dirty[id] = true;
    const std::vector<std::uint64_t>& leaves = tree.leaves();
    std::map<std::uint64_t, bool> live;
    float error_upper = 0.0f;
    for (std::uint64_t id : leaves) {
        live[id] = true;
        const auto mesh_it = meshes.find(id);
        if (mesh_it == meshes.end()) continue;
        bool needs_render =
            full || dirty.find(id) != dirty.end() ||
            tiles_.find(id) == tiles_.end();
        if (!needs_render) {
            const Tile& existing = tiles_.at(id);
            if (existing.rect.x != mesh_it->second.rect.x ||
                existing.rect.y != mesh_it->second.rect.y ||
                existing.rect.width != mesh_it->second.rect.width ||
                existing.rect.height != mesh_it->second.rect.height) {
                needs_render = true;
            }
        }
        if (needs_render) {
            Tile tile = rasterizeTile(mesh_it->second, height);
            error_upper = std::max(error_upper, tile.max_error);
            tiles_[id] = std::move(tile);
            ++summary.rerendered_blocks;
        } else {
            error_upper = std::max(error_upper, tiles_.at(id).max_error);
            ++summary.reused_blocks;
        }
    }
    for (auto it = tiles_.begin(); it != tiles_.end();) {
        if (live.find(it->first) == live.end()) {
            it = tiles_.erase(it);
        } else {
            ++it;
        }
    }
    composite(tree);
    summary.width = frame_.width;
    summary.height = frame_.height;
    summary.error_upper_bound = error_upper;
    summary.color_hash = colorHash(frame_.color);
    summary.heat_hash = heatHash(frame_.heat);
    last_summary_ = summary;
    return summary;
}

RenderSummary SoftwareOffscreenRenderer::renderFull(
    const Quadtree& tree, const HeightField& height,
    const SurfaceCache& surface,
    const std::map<std::uint64_t, BlockMesh>& meshes,
    std::uint64_t patch_sequence) {
    return renderInternal(tree, height, surface, meshes, {}, 0, patch_sequence,
                          true);
}

RenderSummary SoftwareOffscreenRenderer::renderIncremental(
    const Quadtree& tree, const HeightField& height,
    const SurfaceCache& surface,
    const std::map<std::uint64_t, BlockMesh>& meshes,
    const std::vector<std::uint64_t>& dirty_blocks,
    std::uint32_t invalid_block_count, std::uint64_t patch_sequence) {
    return renderInternal(tree, height, surface, meshes, dirty_blocks,
                          invalid_block_count, patch_sequence, false);
}

}  // namespace terrain

