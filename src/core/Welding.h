#pragma once

#include "core/Error.h"
#include "core/Mesh.h"
#include "core/Topology.h"

#include <string>
#include <vector>

namespace meshedit {

struct WeldOptions {
    double tolerance = 1e-6;
    bool recomputeNormals = true;
};

struct WeldReport {
    std::size_t oldVertexCount = 0;
    std::size_t newVertexCount = 0;
    std::size_t mergedVertexCount = 0;
    std::size_t affectedFaceCount = 0;
    std::size_t removedDegenerateFaces = 0;
    std::vector<int> vertexMergeMap;
    std::size_t boundaryEdgeCount = 0;
    std::size_t boundaryLoopCount = 0;
    std::size_t nonManifoldEdgeCount = 0;

    std::string toJson() const;
};

Result<WeldReport> weldMesh(Mesh& mesh, const WeldOptions& options);

} // namespace meshedit
