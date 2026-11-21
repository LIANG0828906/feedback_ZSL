#pragma once

#include <array>
#include <atomic>
#include <cstdint>
#include <functional>
#include <map>
#include <memory>
#include <optional>
#include <string>
#include <utility>
#include <vector>

namespace meshrepair {

enum class ErrorCode {
    None = 0,
    MeshInvalid,
    ConfigInvalid,
    BoundaryPending,
    TopologyConflict,
    BudgetExceeded,
    RecoveryInvalid,
    Busy,
    IoError,
    Cancelled,
    Internal
};

const char* errorCodeName(ErrorCode code);

struct Error {
    ErrorCode code = ErrorCode::None;
    std::string message;
    std::optional<uint64_t> chunk;
    std::optional<uint64_t> face;
    std::optional<uint64_t> vertex;
    std::optional<uint64_t> edge_a;
    std::optional<uint64_t> edge_b;
    std::optional<std::string> hole_id;
    std::optional<uint64_t> estimated_bytes;

    bool ok() const { return code == ErrorCode::None; }
};

struct Status {
    Error error;

    Status() = default;
    explicit Status(Error e) : error(std::move(e)) {}

    bool ok() const { return error.ok(); }
    explicit operator bool() const { return ok(); }

    static Status success() { return Status(); }
    static Status failure(ErrorCode code, std::string message) {
        Error e;
        e.code = code;
        e.message = std::move(message);
        return Status(std::move(e));
    }
};

template <typename T>
struct Result {
    std::optional<T> value;
    Error error;

    bool ok() const { return error.ok() && value.has_value(); }
    explicit operator bool() const { return ok(); }
    T& operator*() { return *value; }
    const T& operator*() const { return *value; }
    T* operator->() { return &*value; }
    const T* operator->() const { return &*value; }

    static Result success(T v) {
        Result r;
        r.value = std::move(v);
        return r;
    }
    static Result failure(Error e) {
        Result r;
        r.error = std::move(e);
        return r;
    }
};

struct Vec3 {
    double x = 0.0;
    double y = 0.0;
    double z = 0.0;

    bool operator==(const Vec3& other) const {
        return x == other.x && y == other.y && z == other.z;
    }
};

struct Face {
    std::array<uint64_t, 3> v{};

    bool operator==(const Face& other) const { return v == other.v; }
};

struct Mesh {
    std::vector<Vec3> vertices;
    std::vector<Face> faces;
    uint16_t source_coord_type = 1;
    uint16_t source_index_width = 8;
};

struct Config {
    uint32_t parallelism = 4;
    uint64_t memory_budget_bytes = 256ull * 1024ull * 1024ull;
    uint64_t block_memory_bytes = 64ull * 1024ull * 1024ull;
    double chunk_size_x = 1.0;
    double chunk_size_y = 1.0;
    double chunk_size_z = 1.0;
    double weld_tolerance = 1e-6;
    uint32_t min_component_faces = 3;
    double normal_tolerance_deg = 5.0;
    uint64_t temp_reserve_bytes = 64ull * 1024ull * 1024ull;
    std::string temp_dir;
};

struct EdgeKey {
    uint64_t a = 0;
    uint64_t b = 0;

