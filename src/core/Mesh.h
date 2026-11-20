#pragma once

#include "core/Geometry.h"

#include <array>
#include <map>
#include <string>
#include <vector>

namespace meshedit {

struct Corner {
    int position = -1;
    int texcoord = -1;
    int normal = -1;
};

struct Face {
    std::array<Corner, 3> corners;
    std::string group = "default";
    std::string material;
    int sourceLine = 0;
};

struct Mesh {
    std::string name = "mesh";
    std::vector<Vec3> positions;
    std::vector<Vec2> texcoords;
    std::vector<Vec3> normals;
    std::vector<Face> faces;
    std::vector<std::string> groups{"default"};
    std::vector<std::string> materialLibraries;
    int sourceVersion = 2;
    bool welded = false;

    bool empty() const { return faces.empty(); }
};

struct MeshMetrics {
    std::size_t vertexCount = 0;
    std::size_t faceCount = 0;
    std::size_t triangleCornerCount = 0;
    Aabb bounds;
    double volume = 0.0;
    double surfaceArea = 0.0;
};

MeshMetrics computeMetrics(const Mesh& mesh);
bool metricsEqual(const MeshMetrics& a, const MeshMetrics& b, double epsilon = 0.0);
std::string metricsJson(const MeshMetrics& metrics, int indent = 0);
void recomputeNormals(Mesh& mesh);
void validateMeshIndices(const Mesh& mesh, const char* context);

std::string trim(const std::string& text);
std::vector<std::string> splitWhitespace(const std::string& text);
bool parseInteger(const std::string& token, int& value);
bool parseDouble(const std::string& token, double& value);
bool parseVec2(const std::string& a, const std::string& b, Vec2& value);
bool parseVec3(const std::string& a, const std::string& b, const std::string& c, Vec3& value);

} // namespace meshedit
