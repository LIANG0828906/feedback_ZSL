#include "internal.hpp"

#include <algorithm>
#include <array>
#include <cmath>
#include <cstring>
#include <limits>
#include <map>
#include <numeric>
#include <set>
#include <sstream>
#include <unordered_map>

namespace meshrepair::detail {

namespace {

constexpr char kPlanMagic[8] = {'M', 'R', 'P', 'L', 'A', 'N', '0', '1'};
constexpr char kChunkMagic[8] = {'M', 'R', 'C', 'H', 'U', 'N', 'K', '1'};
constexpr uint32_t kPlanFormatVersion = 1;
constexpr uint32_t kTableVersion = 1;
constexpr uint32_t kBoundaryVersion = 1;
constexpr uint32_t kChunkFormatVersion = 1;

uint64_t cellIndex(double coordinate, double size) {
    const double raw = std::floor(coordinate / size);
    if (raw <= static_cast<double>(std::numeric_limits<int64_t>::min())) {
        return static_cast<uint64_t>(std::numeric_limits<int64_t>::min());
    }
    if (raw >= static_cast<double>(std::numeric_limits<int64_t>::max())) {
        return static_cast<uint64_t>(std::numeric_limits<int64_t>::max());
    }
    return static_cast<uint64_t>(static_cast<int64_t>(raw));
}

ChunkCoord cellFor(const Vec3& p, const Config& config) {
    ChunkCoord c;
    c.x = static_cast<int64_t>(cellIndex(p.x, config.chunk_size_x));
    c.y = static_cast<int64_t>(cellIndex(p.y, config.chunk_size_y));
    c.z = static_cast<int64_t>(cellIndex(p.z, config.chunk_size_z));
    return c;
}

std::string tablePayload(const Manifest& manifest) {
    Writer writer;
    writer.u32(kTableVersion);
    writer.string(manifest.input_hash);
    writer.f64(manifest.chunk_size_x);
    writer.f64(manifest.chunk_size_y);
    writer.f64(manifest.chunk_size_z);
    writer.u32(static_cast<uint32_t>(manifest.chunks.size()));
    for (const ChunkDef& chunk : manifest.chunks) {
        writer.u32(chunk.id);
        writer.i64(chunk.coord.x);
        writer.i64(chunk.coord.y);
        writer.i64(chunk.coord.z);
        writer.u64(chunk.estimated_bytes);
        writer.u64(chunk.faces.size());
        for (uint64_t face : chunk.faces) writer.u64(face);
        writer.u64(chunk.vertices.size());
        for (uint64_t vertex : chunk.vertices) writer.u64(vertex);
    }
    writer.u32(static_cast<uint32_t>(manifest.straddles.size()));
    for (const StraddleDef& straddle : manifest.straddles) {
        writer.u64(straddle.face);
        writer.u32(straddle.owner_chunk);
        writer.u32(static_cast<uint32_t>(straddle.touched.size()));
        for (const ChunkCoord& cell : straddle.touched) {
            writer.i64(cell.x);
            writer.i64(cell.y);
            writer.i64(cell.z);
        }
    }
    const std::vector<uint8_t>& bytes = writer.bytes();
    return std::string(reinterpret_cast<const char*>(bytes.data()), bytes.size());
}

std::string boundaryPayload(const Manifest& manifest) {
    Writer writer;
    writer.u32(kBoundaryVersion);
    writer.string(manifest.table_hash);
    writer.u32(static_cast<uint32_t>(manifest.boundaries.size()));
    for (const BoundaryDef& boundary : manifest.boundaries) {
        writer.u32(boundary.chunk_a);
        writer.u32(boundary.chunk_b);
        writer.u64(boundary.vertices.size());
        for (uint64_t vertex : boundary.vertices) writer.u64(vertex);
        writer.u64(boundary.edges.size());
        for (const EdgeKey& edge : boundary.edges) {
            writer.u64(edge.a);
            writer.u64(edge.b);
        }
    }
    const std::vector<uint8_t>& bytes = writer.bytes();
    return std::string(reinterpret_cast<const char*>(bytes.data()), bytes.size());
}

Error planFailure(const std::string& message) {
    return makeError(ErrorCode::RecoveryInvalid, message);
}

struct Dsu {
    explicit Dsu(size_t size) : parent(size), rank(size, 0) {
        std::iota(parent.begin(), parent.end(), 0);
    }
    size_t find(size_t value) {
        while (parent[value] != value) {
            parent[value] = parent[parent[value]];
            value = parent[value];
        }
        return value;
    }
    void unite(size_t a, size_t b) {
        a = find(a);
        b = find(b);
        if (a == b) return;
        if (a > b) std::swap(a, b);
        parent[b] = a;
    }
    std::vector<size_t> parent;
    std::vector<uint8_t> rank;
};

struct Cell3 {
    int64_t x = 0;
    int64_t y = 0;
    int64_t z = 0;
    bool operator<(const Cell3& other) const {
        if (x != other.x) return x < other.x;
        if (y != other.y) return y < other.y;
        return z < other.z;
    }
};

Cell3 toleranceCell(const Vec3& p, double tolerance) {
    const double inv = 1.0 / tolerance;
    Cell3 c;
    c.x = static_cast<int64_t>(std::floor(p.x * inv));
    c.y = static_cast<int64_t>(std::floor(p.y * inv));
    c.z = static_cast<int64_t>(std::floor(p.z * inv));
    return c;
}

double distanceSquared(const Vec3& a, const Vec3& b) {
    const double dx = a.x - b.x;
    const double dy = a.y - b.y;
    const double dz = a.z - b.z;
    return dx * dx + dy * dy + dz * dz;
}

bool isMappedBoundaryVertex(const Manifest& manifest, uint64_t vertex) {
    for (const BoundaryDef& boundary : manifest.boundaries) {
        if (std::binary_search(
                boundary.vertices.begin(), boundary.vertices.end(), vertex)) {
            return true;
        }
    }
    return false;
}

void weldCandidateSet(
    const std::vector<uint64_t>& candidates,
    const std::vector<Vec3>& positions,
    double tolerance,
    Dsu& dsu,
    const std::vector<std::vector<uint32_t>>& vertex_chunks,
    const Manifest& manifest) {
    if (candidates.size() < 2) return;
    std::vector<uint64_t> sorted = candidates;
    std::sort(sorted.begin(), sorted.end());
    sorted.erase(std::unique(sorted.begin(), sorted.end()), sorted.end());
    const double tolerance_sq = tolerance * tolerance;

    if (tolerance == 0.0) {
        std::map<std::array<uint64_t, 3>, uint64_t> exact;
        for (uint64_t id : sorted) {
            const Vec3& p = positions[static_cast<size_t>(id)];
            uint64_t bits[3];
            std::memcpy(&bits[0], &p.x, 8);
            std::memcpy(&bits[1], &p.y, 8);
            std::memcpy(&bits[2], &p.z, 8);
            auto key = std::array<uint64_t, 3>{bits[0], bits[1], bits[2]};
            auto found = exact.find(key);
            if (found == exact.end()) {
                exact.emplace(key, id);
            } else {
                dsu.unite(static_cast<size_t>(found->second), static_cast<size_t>(id));
            }
        }
        return;
    }

    std::map<Cell3, std::vector<uint64_t>> grid;
    for (uint64_t id : sorted) {
        const Vec3& p = positions[static_cast<size_t>(id)];
        const Cell3 cell = toleranceCell(p, tolerance);
        for (int64_t dx = -1; dx <= 1; ++dx) {
            for (int64_t dy = -1; dy <= 1; ++dy) {
                for (int64_t dz = -1; dz <= 1; ++dz) {
                    const Cell3 neighbor{cell.x + dx, cell.y + dy, cell.z + dz};
                    auto found = grid.find(neighbor);
                    if (found == grid.end()) continue;
                    for (uint64_t other : found->second) {
                        if (distanceSquared(p, positions[static_cast<size_t>(other)]) >
                            tolerance_sq) {
                            continue;
                        }
                        bool allowed = true;
                        const auto& chunks_a = vertex_chunks[static_cast<size_t>(id)];
                        const auto& chunks_b = vertex_chunks[static_cast<size_t>(other)];
                        if (chunks_a.empty() || chunks_b.empty()) {
                            allowed = false;
                        } else {
                            bool same_chunk = false;
                            for (uint32_t a : chunks_a) {
                                if (std::binary_search(chunks_b.begin(), chunks_b.end(), a)) {
                                    same_chunk = true;
                                    break;
                                }
                            }
                            if (!same_chunk) {
                                // Every boundary pair in the manifest has been
                                // committed before repair starts. A weld between
                                // two mapped boundary vertices is therefore
                                // authorized by the committed protocol.
                                allowed =
                                    isMappedBoundaryVertex(manifest, id) &&
                                    isMappedBoundaryVertex(manifest, other);
                            }
                        }
                        if (allowed) {
                            dsu.unite(static_cast<size_t>(id), static_cast<size_t>(other));
                        }
                    }
                }
            }
        }
        grid[cell].push_back(id);
    }
}

struct BoundaryWalkEdge {
    EdgeKey key;
    uint64_t face = 0;
    uint64_t from = 0;
    uint64_t to = 0;
};

std::vector<std::vector<uint64_t>> traceBoundaryLoops(
    const std::vector<ChunkFaceRecord>& faces,
    const std::map<EdgeKey, std::vector<size_t>>& incidences,
    std::vector<BoundaryWalkEdge>& boundary_edges) {
    boundary_edges.clear();
    for (const auto& entry : incidences) {
        if (entry.second.size() != 1) continue;
        const size_t face_index = entry.second.front();
        const ChunkFaceRecord& face = faces[face_index];
        uint64_t from = 0;
        uint64_t to = 0;
        for (int i = 0; i < 3; ++i) {
            if (EdgeKey::make(face.v[static_cast<size_t>(i)],
                              face.v[static_cast<size_t>((i + 1) % 3)]) == entry.first) {
                from = face.v[static_cast<size_t>(i)];
                to = face.v[static_cast<size_t>((i + 1) % 3)];
                break;
            }
        }
        boundary_edges.push_back(BoundaryWalkEdge{entry.first, face_index, from, to});
    }
    std::sort(
        boundary_edges.begin(), boundary_edges.end(),
        [](const BoundaryWalkEdge& a, const BoundaryWalkEdge& b) {
            if (a.key == b.key) return a.face < b.face;
            return a.key < b.key;
        });

    std::map<uint64_t, std::vector<uint32_t>> incident;
    for (uint32_t i = 0; i < boundary_edges.size(); ++i) {
        incident[boundary_edges[i].key.a].push_back(i);
        incident[boundary_edges[i].key.b].push_back(i);
    }
    for (auto& entry : incident) {
        std::sort(entry.second.begin(), entry.second.end());
    }

    std::vector<uint8_t> visited(boundary_edges.size(), 0);
    std::vector<std::vector<uint64_t>> loops;
    for (uint32_t start_edge = 0; start_edge < boundary_edges.size(); ++start_edge) {
        if (visited[start_edge]) continue;
        const uint64_t start_vertex = boundary_edges[start_edge].key.a;
        std::vector<uint64_t> loop;
        uint64_t current_vertex = start_vertex;
        uint32_t current_edge = start_edge;
        loop.push_back(current_vertex);
        bool closed = false;
        for (size_t guard = 0; guard <= boundary_edges.size(); ++guard) {
            visited[current_edge] = 1;
            const BoundaryWalkEdge& edge = boundary_edges[current_edge];
            const uint64_t next_vertex =
                edge.key.a == current_vertex ? edge.key.b : edge.key.a;
            if (next_vertex == start_vertex) {
                closed = true;
                break;
            }
            loop.push_back(next_vertex);
            const auto& options = incident[next_vertex];
            uint32_t next_edge = UINT32_MAX;
            for (uint32_t option : options) {
                if (!visited[option]) {
                    next_edge = option;
                    break;
                }
            }
            if (next_edge == UINT32_MAX) break;
            current_vertex = next_vertex;
            current_edge = next_edge;
        }
        if (closed && loop.size() >= 3) {
            loops.push_back(std::move(loop));
        }
    }
    std::sort(loops.begin(), loops.end());
    return loops;
}

struct Point2 {
    double x = 0.0;
    double y = 0.0;
};

double cross2(const Point2& a, const Point2& b, const Point2& c) {
    return (b.x - a.x) * (c.y - a.y) - (b.y - a.y) * (c.x - a.x);
}

bool pointInTriangle(const Point2& p, const Point2& a, const Point2& b, const Point2& c) {
    const double c1 = cross2(a, b, p);
    const double c2 = cross2(b, c, p);
    const double c3 = cross2(c, a, p);
    const double epsilon = 1e-14;
    const bool has_neg = c1 < -epsilon || c2 < -epsilon || c3 < -epsilon;
    const bool has_pos = c1 > epsilon || c2 > epsilon || c3 > epsilon;
    return !(has_neg && has_pos);
}

std::vector<std::array<uint64_t, 3>> triangulateLoop(
    const std::vector<uint64_t>& loop,
    const std::map<uint64_t, Vec3>& positions) {
    std::vector<std::array<uint64_t, 3>> result;
    if (loop.size() < 3) return result;

    Vec3 normal{};
    for (size_t i = 0; i < loop.size(); ++i) {
        const Vec3& current = positions.at(loop[i]);
        const Vec3& next = positions.at(loop[(i + 1) % loop.size()]);
        normal.x += (current.y - next.y) * (current.z + next.z);
        normal.y += (current.z - next.z) * (current.x + next.x);
        normal.z += (current.x - next.x) * (current.y + next.y);
    }
    int drop = 2;
    if (std::abs(normal.x) >= std::abs(normal.y) &&
        std::abs(normal.x) >= std::abs(normal.z)) {
        drop = 0;
    } else if (std::abs(normal.y) >= std::abs(normal.z)) {
        drop = 1;
    }

    std::vector<Point2> points;
    points.reserve(loop.size());
    for (uint64_t id : loop) {
        const Vec3& p = positions.at(id);
        if (drop == 0) {
            points.push_back(Point2{p.y, p.z});
        } else if (drop == 1) {
            points.push_back(Point2{p.x, p.z});
        } else {
            points.push_back(Point2{p.x, p.y});
        }
    }
    double area = 0.0;
    for (size_t i = 0; i < points.size(); ++i) {
        const Point2& a = points[i];
        const Point2& b = points[(i + 1) % points.size()];
        area += a.x * b.y - b.x * a.y;
    }
    std::vector<size_t> indices(loop.size());
    std::iota(indices.begin(), indices.end(), 0);
    if (area < 0.0) std::reverse(indices.begin(), indices.end());

    bool ear_clipping_ok = true;
    while (indices.size() > 3) {
        bool found_ear = false;
        for (size_t i = 0; i < indices.size(); ++i) {
            const size_t prev = indices[(i + indices.size() - 1) % indices.size()];
            const size_t current = indices[i];
            const size_t next = indices[(i + 1) % indices.size()];
            if (cross2(points[prev], points[current], points[next]) <= 1e-14) continue;
            bool contains = false;
            for (size_t other : indices) {
                if (other == prev || other == current || other == next) continue;
                if (pointInTriangle(points[other], points[prev], points[current], points[next])) {
                    contains = true;
                    break;
                }
            }
            if (contains) continue;
            result.push_back(
                std::array<uint64_t, 3>{loop[prev], loop[current], loop[next]});
            indices.erase(indices.begin() + static_cast<std::ptrdiff_t>(i));
            found_ear = true;
            break;
        }
        if (!found_ear) {
            ear_clipping_ok = false;
            break;
        }
    }
    if (ear_clipping_ok && indices.size() == 3) {
        result.push_back(std::array<uint64_t, 3>{
            loop[indices[0]], loop[indices[1]], loop[indices[2]]});
        return result;
    }

    result.clear();
    for (size_t i = 1; i + 1 < loop.size(); ++i) {
        result.push_back(std::array<uint64_t, 3>{loop[0], loop[i], loop[i + 1]});
    }
    return result;
}

std::array<uint64_t, 3> sortedTriangle(const std::array<uint64_t, 3>& triangle) {
    std::array<uint64_t, 3> sorted = triangle;
    std::sort(sorted.begin(), sorted.end());
    return sorted;
}

std::string hexBytes(const uint8_t* data, size_t size) {
    static constexpr char digits[] = "0123456789abcdef";
    std::string result(size * 2, '0');
    for (size_t i = 0; i < size; ++i) {
        result[i * 2] = digits[data[i] >> 4];
        result[i * 2 + 1] = digits[data[i] & 0x0fu];
    }
    return result;
}

}  // namespace

Result<Manifest> buildManifest(
    const Mesh& mesh,
    const Config& config,
    const Topology& topology,
    const std::string& input_hash,
    const std::string& config_hash) {
    Manifest manifest;
    manifest.input_hash = input_hash;
    manifest.config_hash = config_hash;
    manifest.chunk_size_x = config.chunk_size_x;
    manifest.chunk_size_y = config.chunk_size_y;
    manifest.chunk_size_z = config.chunk_size_z;

    std::vector<ChunkCoord> face_owner(mesh.faces.size());
    std::vector<std::vector<ChunkCoord>> face_touched(mesh.faces.size());
    std::set<ChunkCoord> owner_coords;
    for (size_t face_id = 0; face_id < mesh.faces.size(); ++face_id) {
        ChunkCoord minimum;
        ChunkCoord maximum;
        bool first = true;
        for (uint64_t vertex : mesh.faces[face_id].v) {
            const ChunkCoord cell = cellFor(mesh.vertices[static_cast<size_t>(vertex)], config);
            if (first) {
                minimum = maximum = cell;
                first = false;
            } else {
                minimum.x = std::min(minimum.x, cell.x);
                minimum.y = std::min(minimum.y, cell.y);
                minimum.z = std::min(minimum.z, cell.z);
                maximum.x = std::max(maximum.x, cell.x);
                maximum.y = std::max(maximum.y, cell.y);
                maximum.z = std::max(maximum.z, cell.z);
            }
        }
        std::vector<ChunkCoord> touched;
        for (int64_t x = minimum.x; x <= maximum.x; ++x) {
            for (int64_t y = minimum.y; y <= maximum.y; ++y) {
                for (int64_t z = minimum.z; z <= maximum.z; ++z) {
                    touched.push_back(ChunkCoord{x, y, z});
                    if (touched.size() > 1000000) {
                        return Result<Manifest>::failure(makeError(
                            ErrorCode::MeshInvalid,
                            "a face touches more than one million chunks",
                            {},
                            face_id));
                    }
                }
            }
        }
        std::sort(touched.begin(), touched.end());
        face_owner[face_id] = touched.front();
        face_touched[face_id] = std::move(touched);
        owner_coords.insert(face_owner[face_id]);
    }

    std::map<ChunkCoord, uint32_t> chunk_id;
    uint32_t next_id = 0;
    for (const ChunkCoord& coord : owner_coords) {
        chunk_id.emplace(coord, next_id++);
    }
    manifest.chunks.resize(chunk_id.size());
    for (const auto& entry : chunk_id) {
        ChunkDef& chunk = manifest.chunks[entry.second];
        chunk.id = entry.second;
        chunk.coord = entry.first;
    }
    std::vector<uint32_t> face_chunk(mesh.faces.size(), UINT32_MAX);
    for (size_t face_id = 0; face_id < mesh.faces.size(); ++face_id) {
        const uint32_t owner = chunk_id.at(face_owner[face_id]);
        face_chunk[face_id] = owner;
        manifest.chunks[owner].faces.push_back(face_id);
        for (uint64_t vertex : mesh.faces[face_id].v) {
            manifest.chunks[owner].vertices.push_back(vertex);
        }
        if (face_touched[face_id].size() > 1) {
            StraddleDef straddle;
            straddle.face = face_id;
            straddle.owner_chunk = owner;
            straddle.touched = face_touched[face_id];
            manifest.straddles.push_back(std::move(straddle));
        }
    }
    for (ChunkDef& chunk : manifest.chunks) {
        std::sort(chunk.vertices.begin(), chunk.vertices.end());
        chunk.vertices.erase(
            std::unique(chunk.vertices.begin(), chunk.vertices.end()),
            chunk.vertices.end());
        chunk.estimated_bytes =
            4096ull + static_cast<uint64_t>(chunk.vertices.size()) * 128ull +
            static_cast<uint64_t>(chunk.faces.size()) * 192ull;
    }
    std::sort(
        manifest.straddles.begin(),
        manifest.straddles.end(),
        [](const StraddleDef& a, const StraddleDef& b) { return a.face < b.face; });

    std::map<std::pair<uint32_t, uint32_t>, BoundaryDef> boundary_map;
    for (const auto& entry : topology.edge_faces) {
        std::vector<uint32_t> owners;
        owners.reserve(entry.second.size());
        for (uint64_t face : entry.second) {
            owners.push_back(face_chunk[static_cast<size_t>(face)]);
        }
        std::sort(owners.begin(), owners.end());
        owners.erase(std::unique(owners.begin(), owners.end()), owners.end());
        for (size_t i = 0; i < owners.size(); ++i) {
            for (size_t j = i + 1; j < owners.size(); ++j) {
                const auto key = std::make_pair(
                    std::min(owners[i], owners[j]), std::max(owners[i], owners[j]));
                BoundaryDef& boundary = boundary_map[key];
                boundary.chunk_a = key.first;
                boundary.chunk_b = key.second;
                boundary.edges.push_back(entry.first);
                boundary.vertices.push_back(entry.first.a);
                boundary.vertices.push_back(entry.first.b);
            }
        }
    }
    for (auto& entry : boundary_map) {
        BoundaryDef& boundary = entry.second;
        std::sort(boundary.vertices.begin(), boundary.vertices.end());
        boundary.vertices.erase(
            std::unique(boundary.vertices.begin(), boundary.vertices.end()),
            boundary.vertices.end());
        std::sort(boundary.edges.begin(), boundary.edges.end());
        boundary.edges.erase(
            std::unique(boundary.edges.begin(), boundary.edges.end()),
            boundary.edges.end());
        Writer writer;
        writer.u32(boundary.chunk_a);
        writer.u32(boundary.chunk_b);
        writer.u64(boundary.vertices.size());
        for (uint64_t vertex : boundary.vertices) writer.u64(vertex);
        writer.u64(boundary.edges.size());
        for (const EdgeKey& edge : boundary.edges) {
            writer.u64(edge.a);
            writer.u64(edge.b);
        }
        boundary.hash = sha256Hex(writer.bytes());
        manifest.boundaries.push_back(boundary);
    }
    std::sort(
        manifest.boundaries.begin(),
        manifest.boundaries.end(),
        [](const BoundaryDef& a, const BoundaryDef& b) {
            if (a.chunk_a != b.chunk_a) return a.chunk_a < b.chunk_a;
            return a.chunk_b < b.chunk_b;
        });

    const std::string table_blob = tablePayload(manifest);
    manifest.table_hash = sha256Hex(table_blob);
    const std::string boundary_blob = boundaryPayload(manifest);
    manifest.boundary_hash = sha256Hex(boundary_blob);
    const std::vector<uint8_t> plan_bytes = serializeManifest(manifest);
    manifest.plan_hash = sha256Hex(
        plan_bytes.data(), plan_bytes.size() >= 32 ? plan_bytes.size() - 32 : 0);
    return Result<Manifest>::success(std::move(manifest));
}

std::vector<uint8_t> serializeManifest(const Manifest& manifest) {
    const std::string table_blob = tablePayload(manifest);
    const std::string boundary_blob = boundaryPayload(manifest);
    Writer writer;
    writer.raw(kPlanMagic, sizeof(kPlanMagic));
    writer.u32(kPlanFormatVersion);
    writer.string(manifest.input_hash);
    writer.string(manifest.config_hash);
    writer.f64(manifest.chunk_size_x);
    writer.f64(manifest.chunk_size_y);
    writer.f64(manifest.chunk_size_z);
    writer.u32(kTableVersion);
    writer.string(manifest.table_hash);
    writer.u64(table_blob.size());
    writer.raw(table_blob.data(), table_blob.size());
    writer.u32(kBoundaryVersion);
    writer.string(manifest.boundary_hash);
    writer.u64(boundary_blob.size());
    writer.raw(boundary_blob.data(), boundary_blob.size());
    const std::string checksum = sha256Hex(writer.bytes());
    std::array<uint8_t, 32> digest{};
    for (size_t i = 0; i < 32; ++i) {
        const std::string byte = checksum.substr(i * 2, 2);
        digest[i] = static_cast<uint8_t>(std::stoul(byte, nullptr, 16));
    }
    writer.raw(digest.data(), digest.size());
    return writer.bytes();
}

Result<Manifest> parseManifestBytes(const std::vector<uint8_t>& bytes) {
    if (bytes.size() < sizeof(kPlanMagic) + 4 + 32) {
        return Result<Manifest>::failure(planFailure("plan manifest is truncated"));
    }
    const size_t body_size = bytes.size() - 32;
    const std::string expected_checksum = sha256Hex(bytes.data(), body_size);
    std::array<uint8_t, 32> stored{};
    std::memcpy(stored.data(), bytes.data() + body_size, 32);
    const std::string stored_checksum = hexBytes(stored.data(), stored.size());
    if (expected_checksum != stored_checksum) {
        return Result<Manifest>::failure(planFailure("plan manifest checksum mismatch"));
    }
    Reader reader(bytes.data(), body_size);
    char magic[8]{};
    if (!reader.raw(magic, sizeof(magic)) ||
        std::memcmp(magic, kPlanMagic, sizeof(magic)) != 0) {
        return Result<Manifest>::failure(planFailure("bad plan manifest magic"));
    }
    uint32_t version = 0;
    if (!reader.u32(version) || version != kPlanFormatVersion) {
        return Result<Manifest>::failure(planFailure("unsupported plan manifest version"));
    }
    Manifest manifest;
    if (!reader.string(manifest.input_hash) ||
        !reader.string(manifest.config_hash) ||
        !reader.f64(manifest.chunk_size_x) ||
        !reader.f64(manifest.chunk_size_y) ||
        !reader.f64(manifest.chunk_size_z)) {
        return Result<Manifest>::failure(planFailure("invalid plan manifest header"));
    }
    uint32_t table_version = 0;
    std::string table_hash;
    uint64_t table_size = 0;
    if (!reader.u32(table_version) || table_version != kTableVersion ||
        !reader.string(table_hash) || !reader.u64(table_size) ||
        table_size > reader.remaining()) {
        return Result<Manifest>::failure(planFailure("invalid table manifest section"));
    }
    std::vector<uint8_t> table_blob(static_cast<size_t>(table_size));
    if (!reader.raw(table_blob.data(), table_blob.size())) {
        return Result<Manifest>::failure(planFailure("truncated table manifest"));
    }
    uint32_t boundary_version = 0;
    std::string boundary_hash;
    uint64_t boundary_size = 0;
    if (!reader.u32(boundary_version) || boundary_version != kBoundaryVersion ||
        !reader.string(boundary_hash) || !reader.u64(boundary_size) ||
        boundary_size > reader.remaining()) {
        return Result<Manifest>::failure(planFailure("invalid boundary manifest section"));
    }
    std::vector<uint8_t> boundary_blob(static_cast<size_t>(boundary_size));
    if (!reader.raw(boundary_blob.data(), boundary_blob.size()) || !reader.atEnd()) {
        return Result<Manifest>::failure(planFailure("truncated boundary manifest"));
    }
    if (sha256Hex(table_blob) != table_hash ||
        sha256Hex(boundary_blob) != boundary_hash) {
        return Result<Manifest>::failure(planFailure("plan section hash mismatch"));
    }
    manifest.table_hash = table_hash;
    manifest.boundary_hash = boundary_hash;
    manifest.plan_hash = expected_checksum;

    Reader table(table_blob.data(), table_blob.size());
    uint32_t table_version_value = 0;
    uint32_t chunk_count = 0;
    if (!table.u32(table_version_value) || table_version_value != kTableVersion ||
        !table.string(manifest.input_hash) ||
        !table.f64(manifest.chunk_size_x) ||
        !table.f64(manifest.chunk_size_y) ||
        !table.f64(manifest.chunk_size_z) ||
        !table.u32(chunk_count)) {
        return Result<Manifest>::failure(planFailure("invalid table payload"));
    }
    manifest.chunks.resize(chunk_count);
    for (ChunkDef& chunk : manifest.chunks) {
        uint64_t face_count = 0;
        uint64_t vertex_count = 0;
        if (!table.u32(chunk.id) || !table.i64(chunk.coord.x) ||
            !table.i64(chunk.coord.y) || !table.i64(chunk.coord.z) ||
            !table.u64(chunk.estimated_bytes) || !table.u64(face_count) ||
            face_count > table.remaining() / 8) {
            return Result<Manifest>::failure(planFailure("invalid chunk table entry"));
        }
        chunk.faces.resize(static_cast<size_t>(face_count));
        for (uint64_t& face : chunk.faces) {
            if (!table.u64(face)) {
                return Result<Manifest>::failure(planFailure("truncated face table"));
            }
        }
        if (!table.u64(vertex_count) || vertex_count > table.remaining() / 8) {
            return Result<Manifest>::failure(planFailure("invalid vertex table entry"));
        }
        chunk.vertices.resize(static_cast<size_t>(vertex_count));
        for (uint64_t& vertex : chunk.vertices) {
            if (!table.u64(vertex)) {
                return Result<Manifest>::failure(planFailure("truncated vertex table"));
            }
        }
    }
    uint32_t straddle_count = 0;
    if (!table.u32(straddle_count)) {
        return Result<Manifest>::failure(planFailure("missing straddle table"));
    }
    manifest.straddles.resize(straddle_count);
    for (StraddleDef& straddle : manifest.straddles) {
        uint32_t touched_count = 0;
        if (!table.u64(straddle.face) || !table.u32(straddle.owner_chunk) ||
            !table.u32(touched_count)) {
            return Result<Manifest>::failure(planFailure("invalid straddle entry"));
        }
        straddle.touched.resize(touched_count);
        for (ChunkCoord& cell : straddle.touched) {
            if (!table.i64(cell.x) || !table.i64(cell.y) || !table.i64(cell.z)) {
                return Result<Manifest>::failure(planFailure("truncated straddle cells"));
            }
        }
    }
    if (!table.atEnd()) {
        return Result<Manifest>::failure(planFailure("trailing bytes in table payload"));
    }

    Reader boundary(boundary_blob.data(), boundary_blob.size());
    uint32_t boundary_version_value = 0;
    uint32_t pair_count = 0;
    std::string boundary_table_hash;
    if (!boundary.u32(boundary_version_value) ||
        boundary_version_value != kBoundaryVersion ||
        !boundary.string(boundary_table_hash) ||
        boundary_table_hash != manifest.table_hash ||
        !boundary.u32(pair_count)) {
        return Result<Manifest>::failure(planFailure("invalid boundary payload"));
    }
    manifest.boundaries.resize(pair_count);
    for (BoundaryDef& pair : manifest.boundaries) {
        uint64_t vertex_count = 0;
        uint64_t edge_count = 0;
        if (!boundary.u32(pair.chunk_a) || !boundary.u32(pair.chunk_b) ||
            !boundary.u64(vertex_count) || vertex_count > boundary.remaining() / 8) {
            return Result<Manifest>::failure(planFailure("invalid boundary pair"));
        }
        pair.vertices.resize(static_cast<size_t>(vertex_count));
        for (uint64_t& vertex : pair.vertices) {
            if (!boundary.u64(vertex)) {
                return Result<Manifest>::failure(planFailure("truncated boundary vertices"));
            }
        }
        if (!boundary.u64(edge_count) || edge_count > boundary.remaining() / 16) {
            return Result<Manifest>::failure(planFailure("invalid boundary edge list"));
        }
        pair.edges.resize(static_cast<size_t>(edge_count));
        for (EdgeKey& edge : pair.edges) {
            if (!boundary.u64(edge.a) || !boundary.u64(edge.b)) {
                return Result<Manifest>::failure(planFailure("truncated boundary edges"));
            }
        }
    }
    if (!boundary.atEnd()) {
        return Result<Manifest>::failure(planFailure("trailing bytes in boundary payload"));
    }
    for (BoundaryDef& pair : manifest.boundaries) {
        Writer writer;
        writer.u32(pair.chunk_a);
        writer.u32(pair.chunk_b);
        writer.u64(pair.vertices.size());
        for (uint64_t vertex : pair.vertices) writer.u64(vertex);
        writer.u64(pair.edges.size());
        for (const EdgeKey& edge : pair.edges) {
            writer.u64(edge.a);
            writer.u64(edge.b);
        }
        pair.hash = sha256Hex(writer.bytes());
    }
    return Result<Manifest>::success(std::move(manifest));
}

std::string localBoundaryHash(const Manifest& manifest, uint32_t chunk_id) {
    Writer writer;
    writer.string(manifest.boundary_hash);
    writer.u32(chunk_id);
    for (const BoundaryDef& boundary : manifest.boundaries) {
        if (boundary.chunk_a != chunk_id && boundary.chunk_b != chunk_id) continue;
        writer.u32(boundary.chunk_a);
        writer.u32(boundary.chunk_b);
        writer.string(boundary.hash);
    }
    return sha256Hex(writer.bytes());
}

std::vector<uint32_t> chunkIdsForFaceOwners(
    const Manifest& manifest,
    const std::vector<uint64_t>& face_owner_chunks) {
    (void)manifest;
    std::vector<uint32_t> result(face_owner_chunks.size());
    for (size_t i = 0; i < face_owner_chunks.size(); ++i) {
        result[i] = static_cast<uint32_t>(face_owner_chunks[i]);
    }
    return result;
}

Result<RepairPlanData> computeRepairPlan(
    const Mesh& mesh,
    const Config& config,
    const Manifest& manifest,
    const Topology& original_topology) {
    (void)original_topology;
    RepairPlanData plan;
    const size_t vertex_count = mesh.vertices.size();
    const size_t face_count = mesh.faces.size();

    std::vector<std::vector<uint32_t>> vertex_chunks(vertex_count);
    std::vector<uint32_t> face_owner(face_count, UINT32_MAX);
    for (const ChunkDef& chunk : manifest.chunks) {
        for (uint64_t face : chunk.faces) {
            if (face < face_count) face_owner[static_cast<size_t>(face)] = chunk.id;
        }
        for (uint64_t vertex : chunk.vertices) {
            if (vertex < vertex_count) {
                vertex_chunks[static_cast<size_t>(vertex)].push_back(chunk.id);
            }
        }
    }
    for (auto& chunks : vertex_chunks) {
        std::sort(chunks.begin(), chunks.end());
        chunks.erase(std::unique(chunks.begin(), chunks.end()), chunks.end());
    }

    Dsu weld(vertex_count);
    for (const ChunkDef& chunk : manifest.chunks) {
        weldCandidateSet(
            chunk.vertices,
            mesh.vertices,
            config.weld_tolerance,
            weld,
            vertex_chunks,
            manifest);
    }
    for (const BoundaryDef& boundary : manifest.boundaries) {
        for (uint64_t vertex : boundary.vertices) {
            if (vertex >= vertex_count) {
                return Result<RepairPlanData>::failure(makeError(
                    ErrorCode::MeshInvalid,
                    "boundary map references an invalid vertex",
                    boundary.chunk_a,
                    {},
                    vertex));
            }
        }
        weldCandidateSet(
            boundary.vertices,
            mesh.vertices,
            config.weld_tolerance,
            weld,
            vertex_chunks,
            manifest);
    }

    std::vector<uint64_t> weld_map(vertex_count);
    for (size_t i = 0; i < vertex_count; ++i) {
        weld_map[i] = static_cast<uint64_t>(weld.find(i));
    }
    std::map<uint64_t, Vec3> vertex_positions;
    for (size_t i = 0; i < vertex_count; ++i) {
        if (weld_map[i] == i) {
            vertex_positions[static_cast<uint64_t>(i)] = mesh.vertices[i];
        }
    }

    struct WorkingFace {
        ChunkFaceRecord record;
        std::array<uint64_t, 3> source_v{};
    };
    std::vector<WorkingFace> faces;
    faces.reserve(face_count);
    uint64_t collapsed = 0;
    for (size_t face_id = 0; face_id < face_count; ++face_id) {
        std::array<uint64_t, 3> remapped{};
        for (int corner = 0; corner < 3; ++corner) {
            remapped[static_cast<size_t>(corner)] =
                weld_map[static_cast<size_t>(mesh.faces[face_id].v[static_cast<size_t>(corner)])];
        }
        if (remapped[0] == remapped[1] || remapped[1] == remapped[2] ||
            remapped[2] == remapped[0]) {
            ++collapsed;
            continue;
        }
        WorkingFace face;
        face.record.source_face = face_id;
        face.record.v = remapped;
        face.record.owner_chunk = face_owner[face_id];
        face.source_v = remapped;
        faces.push_back(face);
    }

    std::set<uint64_t> original_used;
    for (const auto& face : mesh.faces) {
        for (uint64_t vertex : face.v) original_used.insert(vertex);
    }
    std::set<uint64_t> welded_used;
    for (const WorkingFace& face : faces) {
        for (uint64_t vertex : face.record.v) welded_used.insert(vertex);
    }
    plan.vertices_after_weld = welded_used.size();
    plan.welded_vertex_removals =
        original_used.size() >= welded_used.size()
            ? original_used.size() - welded_used.size()
            : 0;

    auto buildIncidence = [&faces]() {
        std::map<EdgeKey, std::vector<size_t>> incidence;
        for (size_t i = 0; i < faces.size(); ++i) {
            for (int corner = 0; corner < 3; ++corner) {
                incidence[EdgeKey::make(
                    faces[i].record.v[static_cast<size_t>(corner)],
                    faces[i].record.v[static_cast<size_t>((corner + 1) % 3)])]
                    .push_back(i);
            }
        }
        for (auto& entry : incidence) {
            std::sort(entry.second.begin(), entry.second.end());
            entry.second.erase(
                std::unique(entry.second.begin(), entry.second.end()),
                entry.second.end());
        }
        return incidence;
    };

    // Debris removal uses all shared edges (including non-manifold edges), so a
    // fin attached to the main surface is not classified as debris.
    {
        const auto incidence = buildIncidence();
        std::vector<std::vector<size_t>> neighbors(faces.size());
        for (const auto& entry : incidence) {
            if (entry.second.size() < 2) continue;
            for (size_t i = 0; i < entry.second.size(); ++i) {
                for (size_t j = i + 1; j < entry.second.size(); ++j) {
                    neighbors[entry.second[i]].push_back(entry.second[j]);
                    neighbors[entry.second[j]].push_back(entry.second[i]);
                }
            }
        }
        std::vector<uint8_t> visited(faces.size(), 0);
        std::vector<uint8_t> keep(faces.size(), 1);
        for (size_t start = 0; start < faces.size(); ++start) {
            if (visited[start]) continue;
            std::vector<size_t> component;
            std::vector<size_t> stack{start};
            visited[start] = 1;
            while (!stack.empty()) {
                const size_t current = stack.back();
                stack.pop_back();
                component.push_back(current);
                for (size_t next : neighbors[current]) {
                    if (!visited[next]) {
                        visited[next] = 1;
                        stack.push_back(next);
                    }
                }
            }
            if (component.size() < config.min_component_faces) {
                for (size_t index : component) keep[index] = 0;
                plan.debris_faces_removed += component.size();
            }
        }
        std::vector<WorkingFace> retained;
        retained.reserve(faces.size());
        for (size_t i = 0; i < faces.size(); ++i) {
            if (keep[i]) retained.push_back(std::move(faces[i]));
        }
        faces = std::move(retained);
    }

    // Hole extraction is performed before non-manifold splitting. Artificial
    // boundaries introduced by splitting are not holes and must not be filled.
    {
        const auto incidence = buildIncidence();
        std::vector<ChunkFaceRecord> face_records;
        face_records.reserve(faces.size());
        for (const WorkingFace& face : faces) face_records.push_back(face.record);
        std::vector<BoundaryWalkEdge> boundary_edges;
        const auto loops =
            traceBoundaryLoops(face_records, incidence, boundary_edges);
        std::vector<std::vector<uint64_t>> accepted_loops;
        std::vector<std::vector<uint32_t>> involved_chunks;
        std::vector<uint32_t> owners;
        for (const auto& loop : loops) {
            std::set<uint32_t> chunks;
            const std::set<uint64_t> loop_vertices(loop.begin(), loop.end());
            for (const BoundaryWalkEdge& edge : boundary_edges) {
                if (loop_vertices.count(edge.key.a) != 0 &&
                    loop_vertices.count(edge.key.b) != 0) {
                    chunks.insert(faces[edge.face].record.owner_chunk);
                }
            }
            std::vector<uint32_t> involved(chunks.begin(), chunks.end());
            if (involved.empty()) continue;
            accepted_loops.push_back(loop);
            involved_chunks.push_back(std::move(involved));
            owners.push_back(*std::min_element(chunks.begin(), chunks.end()));
        }

        std::set<std::array<uint64_t, 3>> existing;
        for (const WorkingFace& face : faces) {
            existing.insert(sortedTriangle(face.record.v));
        }
        struct FillFace {
            uint32_t owner;
            std::string hole_id;
            std::array<uint64_t, 3> triangle;
        };
        std::vector<FillFace> fills;
        for (size_t hole_index = 0; hole_index < accepted_loops.size(); ++hole_index) {
            const auto& loop = accepted_loops[hole_index];
            std::vector<uint64_t> sorted_loop = loop;
            std::sort(sorted_loop.begin(), sorted_loop.end());
            Writer id_writer;
            id_writer.string("hole-v1");
            for (uint64_t vertex : sorted_loop) id_writer.u64(vertex);
            const std::string hole_id = "H" + sha256Hex(id_writer.bytes()).substr(0, 24);

            Vec3 bbox_min{std::numeric_limits<double>::infinity(),
                          std::numeric_limits<double>::infinity(),
                          std::numeric_limits<double>::infinity()};
            Vec3 bbox_max{-std::numeric_limits<double>::infinity(),
                          -std::numeric_limits<double>::infinity(),
                          -std::numeric_limits<double>::infinity()};
            for (uint64_t vertex : loop) {
                const Vec3& p = vertex_positions.at(vertex);
                bbox_min.x = std::min(bbox_min.x, p.x);
                bbox_min.y = std::min(bbox_min.y, p.y);
                bbox_min.z = std::min(bbox_min.z, p.z);
                bbox_max.x = std::max(bbox_max.x, p.x);
                bbox_max.y = std::max(bbox_max.y, p.y);
                bbox_max.z = std::max(bbox_max.z, p.z);
            }

            std::vector<uint64_t> reversed(loop.rbegin(), loop.rend());
            const auto triangles = triangulateLoop(reversed, vertex_positions);
            HoleSummary summary;
            summary.id = hole_id;
            summary.owner_chunk = owners[hole_index];
            summary.involved_chunks = involved_chunks[hole_index];
            summary.vertex_count = loop.size();
            summary.bbox_min = bbox_min;
            summary.bbox_max = bbox_max;
            summary.cross_chunk = summary.involved_chunks.size() > 1;
            summary.status = "FILLED";
            for (const auto& triangle : triangles) {
                const auto key = sortedTriangle(triangle);
                if (!existing.insert(key).second) {
                    Error error = makeError(
                        ErrorCode::TopologyConflict,
                        "duplicate hole fill triangle",
                        static_cast<uint64_t>(summary.owner_chunk));
                    error.hole_id = hole_id;
                    error.face = key[0];
                    return Result<RepairPlanData>::failure(std::move(error));
                }
                fills.push_back(FillFace{summary.owner_chunk, hole_id, triangle});
                ++plan.filled_faces;
                ++summary.filled_faces;
            }
            plan.holes.push_back(std::move(summary));
        }
        for (const FillFace& fill : fills) {
            WorkingFace face;
            face.record.source_face = kGeneratedSourceFace;
            face.record.v = fill.triangle;
            face.record.owner_chunk = fill.owner;
            face.record.hole_id = fill.hole_id;
            face.record.fill = true;
            face.source_v = fill.triangle;
            faces.push_back(face);
        }
    }

    // Split non-manifold edges into deterministically ordered face groups. The
    // lowest group keeps the original vertex ids; every other group receives
    // stable generated ids. Each group records one owner chunk.
    {
        const auto incidence = buildIncidence();
        uint64_t next_vertex_id =
            static_cast<uint64_t>(std::max<size_t>(vertex_count, 1));
        std::vector<std::set<EdgeKey>> face_edges(faces.size());
        for (size_t i = 0; i < faces.size(); ++i) {
            for (int corner = 0; corner < 3; ++corner) {
                face_edges[i].insert(EdgeKey::make(
                    faces[i].record.v[static_cast<size_t>(corner)],
                    faces[i].record.v[static_cast<size_t>((corner + 1) % 3)]));
            }
        }
        for (const auto& entry : incidence) {
            if (entry.second.size() <= 2) continue;
            ++plan.split_edges;
            std::vector<size_t> incident = entry.second;
            Dsu local(incident.size());
            for (size_t i = 0; i < incident.size(); ++i) {
                for (size_t j = i + 1; j < incident.size(); ++j) {
                    bool connected = false;
                    for (const EdgeKey& candidate : face_edges[incident[i]]) {
                        if (candidate == entry.first) continue;
                        if (face_edges[incident[j]].count(candidate) != 0) {
                            connected = true;
                            break;
                        }
                    }
                    if (connected) local.unite(i, j);
                }
            }
            std::map<size_t, std::vector<size_t>> grouped;
            for (size_t i = 0; i < incident.size(); ++i) {
                grouped[local.find(i)].push_back(incident[i]);
            }
            std::vector<std::vector<size_t>> groups;
            for (auto& group : grouped) {
                std::sort(group.second.begin(), group.second.end());
                for (size_t offset = 0; offset < group.second.size(); offset += 2) {
                    const size_t end = std::min(group.second.size(), offset + 2);
                    groups.emplace_back(
                        group.second.begin() + static_cast<std::ptrdiff_t>(offset),
                        group.second.begin() + static_cast<std::ptrdiff_t>(end));
                }
            }
            std::sort(
                groups.begin(), groups.end(),
                [](const std::vector<size_t>& a, const std::vector<size_t>& b) {
                    return a.front() < b.front();
                });
            if (groups.size() <= 1) continue;
            plan.split_groups += groups.size();
            for (size_t group_index = 0; group_index < groups.size(); ++group_index) {
                uint32_t owner = UINT32_MAX;
                for (size_t face_index : groups[group_index]) {
                    owner = std::min(owner, faces[face_index].record.owner_chunk);
                }
                plan.split_ownership.push_back(SplitOwnership{
                    entry.first.a,
                    entry.first.b,
                    owner,
                    static_cast<uint32_t>(group_index),
                    groups[group_index].size()});
            }
            for (size_t group_index = 1; group_index < groups.size(); ++group_index) {
                const uint64_t copy_a = next_vertex_id++;
                const uint64_t copy_b = next_vertex_id++;
                ++plan.split_vertex_copies;
                vertex_positions[copy_a] = vertex_positions.at(entry.first.a);
                vertex_positions[copy_b] = vertex_positions.at(entry.first.b);
                for (size_t face_index : groups[group_index]) {
                    for (int corner = 0; corner < 3; ++corner) {
                        uint64_t& value = faces[face_index].record.v[static_cast<size_t>(corner)];
                        if (value == entry.first.a) value = copy_a;
                        if (value == entry.first.b) value = copy_b;
                    }
                }
            }
        }
    }

    std::set<uint64_t> used_ids;
    for (const WorkingFace& face : faces) {
        used_ids.insert(face.record.v.begin(), face.record.v.end());
    }
    for (const auto& entry : vertex_positions) {
        if (used_ids.count(entry.first) != 0) {
            plan.global_vertices.push_back(ChunkVertexRecord{entry.first, entry.second});
        }
    }
    std::sort(
        plan.global_vertices.begin(),
        plan.global_vertices.end(),
        [](const ChunkVertexRecord& a, const ChunkVertexRecord& b) {
            return a.id < b.id;
        });

    for (const WorkingFace& face : faces) {
        plan.faces.push_back(face.record);
    }
    std::sort(
        plan.faces.begin(),
        plan.faces.end(),
        [](const ChunkFaceRecord& a, const ChunkFaceRecord& b) {
            if (a.owner_chunk != b.owner_chunk) return a.owner_chunk < b.owner_chunk;
            if (a.fill != b.fill) return !a.fill;
            if (!a.fill && a.source_face != b.source_face) {
                return a.source_face < b.source_face;
            }
            if (a.hole_id != b.hole_id) return a.hole_id < b.hole_id;
            return sortedTriangle(a.v) < sortedTriangle(b.v);
        });

    for (HoleSummary& hole : plan.holes) {
        bool has_fill = false;
        for (const ChunkFaceRecord& face : plan.faces) {
            if (face.fill && face.hole_id == hole.id) {
                has_fill = true;
                break;
            }
        }
        if (!has_fill) hole.status = "SKIPPED";
    }
    return Result<RepairPlanData>::success(std::move(plan));
}

std::vector<uint8_t> serializeChunkBundle(const ChunkBundle& bundle) {
    Writer writer;
    writer.raw(kChunkMagic, sizeof(kChunkMagic));
    writer.u32(kChunkFormatVersion);
    writer.u32(bundle.id);
    writer.string(bundle.input_hash);
    writer.string(bundle.table_hash);
    writer.string(bundle.boundary_hash);
    writer.string(bundle.local_boundary_hash);
    writer.string(bundle.prev_chain_hash);
    writer.u64(bundle.filled_faces);
    writer.u64(bundle.split_groups);
    writer.u64(bundle.vertices.size());
    for (const ChunkVertexRecord& vertex : bundle.vertices) {
        writer.u64(vertex.id);
        writer.f64(vertex.position.x);
        writer.f64(vertex.position.y);
        writer.f64(vertex.position.z);
    }
    writer.u64(bundle.faces.size());
    for (const ChunkFaceRecord& face : bundle.faces) {
        writer.u64(face.source_face);
        writer.u64(face.v[0]);
        writer.u64(face.v[1]);
        writer.u64(face.v[2]);
        writer.u32(face.owner_chunk);
        writer.u8(face.fill ? 1 : 0);
        writer.string(face.hole_id);
    }
    const std::string checksum = sha256Hex(writer.bytes());
    std::array<uint8_t, 32> digest{};
    for (size_t i = 0; i < digest.size(); ++i) {
        digest[i] = static_cast<uint8_t>(
            std::stoul(checksum.substr(i * 2, 2), nullptr, 16));
    }
    writer.raw(digest.data(), digest.size());
    return writer.bytes();
}

Result<ChunkBundle> parseChunkBundleBytes(const std::vector<uint8_t>& bytes) {
    if (bytes.size() < sizeof(kChunkMagic) + 4 + 32) {
        return Result<ChunkBundle>::failure(planFailure("chunk bundle is truncated"));
    }
    const size_t body_size = bytes.size() - 32;
    const std::string expected_checksum = sha256Hex(bytes.data(), body_size);
    const std::string stored_checksum =
        hexBytes(bytes.data() + body_size, 32);
    if (expected_checksum != stored_checksum) {
        return Result<ChunkBundle>::failure(planFailure("chunk bundle checksum mismatch"));
    }
    Reader reader(bytes.data(), body_size);
    char magic[8]{};
    uint32_t version = 0;
    ChunkBundle bundle;
    if (!reader.raw(magic, sizeof(magic)) ||
        std::memcmp(magic, kChunkMagic, sizeof(magic)) != 0 ||
        !reader.u32(version) || version != kChunkFormatVersion ||
        !reader.u32(bundle.id) ||
        !reader.string(bundle.input_hash) ||
        !reader.string(bundle.table_hash) ||
        !reader.string(bundle.boundary_hash) ||
        !reader.string(bundle.local_boundary_hash) ||
        !reader.string(bundle.prev_chain_hash) ||
        !reader.u64(bundle.filled_faces) ||
        !reader.u64(bundle.split_groups)) {
        return Result<ChunkBundle>::failure(planFailure("invalid chunk bundle header"));
    }
    uint64_t vertex_count = 0;
    if (!reader.u64(vertex_count) || vertex_count > reader.remaining() / 32) {
        return Result<ChunkBundle>::failure(planFailure("invalid chunk vertex count"));
    }
    bundle.vertices.resize(static_cast<size_t>(vertex_count));
    for (ChunkVertexRecord& vertex : bundle.vertices) {
        if (!reader.u64(vertex.id) || !reader.f64(vertex.position.x) ||
            !reader.f64(vertex.position.y) || !reader.f64(vertex.position.z)) {
            return Result<ChunkBundle>::failure(planFailure("truncated chunk vertices"));
        }
    }
    uint64_t face_count = 0;
    if (!reader.u64(face_count) || face_count > reader.remaining() / 37) {
        return Result<ChunkBundle>::failure(planFailure("invalid chunk face count"));
    }
    bundle.faces.resize(static_cast<size_t>(face_count));
    for (ChunkFaceRecord& face : bundle.faces) {
        uint8_t fill = 0;
        if (!reader.u64(face.source_face) ||
            !reader.u64(face.v[0]) ||
            !reader.u64(face.v[1]) ||
            !reader.u64(face.v[2]) ||
            !reader.u32(face.owner_chunk) ||
            !reader.u8(fill) ||
            !reader.string(face.hole_id)) {
            return Result<ChunkBundle>::failure(planFailure("truncated chunk faces"));
        }
        face.fill = fill != 0;
    }
    if (!reader.atEnd()) {
        return Result<ChunkBundle>::failure(planFailure("trailing bytes in chunk bundle"));
    }
    bundle.hash = expected_checksum;
    return Result<ChunkBundle>::success(std::move(bundle));
}

Result<ChunkBundle> readChunkBundle(const std::string& path) {
    auto bytes = readFileBytes(path);
    if (!bytes) return Result<ChunkBundle>::failure(bytes.error);
    return parseChunkBundleBytes(*bytes);
}

Status writeChunkBundle(const std::string& path, const ChunkBundle& bundle) {
    return atomicWriteFile(path, serializeChunkBundle(bundle));
}

Result<ChunkBundle> makeChunkBundle(
    const RepairPlanData& plan,
    const Manifest& manifest,
    uint32_t chunk_id,
    const std::string& prev_chain_hash) {
    ChunkBundle bundle;
    bundle.id = chunk_id;
    bundle.input_hash = manifest.input_hash;
    bundle.table_hash = manifest.table_hash;
    bundle.boundary_hash = manifest.boundary_hash;
    bundle.local_boundary_hash = localBoundaryHash(manifest, chunk_id);
    bundle.prev_chain_hash = prev_chain_hash;

    std::map<uint64_t, Vec3> all_positions;
    for (const ChunkVertexRecord& vertex : plan.global_vertices) {
        all_positions[vertex.id] = vertex.position;
    }
    std::set<uint64_t> used;
    for (const ChunkFaceRecord& face : plan.faces) {
        if (face.owner_chunk != chunk_id) continue;
        bundle.faces.push_back(face);
        used.insert(face.v.begin(), face.v.end());
        if (face.fill) ++bundle.filled_faces;
    }
    for (uint64_t id : used) {
        auto found = all_positions.find(id);
        if (found == all_positions.end()) {
            return Result<ChunkBundle>::failure(makeError(
                ErrorCode::Internal,
                "repair plan lost a referenced vertex",
                chunk_id,
                {},
                id));
        }
        bundle.vertices.push_back(ChunkVertexRecord{id, found->second});
    }
    bundle.split_groups = 0;
    const std::vector<uint8_t> bytes = serializeChunkBundle(bundle);
    bundle.hash = sha256Hex(bytes.data(), bytes.size() - 32);
    return Result<ChunkBundle>::success(std::move(bundle));
}

Result<StitchResult> stitchChunks(
    const Mesh& original,
    const Manifest& manifest,
    const std::vector<ChunkBundle>& bundles) {
    (void)original;
    StitchResult result;
    std::map<uint64_t, Vec3> vertices;
    std::map<uint64_t, uint32_t> vertex_chunk;
    std::set<std::array<uint64_t, 3>> seen_fill_triangles;
    std::set<std::array<uint64_t, 3>> seen_triangles;
    std::vector<ChunkFaceRecord> faces;
    std::vector<uint32_t> face_chunk;

    std::vector<const ChunkBundle*> ordered;
    ordered.reserve(bundles.size());
    for (const ChunkBundle& bundle : bundles) ordered.push_back(&bundle);
    std::sort(
        ordered.begin(), ordered.end(),
        [](const ChunkBundle* a, const ChunkBundle* b) { return a->id < b->id; });
    for (const ChunkBundle* bundle_ptr : ordered) {
        const ChunkBundle& bundle = *bundle_ptr;
        for (const ChunkVertexRecord& vertex : bundle.vertices) {
            auto found = vertices.find(vertex.id);
            if (found == vertices.end()) {
                vertices[vertex.id] = vertex.position;
                vertex_chunk[vertex.id] = bundle.id;
            } else if (!(found->second == vertex.position)) {
                Error error = makeError(
                    ErrorCode::TopologyConflict,
                    "chunk vertex id has conflicting positions",
                    bundle.id,
                    {},
                    vertex.id);
                result.error = error;
                return Result<StitchResult>::failure(error);
            }
        }
        for (const ChunkFaceRecord& face : bundle.faces) {
            const auto key = sortedTriangle(face.v);
            if (face.fill) {
                if (!seen_fill_triangles.insert(key).second) {
                    Error error = makeError(
                        ErrorCode::TopologyConflict,
                        "duplicate fill triangle across chunks",
                        bundle.id);
                    error.hole_id = face.hole_id;
                    error.face = face.source_face;
                    result.error = error;
                    return Result<StitchResult>::failure(error);
                }
                ++result.duplicate_fill_faces;
            }
            if (!seen_triangles.insert(key).second && face.fill) {
                Error error = makeError(
                    ErrorCode::TopologyConflict,
                    "fill triangle overlaps an existing triangle",
                    bundle.id);
                error.hole_id = face.hole_id;
                result.error = error;
                return Result<StitchResult>::failure(error);
            }
            faces.push_back(face);
            face_chunk.push_back(bundle.id);
        }
    }

    std::map<EdgeKey, uint64_t> final_refs;
    for (const ChunkFaceRecord& face : faces) {
        for (int corner = 0; corner < 3; ++corner) {
            ++final_refs[EdgeKey::make(
                face.v[static_cast<size_t>(corner)],
                face.v[static_cast<size_t>((corner + 1) % 3)])];
        }
    }
    for (const BoundaryDef& boundary : manifest.boundaries) {
        for (const EdgeKey& edge : boundary.edges) {
            auto found = final_refs.find(edge);
            if (found == final_refs.end() || found->second != 2) {
                ++result.invalid_boundary_edges;
                Error error = makeError(
                    ErrorCode::TopologyConflict,
                    "cross-chunk boundary edge is not referenced exactly twice",
                    boundary.chunk_a);
                error.edge_a = edge.a;
                error.edge_b = edge.b;
                result.error = error;
                return Result<StitchResult>::failure(error);
            }
        }
    }

    std::vector<uint64_t> ids;
    ids.reserve(vertices.size());
    for (const auto& entry : vertices) ids.push_back(entry.first);
    for (const ChunkFaceRecord& face : faces) {
        for (uint64_t vertex : face.v) {
            if (vertices.count(vertex) == 0) {
                Error error = makeError(
                    ErrorCode::TopologyConflict,
                    "stitched face references a vertex absent from every committed chunk",
                    face.owner_chunk,
                    face.source_face);
                error.vertex = vertex;
                result.error = error;
                return Result<StitchResult>::failure(error);
            }
        }
    }
    std::map<uint64_t, uint64_t> compact;
    result.mesh.vertices.reserve(ids.size());
    for (size_t i = 0; i < ids.size(); ++i) {
        compact[ids[i]] = i;
        result.mesh.vertices.push_back(vertices[ids[i]]);
    }
    result.mesh.faces.reserve(faces.size());
    for (const ChunkFaceRecord& face : faces) {
        result.mesh.faces.push_back(Face{{
            compact.at(face.v[0]),
            compact.at(face.v[1]),
            compact.at(face.v[2])}});
    }
    result.mesh.source_coord_type = 1;
    result.mesh.source_index_width = 8;
    auto bytes = serializeMesh(result.mesh, 1, 8);
    if (!bytes) {
        return Result<StitchResult>::failure(bytes.error);
    }
    result.hash = sha256Hex(*bytes);
    result.face_chunk = std::move(face_chunk);
    return Result<StitchResult>::success(std::move(result));
}

}  // namespace meshrepair::detail
