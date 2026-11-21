#pragma once

#include "meshrepair/api.hpp"

#include <array>
#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <optional>
#include <string>
#include <utility>
#include <vector>

namespace meshrepair::detail {

constexpr uint64_t kGeneratedSourceFace = UINT64_MAX;

class Writer {
public:
    void u8(uint8_t value) { bytes_.push_back(value); }
    void u16(uint16_t value);
    void u32(uint32_t value);
    void u64(uint64_t value);
    void i64(int64_t value) { u64(static_cast<uint64_t>(value)); }
    void f64(double value);
    void raw(const void* data, size_t size);
    void string(const std::string& value);

    const std::vector<uint8_t>& bytes() const { return bytes_; }
    std::vector<uint8_t>& bytes() { return bytes_; }
    std::vector<uint8_t> take() { return std::move(bytes_); }

private:
    std::vector<uint8_t> bytes_;
};

class Reader {
public:
    Reader(const uint8_t* data, size_t size) : data_(data), size_(size) {}

    bool u8(uint8_t& value);
    bool u16(uint16_t& value);
    bool u32(uint32_t& value);
    bool u64(uint64_t& value);
    bool i64(int64_t& value);
    bool f64(double& value);
    bool raw(void* out, size_t size);
    bool string(std::string& value);

    bool ok() const { return ok_; }
    bool atEnd() const { return pos_ == size_; }
    size_t remaining() const { return size_ - pos_; }
    size_t position() const { return pos_; }

private:
    bool need(size_t count);

    const uint8_t* data_ = nullptr;
    size_t size_ = 0;
    size_t pos_ = 0;
    bool ok_ = true;
};

Error makeError(
    ErrorCode code,
    std::string message,
    std::optional<uint64_t> chunk = {},
    std::optional<uint64_t> face = {},
    std::optional<uint64_t> vertex = {});

Result<std::vector<uint8_t>> readFileBytes(const std::string& path);
Status atomicWriteFile(const std::string& path, const std::vector<uint8_t>& bytes);
Status ensureDirectory(const std::string& path);
Status removeStaleTemps(const std::string& directory);
std::string parentPath(const std::string& path);
std::string joinPath(const std::string& a, const std::string& b);

class SessionLock {
public:
    SessionLock() = default;
    ~SessionLock();
    SessionLock(const SessionLock&) = delete;
    SessionLock& operator=(const SessionLock&) = delete;

    Status acquire(const std::string& path);
    void release();

private:
    int fd_ = -1;
    std::string path_;
    void* native_handle_ = nullptr;
};

struct ChunkCoord {
    int64_t x = 0;
    int64_t y = 0;
    int64_t z = 0;

    bool operator<(const ChunkCoord& other) const {
        if (x != other.x) return x < other.x;
        if (y != other.y) return y < other.y;
        return z < other.z;
    }
    bool operator==(const ChunkCoord& other) const {
        return x == other.x && y == other.y && z == other.z;
    }
};

struct ChunkDef {
    uint32_t id = 0;
    ChunkCoord coord;
    uint64_t estimated_bytes = 0;
    std::vector<uint64_t> faces;
    std::vector<uint64_t> vertices;
};

struct StraddleDef {
    uint64_t face = 0;
    uint32_t owner_chunk = 0;
    std::vector<ChunkCoord> touched;
};

struct BoundaryDef {
    uint32_t chunk_a = 0;
    uint32_t chunk_b = 0;
    std::vector<uint64_t> vertices;
    std::vector<EdgeKey> edges;
    std::string hash;
};

struct Manifest {
    std::string input_hash;
    std::string config_hash;
    std::string table_hash;
    std::string boundary_hash;
    std::string plan_hash;
    double chunk_size_x = 1.0;
    double chunk_size_y = 1.0;
    double chunk_size_z = 1.0;
    std::vector<ChunkDef> chunks;
    std::vector<StraddleDef> straddles;
    std::vector<BoundaryDef> boundaries;
};

Result<Manifest> buildManifest(
    const Mesh& mesh,
    const Config& config,
    const Topology& topology,
    const std::string& input_hash,
    const std::string& config_hash);

Result<Manifest> parseManifestBytes(const std::vector<uint8_t>& bytes);
std::vector<uint8_t> serializeManifest(const Manifest& manifest);
std::string localBoundaryHash(const Manifest& manifest, uint32_t chunk_id);
std::vector<uint32_t> chunkIdsForFaceOwners(
    const Manifest& manifest,
    const std::vector<uint64_t>& face_owner_chunks);

struct ChunkFaceRecord {
    uint64_t source_face = kGeneratedSourceFace;
    std::array<uint64_t, 3> v{};
    uint32_t owner_chunk = 0;
    std::string hole_id;
    bool fill = false;
};

struct ChunkVertexRecord {
    uint64_t id = 0;
    Vec3 position;
};

struct ChunkBundle {
    uint32_t id = 0;
    std::string input_hash;
    std::string table_hash;
    std::string boundary_hash;
    std::string local_boundary_hash;
    std::string prev_chain_hash;
    std::string hash;
    std::vector<ChunkVertexRecord> vertices;
    std::vector<ChunkFaceRecord> faces;
    uint64_t filled_faces = 0;
    uint64_t split_groups = 0;
};

struct RepairPlanData {
    std::vector<ChunkVertexRecord> global_vertices;
    std::vector<ChunkFaceRecord> faces;
    std::vector<HoleSummary> holes;
    uint64_t vertices_after_weld = 0;
    uint64_t welded_vertex_removals = 0;
    uint64_t filled_faces = 0;
    uint64_t split_edges = 0;
    uint64_t split_groups = 0;
    uint64_t split_vertex_copies = 0;
    uint64_t debris_faces_removed = 0;
    std::vector<SplitOwnership> split_ownership;
    std::vector<uint32_t> committed_chunks;
};

Result<RepairPlanData> computeRepairPlan(
    const Mesh& mesh,
    const Config& config,
    const Manifest& manifest,
    const Topology& original_topology);

Result<ChunkBundle> makeChunkBundle(
    const RepairPlanData& plan,
    const Manifest& manifest,
    uint32_t chunk_id,
    const std::string& prev_chain_hash);

std::vector<uint8_t> serializeChunkBundle(const ChunkBundle& bundle);
Result<ChunkBundle> parseChunkBundleBytes(const std::vector<uint8_t>& bytes);
Result<ChunkBundle> readChunkBundle(const std::string& path);
Status writeChunkBundle(const std::string& path, const ChunkBundle& bundle);

struct StitchResult {
    Mesh mesh;
    std::vector<uint32_t> face_chunk;
    std::string hash;
    std::optional<Error> error;
    uint64_t duplicate_fill_faces = 0;
    uint64_t invalid_boundary_edges = 0;
};

Result<StitchResult> stitchChunks(
    const Mesh& original,
    const Manifest& manifest,
    const std::vector<ChunkBundle>& bundles);

std::string configHash(const Config& config);
std::string planPath(const std::string& output_dir);
std::string chunkPath(const std::string& output_dir, uint32_t chunk_id);
std::string chunkFileName(uint32_t chunk_id);
std::string tracePath(const std::string& output_dir);
std::string finalMeshPath(const std::string& output_dir);
std::string reportPath(const std::string& output_dir);
std::string lockPathForInput(const Config& config, const std::string& input_hash);

std::string chunkSummaryJson(const ChunkSummary& chunk);
std::string holeSummaryJson(const HoleSummary& hole);

}  // namespace meshrepair::detail
