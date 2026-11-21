#include "internal.hpp"

#include <cmath>

namespace meshrepair {

namespace {

uint64_t splitMix64(uint64_t& state) {
    uint64_t z = (state += 0x9e3779b97f4a7c15ull);
    z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9ull;
    z = (z ^ (z >> 27)) * 0x94d049bb133111ebull;
    return z ^ (z >> 31);
}

double jitter(uint64_t& state, double amplitude) {
    const uint64_t value = splitMix64(state);
    const double unit = static_cast<double>(value >> 11) /
                        static_cast<double>(1ull << 53);
    return (unit * 2.0 - 1.0) * amplitude;
}

}  // namespace

Result<Mesh> generateSample(uint64_t seed) {
    Mesh mesh;
    mesh.source_coord_type = 0;
    mesh.source_index_width = 4;
    mesh.vertices = {
        {0.0, 0.0, 0.0},       // 0
        {2.0, 0.0, 0.0},       // 1
        {2.0, 2.0, 0.0},       // 2
        {0.0, 2.0, 0.0},       // 3
        {0.0, 0.0, 2.0},       // 4
        {2.0, 0.0, 2.0},       // 5
        {2.0, 2.0, 2.0},       // 6
        {0.0, 2.0, 2.0},       // 7
        {0.2, 0.2, 0.2},       // 8: tetra base
        {0.8, 0.2, 0.2},       // 9
        {0.2, 0.8, 0.2},       // 10
        {0.2, 0.2, 0.8},       // 11
        {0.2, 0.6, 0.3},       // 12: non-manifold fin apex
        {0.2, 0.2, 0.8},       // 13: duplicate of 11, welded in-chunk
        {10.0, 10.0, 10.0},    // 14: debris
        {11.0, 10.0, 10.0},    // 15
        {10.0, 11.0, 10.0}};   // 16

    uint64_t state = seed ^ 0x4d45534852455044ull;
    for (size_t index = 8; index < mesh.vertices.size(); ++index) {
        Vec3& p = mesh.vertices[index];
        p.x += jitter(state, 1e-5);
        p.y += jitter(state, 1e-5);
        p.z += jitter(state, 1e-5);
    }
    // Keep the deliberately duplicated vertex exactly coincident.
    mesh.vertices[13] = mesh.vertices[11];

    mesh.faces = {
        // Bottom.
        {{0, 2, 1}},
        {{0, 3, 2}},
        // Top is intentionally absent: the square loop 4-5-6-7 is a
        // cross-chunk hole.
        // Front.
        {{0, 1, 5}},
        {{0, 5, 4}},
        // Right.
        {{1, 2, 6}},
        {{1, 6, 5}},
        // Back.
        {{2, 3, 7}},
        {{2, 7, 6}},
        // Left.
        {{3, 0, 4}},
        {{3, 4, 7}},
        // Closed tetrahedron (11 and 13 weld to one vertex) plus a fin
        // attached to edge 8-9.
        {{8, 10, 9}},
        {{8, 9, 11}},
        {{9, 10, 13}},
        {{10, 8, 13}},
        {{8, 9, 12}},
        // Isolated debris triangle.
        {{14, 15, 16}}};
    return Result<Mesh>::success(std::move(mesh));
}

Status writeSample(const std::string& path, uint64_t seed) {
    auto mesh = generateSample(seed);
    if (!mesh) return Status(mesh.error);
    return writeMeshAtomic(*mesh, path, 0, 4);
}

}  // namespace meshrepair
