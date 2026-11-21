#pragma once

#include <array>
#include <cstdint>
#include <optional>
#include <string>
#include <vector>

namespace chunkpipe {

constexpr std::uint32_t kNoChunk = 0xffffffffu;

struct Vec3 {
    float x = 0.0f;
    float y = 0.0f;
    float z = 0.0f;
};

Vec3 operator+(const Vec3& a, const Vec3& b);
Vec3 operator-(const Vec3& a, const Vec3& b);
Vec3 operator*(const Vec3& a, float scalar);
float dot(const Vec3& a, const Vec3& b);
Vec3 cross(const Vec3& a, const Vec3& b);
float length(const Vec3& value);
Vec3 normalize(const Vec3& value);
Vec3 minVec(const Vec3& a, const Vec3& b);
Vec3 maxVec(const Vec3& a, const Vec3& b);

struct Aabb {
    Vec3 min;
    Vec3 max;

    Vec3 center() const;
    Vec3 extent() const;
    bool finiteAndOrdered() const;
    bool contains(const Vec3& point, float tolerance) const;
    std::array<Vec3, 8> corners() const;
};

struct GridDivision {
    std::uint32_t x = 0;
    std::uint32_t y = 0;
    std::uint32_t z = 0;
    float cellSize = 0.0f;
};

struct CompressionDesc {
    std::uint32_t codec = 0;
    std::uint64_t rawBytes = 0;
    std::uint64_t storedBytes = 0;
    std::uint32_t flags = 0;
};

struct HierarchyTable {
    std::uint32_t parent = kNoChunk;
    std::vector<std::uint32_t> children;
};

enum class PrioritySource : std::uint32_t {
    AuthoredV3 = 0,
    DerivedFromBounds = 1
};

const char* prioritySourceName(PrioritySource source);

struct Chunk {
    std::uint32_t id = 0;
    Aabb bounds;
    std::vector<Vec3> vertices;
    std::vector<std::uint32_t> indices;

    std::optional<GridDivision> grid;
    std::optional<bool> occluder;
    std::optional<CompressionDesc> compression;
    std::uint32_t priority = 0;
    PrioritySource prioritySource = PrioritySource::AuthoredV3;
    std::optional<HierarchyTable> hierarchy;
};

struct SceneArchive {
    std::uint16_t version = 0;
    std::uint32_t sceneId = 0;
    float boundaryTolerance = 0.0f;
    std::string sourcePath;
    std::vector<Chunk> chunks;
};

} // namespace chunkpipe