    static EdgeKey make(uint64_t u, uint64_t v) {
        return u < v ? EdgeKey{u, v} : EdgeKey{v, u};
    }
    bool operator<(const EdgeKey& other) const {
        return a < other.a || (a == other.a && b < other.b);
    }
    bool operator==(const EdgeKey& other) const { return a == other.a && b == other.b; }
};

struct DirectedEdge {
    uint64_t from = 0;
    uint64_t to = 0;
    uint64_t face = 0;
    uint8_t corner = 0;
};

struct Topology {
    std::vector<std::vector<uint64_t>> vertex_faces;
    std::vector<std::vector<uint64_t>> vertex_neighbors;
    std::vector<DirectedEdge> directed_edges;
    std::map<EdgeKey, std::vector<uint64_t>> edge_faces;
    std::vector<uint64_t> boundary_edges;
    std::vector<uint64_t> nonmanifold_edges;
};

struct ChunkSummary {
    uint32_t id = 0;
    int64_t cell_x = 0;
    int64_t cell_y = 0;
    int64_t cell_z = 0;
    uint64_t estimated_bytes = 0;
    uint64_t vertex_count = 0;
    uint64_t face_count = 0;
    std::string boundary_hash;
    std::string bundle_hash;
    bool committed = false;
};

struct HoleSummary {
    std::string id;
    uint32_t owner_chunk = 0;
    std::vector<uint32_t> involved_chunks;
    uint64_t vertex_count = 0;
    uint64_t filled_faces = 0;
    Vec3 bbox_min;
    Vec3 bbox_max;
    bool cross_chunk = false;
    std::string status;
};

struct NormalDeviation {
    double mean_deg = 0.0;
    double max_deg = 0.0;
    double tolerance_deg = 0.0;
    bool exceeded = false;
    std::optional<uint64_t> face;
    std::optional<uint64_t> chunk;
};

struct SplitOwnership {
    uint64_t edge_a = 0;
    uint64_t edge_b = 0;
    uint32_t owner_chunk = 0;
    uint32_t group = 0;
    uint64_t face_count = 0;
};

struct RepairReport {
    std::string input_hash;
    std::string table_hash;
    std::string boundary_hash;
    std::string plan_hash;
    std::string stitch_hash;
    std::string output_path;
    uint64_t original_vertices = 0;
    uint64_t original_faces = 0;
    uint64_t vertices_after_weld = 0;
    uint64_t final_vertices = 0;
    uint64_t final_faces = 0;
    uint64_t welded_vertex_removals = 0;
    uint64_t filled_faces = 0;
    uint64_t split_edges = 0;
    uint64_t split_groups = 0;
    uint64_t split_vertex_copies = 0;
    uint64_t debris_faces_removed = 0;
    bool stitch_valid = false;
    bool resumed = false;
    bool cancelled = false;
    std::optional<Error> validation_error;
    NormalDeviation normals;
    std::vector<ChunkSummary> chunks;
    std::vector<HoleSummary> holes;
    std::vector<SplitOwnership> split_ownership;
};

struct Snapshot {
    Mesh mesh;
    std::vector<uint32_t> face_chunk;
    std::vector<uint32_t> committed_chunks;
    std::string hash;
    uint64_t committed_count = 0;
};

struct RepairOptions {
    uint32_t stop_after_commits = UINT32_MAX;
    uint32_t hold_lock_ms = 0;
    bool verify_only = false;
    std::atomic<bool>* cancel = nullptr;
    std::function<void(std::shared_ptr<const Snapshot>)> on_snapshot;
};

// Binary mesh format and topology.
Result<Mesh> parseMesh(const std::vector<uint8_t>& bytes, const std::string& source_name);
Result<Mesh> loadMesh(const std::string& path);
Result<std::vector<uint8_t>> serializeMesh(
    const Mesh& mesh,
    uint16_t coord_type = 1,
    uint16_t index_width = 8);
Status writeMeshAtomic(
    const Mesh& mesh,
    const std::string& path,
    uint16_t coord_type = 1,
    uint16_t index_width = 8);
Result<Topology> buildTopology(const Mesh& mesh);

// Configuration is strict: all required fields must be present exactly once.
Result<Config> parseConfig(const std::string& text);
Result<Config> loadConfig(const std::string& path);
std::string defaultConfigText();

// Deterministic local sample: cross-chunk hole, duplicate vertex, non-manifold
// edge, and a removable debris component.
Result<Mesh> generateSample(uint64_t seed);
Status writeSample(const std::string& path, uint64_t seed);
Status writeDefaultConfig(const std::string& path);

// Hash a byte range with SHA-256 (lowercase hexadecimal).
std::string sha256Hex(const uint8_t* data, size_t size);
std::string sha256Hex(const std::vector<uint8_t>& data);
std::string sha256Hex(const std::string& data);

// Full engine. Disk state under output_dir is authoritative after interruption.
Result<RepairReport> repairMesh(
    const Mesh& mesh,
    const Config& config,
    const std::string& output_dir,
    const RepairOptions& options = {});
Result<RepairReport> repairFile(
    const std::string& input_path,
    const std::string& config_path,
    const std::string& output_dir,
    const RepairOptions& options = {});

// Re-open committed state, verify it, and rebuild/verify the stitched result.
Result<RepairReport> verifyCommitted(
    const Mesh& mesh,
    const Config& config,
    const std::string& output_dir);

// Probe the committed-pair gate for every extracted hole without modifying disk.
// An uncommitted cross-chunk pair returns E_BOUNDARY_PENDING and carries the
// first affected hole id and owner chunk.
Result<std::vector<HoleSummary>> evaluateHoleBoundaryGates(
    const Mesh& mesh,
    const Config& config,
    const std::vector<std::pair<uint32_t, uint32_t>>& committed_pairs);

std::string reportToJson(const RepairReport& report);
std::string errorToJson(const Error& error);

}  // namespace meshrepair
