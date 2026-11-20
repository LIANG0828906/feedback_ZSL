#include "core/Editing.h"

#include "core/Topology.h"

#include <algorithm>
#include <array>
#include <cmath>
#include <locale>
#include <queue>
#include <set>
#include <sstream>
#include <unordered_map>

namespace meshedit {
namespace {

void transformPoint(Vec3& point, const TransformSpec& transform) {
    point = point - transform.pivot;
    point.x *= transform.scale.x;
    point.y *= transform.scale.y;
    point.z *= transform.scale.z;
    if (transform.rotationDegrees != 0.0) {
        Vec3 axis = normalized(transform.rotationAxis);
        if (lengthSquared(axis) > 0.0) {
            const double radians = transform.rotationDegrees * 3.14159265358979323846 / 180.0;
            const double c = std::cos(radians);
            const double s = std::sin(radians);
            point = point * c + cross(axis, point) * s + axis * (dot(axis, point) * (1.0 - c));
        }
    }
    point = point + transform.pivot + transform.translation;
}

struct CollapseCandidate {
    int a = 0;
    int b = 0;
    double length = 0.0;
    int versionA = 0;
    int versionB = 0;
};

struct CandidateGreater {
    bool operator()(const CollapseCandidate& left, const CollapseCandidate& right) const {
        if (left.length != right.length) return left.length > right.length;
        if (left.a != right.a) return left.a > right.a;
        return left.b > right.b;
    }
};

void removeFace(std::vector<bool>& alive, int face, std::vector<int>& aliveCount) {
    if (face < 0 || static_cast<std::size_t>(face) >= alive.size() || !alive[static_cast<std::size_t>(face)]) {
        return;
    }
    alive[static_cast<std::size_t>(face)] = false;
    --aliveCount[0];
}

} // namespace

void EditHistory::clear() {
    undo_.clear();
    redo_.clear();
}

void EditHistory::push(EditRecord record) {
    undo_.push_back(std::move(record));
    redo_.clear();
}

bool EditHistory::undo(Mesh& current) {
    if (undo_.empty()) return false;
    EditRecord record = std::move(undo_.back());
    undo_.pop_back();
    current = record.before;
    redo_.push_back(std::move(record));
    return true;
}

bool EditHistory::redo(Mesh& current) {
    if (redo_.empty()) return false;
    EditRecord record = std::move(redo_.back());
    redo_.pop_back();
    current = record.after;
    undo_.push_back(std::move(record));
    return true;
}

Selection selectionFromFaces(const Mesh& mesh, const std::vector<int>& faceIds) {
    Selection selection;
    std::set<int> vertices;
    for (int faceId : faceIds) {
        if (faceId < 0 || static_cast<std::size_t>(faceId) >= mesh.faces.size()) continue;
        selection.faces.push_back(faceId);
        for (const Corner& corner : mesh.faces[static_cast<std::size_t>(faceId)].corners) {
            vertices.insert(corner.position);
        }
    }
    selection.vertices.assign(vertices.begin(), vertices.end());
    return selection;
}

Selection selectVerticesInBounds(const Mesh& mesh, const Aabb& bounds) {
    Selection selection;
    for (std::size_t i = 0; i < mesh.positions.size(); ++i) {
        const Vec3& p = mesh.positions[i];
        if (p.x >= bounds.min.x && p.x <= bounds.max.x &&
            p.y >= bounds.min.y && p.y <= bounds.max.y &&
            p.z >= bounds.min.z && p.z <= bounds.max.z) {
            selection.vertices.push_back(static_cast<int>(i));
        }
    }
    return selection;
}

Selection selectFacesInBounds(const Mesh& mesh, const Aabb& bounds) {
    Selection selection;
    for (std::size_t i = 0; i < mesh.faces.size(); ++i) {
        Vec3 center{};
        bool valid = true;
        for (const Corner& corner : mesh.faces[i].corners) {
            if (corner.position < 0 ||
                static_cast<std::size_t>(corner.position) >= mesh.positions.size()) {
                valid = false;
                break;
            }
            center += mesh.positions[static_cast<std::size_t>(corner.position)];
        }
        if (!valid) continue;
        center = center / 3.0;
        if (center.x >= bounds.min.x && center.x <= bounds.max.x &&
            center.y >= bounds.min.y && center.y <= bounds.max.y &&
            center.z >= bounds.min.z && center.z <= bounds.max.z) {
            selection.faces.push_back(static_cast<int>(i));
        }
    }
    for (const int vertex : selectionFromFaces(mesh, selection.faces).vertices) {
        selection.vertices.push_back(vertex);
    }
    return selection;
}

bool transformSelection(Mesh& mesh, const Selection& selection,
                        const TransformSpec& transform, std::string& error) {
    std::set<int> vertices(selection.vertices.begin(), selection.vertices.end());
    for (int faceId : selection.faces) {
        if (faceId < 0 || static_cast<std::size_t>(faceId) >= mesh.faces.size()) {
            error = "selected face index is out of range";
            return false;
        }
        for (const Corner& corner : mesh.faces[static_cast<std::size_t>(faceId)].corners) {
            vertices.insert(corner.position);
        }
    }
    if (vertices.empty()) {
        error = "selection contains no vertices";
        return false;
    }
    for (int vertex : vertices) {
        if (vertex < 0 || static_cast<std::size_t>(vertex) >= mesh.positions.size()) {
            error = "selected vertex index is out of range";
            return false;
        }
        transformPoint(mesh.positions[static_cast<std::size_t>(vertex)], transform);
    }
    recomputeNormals(mesh);
    return true;
}

Result<EditOutcome> applyTransform(const Mesh& input, const Selection& selection,
                                   const TransformSpec& transform) {
    if (selection.empty()) {
        return Error{Code::InvalidArgument, "select vertices or faces before transforming",
                     0, {}};
    }
    if (transform.scale.x == 0.0 || transform.scale.y == 0.0 || transform.scale.z == 0.0) {
        return Error{Code::InvalidArgument, "zero scale would collapse the selection", 0, {}};
    }
    EditOutcome outcome;
    outcome.mesh = input;
    outcome.beforeMetrics = computeMetrics(input);
    std::string why;
    if (!transformSelection(outcome.mesh, selection, transform, why)) {
        return Error{Code::InvalidArgument, why, 0, {}};
    }
    outcome.afterMetrics = computeMetrics(outcome.mesh);
    outcome.operation = "transform";
    outcome.affectedFaceCount = selection.faces.size();
    std::ostringstream detail;
    detail.imbue(std::locale::classic());
    detail << "translate=(" << formatDouble(transform.translation.x) << ','
           << formatDouble(transform.translation.y) << ','
           << formatDouble(transform.translation.z) << ") rotate="
           << formatDouble(transform.rotationDegrees) << "deg axis=("
           << formatDouble(transform.rotationAxis.x) << ','
           << formatDouble(transform.rotationAxis.y) << ','
           << formatDouble(transform.rotationAxis.z) << ") scale=("
           << formatDouble(transform.scale.x) << ','
           << formatDouble(transform.scale.y) << ','
           << formatDouble(transform.scale.z) << ") selectedVertices="
           << selection.vertices.size();
    outcome.detail = detail.str();
    return Result<EditOutcome>(std::move(outcome));
}

Result<EditOutcome> applySimplify(const Mesh& input, const SimplifyOptions& options) {
    if (!std::isfinite(options.targetRatio) || options.targetRatio <= 0.0 ||
        options.targetRatio >= 1.0) {
        return Error{Code::InvalidArgument,
                     "simplification target ratio must be in (0, 1)", 0, {}};
    }
    if (input.faces.size() < 2) {
        return Error{Code::InvalidArgument, "mesh is too small to simplify", 0, {}};
    }
    EditOutcome outcome;
    outcome.mesh = input;
    outcome.beforeMetrics = computeMetrics(input);
    outcome.operation = "simplify";
    Mesh& mesh = outcome.mesh;
    const std::size_t originalFaces = mesh.faces.size();
    const std::size_t targetFaces = std::max<std::size_t>(
        1, static_cast<std::size_t>(std::floor(
               static_cast<double>(originalFaces) * options.targetRatio)));
    if (targetFaces >= originalFaces) {
        return Error{Code::InvalidArgument, "target keeps every face", 0, {}};
    }

    std::vector<bool> faceAlive(mesh.faces.size(), true);
    std::vector<bool> vertexAlive(mesh.positions.size(), true);
    std::vector<int> vertexVersion(mesh.positions.size(), 0);
    std::vector<std::set<int>> vertexFaces(mesh.positions.size());
    std::map<std::array<int, 3>, int> faceByKey;
    std::vector<int> aliveCount{static_cast<int>(mesh.faces.size())};
    std::size_t movedFaces = 0;

    for (std::size_t fi = 0; fi < mesh.faces.size(); ++fi) {
        const Face& face = mesh.faces[fi];
        int p[3] = {face.corners[0].position, face.corners[1].position,
                    face.corners[2].position};
        for (int v : p) vertexFaces[static_cast<std::size_t>(v)].insert(static_cast<int>(fi));
        for (int i = 0; i < 3; ++i) {
            for (int j = i + 1; j < 3; ++j) {
                if (p[i] == p[j]) continue;
            }
        }
        std::array<int, 3> key = {p[0], p[1], p[2]};
        std::sort(key.begin(), key.end());
        faceByKey.emplace(key, static_cast<int>(fi));
    }

    std::priority_queue<CollapseCandidate, std::vector<CollapseCandidate>, CandidateGreater>
        candidates;
    std::set<std::pair<int, int>> queuedEdges;
    auto enqueueEdge = [&](int a, int b) {
        if (a < 0 || b < 0 || a == b ||
            static_cast<std::size_t>(a) >= mesh.positions.size() ||
            static_cast<std::size_t>(b) >= mesh.positions.size() ||
            !vertexAlive[static_cast<std::size_t>(a)] ||
            !vertexAlive[static_cast<std::size_t>(b)]) {
            return;
        }
        if (a > b) std::swap(a, b);
        const std::pair<int, int> edge{a, b};
        if (!queuedEdges.insert(edge).second) return;
        CollapseCandidate candidate;
        candidate.a = a;
        candidate.b = b;
        candidate.length = length(mesh.positions[static_cast<std::size_t>(b)] -
                                  mesh.positions[static_cast<std::size_t>(a)]);
        candidate.versionA = vertexVersion[static_cast<std::size_t>(a)];
        candidate.versionB = vertexVersion[static_cast<std::size_t>(b)];
        candidates.push(candidate);
    };
    for (std::size_t fi = 0; fi < mesh.faces.size(); ++fi) {
        const Face& face = mesh.faces[fi];
        enqueueEdge(face.corners[0].position, face.corners[1].position);
        enqueueEdge(face.corners[1].position, face.corners[2].position);
        enqueueEdge(face.corners[2].position, face.corners[0].position);
    }

    while (aliveCount[0] > static_cast<int>(targetFaces) && !candidates.empty()) {
        const CollapseCandidate candidate = candidates.top();
        candidates.pop();
        const std::pair<int, int> edge{candidate.a, candidate.b};
        queuedEdges.erase(edge);
        if (!vertexAlive[static_cast<std::size_t>(candidate.a)] ||
            !vertexAlive[static_cast<std::size_t>(candidate.b)]) {
            continue;
        }
        if (vertexVersion[static_cast<std::size_t>(candidate.a)] != candidate.versionA ||
            vertexVersion[static_cast<std::size_t>(candidate.b)] != candidate.versionB) {
            enqueueEdge(candidate.a, candidate.b);
            continue;
        }
        const int a = candidate.a;
        const int b = candidate.b;
        std::vector<int> shared;
        std::set_intersection(vertexFaces[static_cast<std::size_t>(a)].begin(),
                              vertexFaces[static_cast<std::size_t>(a)].end(),
                              vertexFaces[static_cast<std::size_t>(b)].begin(),
                              vertexFaces[static_cast<std::size_t>(b)].end(),
                              std::back_inserter(shared));
        if (shared.size() != 2) continue;
        bool incidentAlive = true;
        for (int face : shared) incidentAlive = incidentAlive && faceAlive[static_cast<std::size_t>(face)];
        if (!incidentAlive) continue;

        for (int faceId : shared) {
            Face& face = mesh.faces[static_cast<std::size_t>(faceId)];
            std::array<int, 3> key = {face.corners[0].position, face.corners[1].position,
                                      face.corners[2].position};
            std::sort(key.begin(), key.end());
            const auto found = faceByKey.find(key);
            if (found != faceByKey.end() && found->second == faceId) faceByKey.erase(found);
            for (const Corner& corner : face.corners) {
                vertexFaces[static_cast<std::size_t>(corner.position)].erase(faceId);
            }
            removeFace(faceAlive, faceId, aliveCount);
        }

        std::vector<int> moved = vertexFaces[static_cast<std::size_t>(b)].begin() ==
                vertexFaces[static_cast<std::size_t>(b)].end()
            ? std::vector<int>{}
            : std::vector<int>(vertexFaces[static_cast<std::size_t>(b)].begin(),
                               vertexFaces[static_cast<std::size_t>(b)].end());
        std::vector<int> toRemove;
        for (int faceId : moved) {
            if (!faceAlive[static_cast<std::size_t>(faceId)]) continue;
            Face& face = mesh.faces[static_cast<std::size_t>(faceId)];
            std::array<int, 3> oldKey = {face.corners[0].position, face.corners[1].position,
                                         face.corners[2].position};
            std::sort(oldKey.begin(), oldKey.end());
            const auto old = faceByKey.find(oldKey);
            if (old != faceByKey.end() && old->second == faceId) faceByKey.erase(old);
            for (Corner& corner : face.corners) {
                if (corner.position == b) corner.position = a;
            }
            bool degenerate = face.corners[0].position == face.corners[1].position ||
                face.corners[1].position == face.corners[2].position ||
                face.corners[2].position == face.corners[0].position;
            std::array<int, 3> newKey = {face.corners[0].position, face.corners[1].position,
                                         face.corners[2].position};
            std::sort(newKey.begin(), newKey.end());
            if (!degenerate) {
                const auto duplicate = faceByKey.find(newKey);
                if (duplicate != faceByKey.end() &&
                    faceAlive[static_cast<std::size_t>(duplicate->second)]) {
                    degenerate = true;
                }
            }
            if (degenerate) {
                toRemove.push_back(faceId);
                continue;
            }
            vertexFaces[static_cast<std::size_t>(a)].insert(faceId);
            faceByKey[newKey] = faceId;
            ++movedFaces;
        }
        for (int faceId : toRemove) {
            if (!faceAlive[static_cast<std::size_t>(faceId)]) continue;
            Face& face = mesh.faces[static_cast<std::size_t>(faceId)];
            for (const Corner& corner : face.corners) {
                vertexFaces[static_cast<std::size_t>(corner.position)].erase(faceId);
            }
            removeFace(faceAlive, faceId, aliveCount);
        }
        vertexFaces[static_cast<std::size_t>(b)].clear();
        vertexAlive[static_cast<std::size_t>(b)] = false;
        ++vertexVersion[static_cast<std::size_t>(a)];
        ++vertexVersion[static_cast<std::size_t>(b)];
        const std::vector<int> neighbors(vertexFaces[static_cast<std::size_t>(a)].begin(),
                                         vertexFaces[static_cast<std::size_t>(a)].end());
        std::set<int> neighborVertices;
        for (int faceId : neighbors) {
            if (!faceAlive[static_cast<std::size_t>(faceId)]) continue;
            for (const Corner& corner : mesh.faces[static_cast<std::size_t>(faceId)].corners) {
                if (corner.position != a) neighborVertices.insert(corner.position);
            }
        }
        for (int neighbor : neighborVertices) enqueueEdge(a, neighbor);
    }

    std::vector<Face> kept;
    kept.reserve(static_cast<std::size_t>(aliveCount[0]));
    for (std::size_t fi = 0; fi < mesh.faces.size(); ++fi) {
        if (faceAlive[fi]) kept.push_back(std::move(mesh.faces[fi]));
    }
    mesh.faces = std::move(kept);
    std::vector<int> compactIndex(mesh.positions.size(), -1);
    std::vector<Vec3> compactPositions;
    compactPositions.reserve(mesh.positions.size());
    for (const Face& face : mesh.faces) {
        for (const Corner& corner : face.corners) {
            const std::size_t oldIndex = static_cast<std::size_t>(corner.position);
            if (compactIndex[oldIndex] < 0) {
                compactIndex[oldIndex] = static_cast<int>(compactPositions.size());
                compactPositions.push_back(mesh.positions[oldIndex]);
            }
        }
    }
    for (Face& face : mesh.faces) {
        for (Corner& corner : face.corners) {
            corner.position = compactIndex[static_cast<std::size_t>(corner.position)];
        }
    }
    mesh.positions = std::move(compactPositions);
    recomputeNormals(mesh);
    outcome.afterMetrics = computeMetrics(mesh);
    outcome.affectedFaceCount = movedFaces + (originalFaces - mesh.faces.size());
    std::ostringstream detail;
    detail.imbue(std::locale::classic());
    detail << "faceCollapse targetRatio=" << formatDouble(options.targetRatio)
           << " targetFaces=" << targetFaces << " resultFaces=" << mesh.faces.size()
           << " moves=" << movedFaces;
    outcome.detail = detail.str();
    return Result<EditOutcome>(std::move(outcome));
}

void MeshDocument::load(Mesh mesh) {
    current = std::move(mesh);
    baseline = current;
    history.clear();
}

void MeshDocument::commit(const EditOutcome& outcome) {
    EditRecord record;
    record.operation = outcome.operation;
    record.detail = outcome.detail;
    record.before = current;
    record.after = outcome.mesh;
    record.beforeMetrics = computeMetrics(record.before);
    record.afterMetrics = computeMetrics(record.after);
    current = outcome.mesh;
    history.push(std::move(record));
}

bool MeshDocument::undo() {
    return history.undo(current);
}

bool MeshDocument::redo() {
    return history.redo(current);
}

std::string editRecordJson(const EditRecord& record) {
    std::ostringstream out;
    out.imbue(std::locale::classic());
    out << "{\"operation\":\"" << record.operation << "\",\"detail\":\""
        << record.detail << "\",\"before\":" << metricsJson(record.beforeMetrics)
        << ",\"after\":" << metricsJson(record.afterMetrics) << '}';
    return out.str();
}

} // namespace meshedit
