#include "core/Welding.h"

#include <algorithm>
#include <array>
#include <cmath>
#include <cstdint>
#include <locale>
#include <map>
#include <set>
#include <sstream>
#include <unordered_map>

namespace meshedit {
namespace {

struct Cell {
    std::int64_t x = 0;
    std::int64_t y = 0;
    std::int64_t z = 0;

    bool operator==(const Cell& other) const {
        return x == other.x && y == other.y && z == other.z;
    }
};

struct CellHash {
    std::size_t operator()(const Cell& cell) const {
        auto mix = [](std::uint64_t value) {
            value ^= value >> 30;
            value *= 0xbf58476d1ce4e5b9ULL;
            value ^= value >> 27;
            value *= 0x94d049bb133111ebULL;
            value ^= value >> 31;
            return value;
        };
        const std::uint64_t hx = mix(static_cast<std::uint64_t>(cell.x));
        const std::uint64_t hy = mix(static_cast<std::uint64_t>(cell.y) + 0x9e3779b97f4a7c15ULL);
        const std::uint64_t hz = mix(static_cast<std::uint64_t>(cell.z) + 0x243f6a8885a308d3ULL);
        return static_cast<std::size_t>(hx ^ (hy << 1) ^ (hz << 2));
    }
};

Cell cellOf(const Vec3& point, double tolerance) {
    return {static_cast<std::int64_t>(std::floor(point.x / tolerance)),
            static_cast<std::int64_t>(std::floor(point.y / tolerance)),
            static_cast<std::int64_t>(std::floor(point.z / tolerance))};
}

std::array<double, 3> exactKey(const Vec3& point) {
    auto normalizeZero = [](double value) { return value == 0.0 ? 0.0 : value; };
    return {normalizeZero(point.x), normalizeZero(point.y), normalizeZero(point.z)};
}

} // namespace

std::string WeldReport::toJson() const {
    std::ostringstream out;
    out.imbue(std::locale::classic());
    out << "{\"oldVertices\":" << oldVertexCount
        << ",\"newVertices\":" << newVertexCount
        << ",\"mergedVertices\":" << mergedVertexCount
        << ",\"affectedFaces\":" << affectedFaceCount
        << ",\"removedDegenerateFaces\":" << removedDegenerateFaces
        << ",\"boundaryEdges\":" << boundaryEdgeCount
        << ",\"boundaryLoops\":" << boundaryLoopCount
        << ",\"nonManifoldEdges\":" << nonManifoldEdgeCount
        << ",\"mergeMap\":[";
    for (std::size_t i = 0; i < vertexMergeMap.size(); ++i) {
        if (i) out << ',';
        out << vertexMergeMap[i];
    }
    out << "]}";
    return out.str();
}

Result<WeldReport> weldMesh(Mesh& mesh, const WeldOptions& options) {
    if (!std::isfinite(options.tolerance) || options.tolerance < 0.0) {
        return Error{Code::WeldInvalid, "welding tolerance must be finite and non-negative",
                     0, {}};
    }

    WeldReport report;
    report.oldVertexCount = mesh.positions.size();
    if (mesh.positions.empty()) {
        return Error{Code::WeldInvalid, "mesh has no vertices to weld", 0, {}};
    }

    const std::size_t oldCount = mesh.positions.size();
    std::vector<int> representative(oldCount);
    std::vector<int> representatives;
    representatives.reserve(oldCount);

    if (options.tolerance <= 1e-15) {
        std::map<std::array<double, 3>, int> exact;
        for (std::size_t i = 0; i < oldCount; ++i) {
            const auto key = exactKey(mesh.positions[i]);
            const auto found = exact.find(key);
            if (found == exact.end()) {
                const int rep = static_cast<int>(i);
                exact.emplace(key, rep);
                representative[i] = rep;
                representatives.push_back(rep);
            } else {
                representative[i] = found->second;
            }
        }
    } else {
        std::unordered_map<Cell, std::vector<int>, CellHash> cells;
        cells.reserve(oldCount * 2 + 1);
        for (std::size_t i = 0; i < oldCount; ++i) {
            const Vec3& point = mesh.positions[i];
            const Cell cell = cellOf(point, options.tolerance);
            int chosen = -1;
            for (std::int64_t dx = -1; dx <= 1 && chosen < 0; ++dx) {
                for (std::int64_t dy = -1; dy <= 1 && chosen < 0; ++dy) {
                    for (std::int64_t dz = -1; dz <= 1 && chosen < 0; ++dz) {
                        const Cell neighbor{cell.x + dx, cell.y + dy, cell.z + dz};
                        const auto found = cells.find(neighbor);
                        if (found == cells.end()) continue;
                        for (int candidate : found->second) {
                            if (nearlyEqual(point, mesh.positions[static_cast<std::size_t>(candidate)],
                                            options.tolerance)) {
                                chosen = candidate;
                                break;
                            }
                        }
                    }
                }
            }
            if (chosen >= 0) {
                representative[i] = chosen;
            } else {
                const int rep = static_cast<int>(i);
                representative[i] = rep;
                representatives.push_back(rep);
                cells[cell].push_back(rep);
            }
        }
    }

    std::vector<int> compressed(oldCount, -1);
    std::vector<Vec3> newPositions;
    newPositions.reserve(representatives.size());
    for (std::size_t i = 0; i < representatives.size(); ++i) {
        const int oldIndex = representatives[i];
        compressed[static_cast<std::size_t>(oldIndex)] = static_cast<int>(i);
        newPositions.push_back(mesh.positions[static_cast<std::size_t>(oldIndex)]);
    }
    report.vertexMergeMap.resize(oldCount);
    for (std::size_t i = 0; i < oldCount; ++i) {
        report.vertexMergeMap[i] = compressed[static_cast<std::size_t>(representative[i])];
    }

    std::vector<Face> newFaces;
    newFaces.reserve(mesh.faces.size());
    std::size_t affectedFaces = 0;
    std::size_t removed = 0;
    for (const Face& original : mesh.faces) {
        Face face = original;
        bool changed = false;
        for (Corner& corner : face.corners) {
            if (corner.position < 0 ||
                static_cast<std::size_t>(corner.position) >= oldCount) {
                return Error{Code::WeldInvalid, "face references an invalid position", 0, {}};
            }
            const int mapped = report.vertexMergeMap[static_cast<std::size_t>(corner.position)];
            changed = changed || mapped != corner.position;
            corner.position = mapped;
        }
        const bool degenerate = face.corners[0].position == face.corners[1].position ||
            face.corners[1].position == face.corners[2].position ||
            face.corners[2].position == face.corners[0].position;
        if (degenerate) {
            ++removed;
            ++affectedFaces;
            continue;
        }
        if (changed) ++affectedFaces;
        newFaces.push_back(std::move(face));
    }

    mesh.positions = std::move(newPositions);
    mesh.faces = std::move(newFaces);
    mesh.welded = true;
    report.newVertexCount = mesh.positions.size();
    report.mergedVertexCount = report.oldVertexCount - report.newVertexCount;
    report.affectedFaceCount = affectedFaces;
    report.removedDegenerateFaces = removed;
    if (options.recomputeNormals) recomputeNormals(mesh);
    const Topology topology = buildTopology(mesh);
    report.boundaryEdgeCount = topology.boundaryEdges.size();
    report.boundaryLoopCount = topology.boundaryLoops.size();
    report.nonManifoldEdgeCount = topology.nonManifoldEdges.size();
    return Result<WeldReport>(std::move(report));
}

} // namespace meshedit
