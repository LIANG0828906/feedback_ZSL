#include "core/Topology.h"

#include <algorithm>
#include <locale>
#include <optional>
#include <set>
#include <sstream>

namespace meshedit {

Topology buildTopology(const Mesh& mesh) {
    Topology topology;
    const std::size_t vertexCount = mesh.positions.size();
    topology.vertexFaces.assign(vertexCount, {});
    topology.adjacency.assign(vertexCount, {});

    for (std::size_t fi = 0; fi < mesh.faces.size(); ++fi) {
        const Face& face = mesh.faces[fi];
        int unique[3] = {
            face.corners[0].position,
            face.corners[1].position,
            face.corners[2].position};
        std::sort(unique, unique + 3);
        int uniqueCount = 0;
        for (int i = 0; i < 3; ++i) {
            if (i == 0 || unique[i] != unique[i - 1]) unique[uniqueCount++] = unique[i];
        }
        for (int i = 0; i < uniqueCount; ++i) {
            const int vertex = unique[i];
            if (vertex < 0 || static_cast<std::size_t>(vertex) >= vertexCount) continue;
            topology.vertexFaces[static_cast<std::size_t>(vertex)].push_back(
                static_cast<int>(fi));
        }
        for (int i = 0; i < uniqueCount; ++i) {
            for (int j = i + 1; j < uniqueCount; ++j) {
                const int a = unique[i];
                const int b = unique[j];
                if (a == b || a < 0 || b < 0 ||
                    static_cast<std::size_t>(a) >= vertexCount ||
                    static_cast<std::size_t>(b) >= vertexCount) {
                    continue;
                }
                const EdgeKey key(a, b);
                EdgeInfo& info = topology.edges[key];
                info.key = key;
                if (info.faces.empty() || info.faces.back() != static_cast<int>(fi)) {
                    info.faces.push_back(static_cast<int>(fi));
                }
            }
        }
    }

    for (auto& item : topology.edges) {
        const EdgeKey& key = item.first;
        const EdgeInfo& info = item.second;
        std::vector<int>& na = topology.adjacency[static_cast<std::size_t>(key.a)];
        std::vector<int>& nb = topology.adjacency[static_cast<std::size_t>(key.b)];
        if (std::find(na.begin(), na.end(), key.b) == na.end()) na.push_back(key.b);
        if (std::find(nb.begin(), nb.end(), key.a) == nb.end()) nb.push_back(key.a);
        if (info.faces.size() == 1) topology.boundaryEdges.push_back(key);
        else if (info.faces.size() > 2) topology.nonManifoldEdges.push_back(key);
    }
    for (auto& neighbors : topology.adjacency) std::sort(neighbors.begin(), neighbors.end());
    for (auto& faces : topology.vertexFaces) std::sort(faces.begin(), faces.end());
    topology.isolatedVertices = static_cast<std::size_t>(
        std::count_if(topology.adjacency.begin(), topology.adjacency.end(),
                      [](const std::vector<int>& neighbors) { return neighbors.empty(); }));

    std::set<EdgeKey> unvisited(topology.boundaryEdges.begin(), topology.boundaryEdges.end());
    auto popSmallestAt = [&](int vertex, int previous) -> std::optional<EdgeKey> {
        std::optional<EdgeKey> result;
        for (auto it = unvisited.begin(); it != unvisited.end(); ++it) {
            if (it->a != vertex && it->b != vertex) continue;
            const int other = it->a == vertex ? it->b : it->a;
            if (other == previous) continue;
            if (!result) result = *it;
        }
        return result;
    };

    while (!unvisited.empty()) {
        const EdgeKey first = *unvisited.begin();
        unvisited.erase(unvisited.begin());
        std::vector<int> loop{first.a, first.b};
        int previous = first.a;
        int current = first.b;
        const int start = first.a;
        while (current != start) {
            std::optional<EdgeKey> next = popSmallestAt(current, previous);
            if (!next) break;
            unvisited.erase(*next);
            previous = current;
            current = next->a == current ? next->b : next->a;
            if (std::find(loop.begin(), loop.end(), current) != loop.end()) {
                break;
            }
            loop.push_back(current);
        }
        topology.boundaryLoops.push_back(std::move(loop));
    }
    return topology;
}

std::string topologySummaryJson(const Topology& topology) {
    std::ostringstream out;
    out.imbue(std::locale::classic());
    out << "{\"edges\":" << topology.edges.size()
        << ",\"boundaryEdges\":" << topology.boundaryEdges.size()
        << ",\"boundaryLoops\":" << topology.boundaryLoops.size()
        << ",\"nonManifoldEdges\":" << topology.nonManifoldEdges.size()
        << ",\"isolatedVertices\":" << topology.isolatedVertices << '}';
    return out.str();
}

} // namespace meshedit
