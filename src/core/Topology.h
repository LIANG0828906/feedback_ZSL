#pragma once

#include "core/Mesh.h"

#include <cstddef>
#include <map>
#include <string>
#include <vector>

namespace meshedit {

struct EdgeKey {
    int a = 0;
    int b = 0;

    EdgeKey() = default;
    EdgeKey(int first, int second)
        : a(std::min(first, second)), b(std::max(first, second)) {}

    bool operator<(const EdgeKey& other) const {
        return a < other.a || (a == other.a && b < other.b);
    }
    bool operator==(const EdgeKey& other) const { return a == other.a && b == other.b; }
};

struct EdgeInfo {
    EdgeKey key;
    std::vector<int> faces;
};

struct Topology {
    std::vector<std::vector<int>> vertexFaces;
    std::vector<std::vector<int>> adjacency;
    std::map<EdgeKey, EdgeInfo> edges;
    std::vector<EdgeKey> boundaryEdges;
    std::vector<EdgeKey> nonManifoldEdges;
    std::vector<std::vector<int>> boundaryLoops;
    std::size_t isolatedVertices = 0;

    std::size_t boundaryLoopCount() const { return boundaryLoops.size(); }
};

Topology buildTopology(const Mesh& mesh);
std::string topologySummaryJson(const Topology& topology);

} // namespace meshedit
