#include "internal.hpp"

#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdlib>
#include <cstring>
#include <future>
#include <iomanip>
#include <map>
#include <numeric>
#include <set>
#include <sstream>
#include <thread>

#ifndef _WIN32
#include <sys/statvfs.h>
#include <unistd.h>
#endif

namespace meshrepair {

namespace {

using namespace detail;

constexpr double kPi = 3.141592653589793238462643383279502884;

double angleBetween(const Vec3& a, const Vec3& b) {
    const double ax = a.x, ay = a.y, az = a.z;
    const double bx = b.x, by = b.y, bz = b.z;
    const double dot = ax * bx + ay * by + az * bz;
    const double la = std::sqrt(ax * ax + ay * ay + az * az);
    const double lb = std::sqrt(bx * bx + by * by + bz * bz);
    if (la == 0.0 || lb == 0.0) return 0.0;
    double cosine = dot / (la * lb);
    cosine = std::max(-1.0, std::min(1.0, cosine));
    return std::acos(cosine) * 180.0 / kPi;
}

Vec3 faceNormal(const Vec3& a, const Vec3& b, const Vec3& c) {
    Vec3 ab{b.x - a.x, b.y - a.y, b.z - a.z};
    Vec3 ac{c.x - a.x, c.y - a.y, c.z - a.z};
    return Vec3{
        ab.y * ac.z - ab.z * ac.y,
        ab.z * ac.x - ab.x * ac.z,
        ab.x * ac.y - ab.y * ac.x};
}

std::string chunkName(uint32_t id) {
    std::ostringstream stream;
    stream << "chunk_" << std::setw(6) << std::setfill('0') << id;
    return stream.str();
}

Status validateConfig(const Config& config) {
    if (config.parallelism == 0 || config.parallelism > 4096) {
        return Status::failure(
            ErrorCode::ConfigInvalid, "parallelism must be in [1, 4096]");
    }
    if (config.memory_budget_bytes == 0) {
        return Status::failure(
            ErrorCode::ConfigInvalid, "memory_budget_bytes must be positive");
    }
    if (config.block_memory_bytes == 0 ||
        config.block_memory_bytes > config.memory_budget_bytes) {
        return Status::failure(
            ErrorCode::ConfigInvalid,
            "block_memory_bytes must be positive and no larger than the global budget");
    }
    for (double value :
         {config.chunk_size_x, config.chunk_size_y, config.chunk_size_z}) {
        if (!std::isfinite(value) || value <= 0.0) {
            return Status::failure(
                ErrorCode::ConfigInvalid, "chunk sizes must be finite and positive");
        }
    }
    if (!std::isfinite(config.weld_tolerance) || config.weld_tolerance < 0.0) {
        return Status::failure(
            ErrorCode::ConfigInvalid,
            "weld_tolerance must be finite and non-negative");
    }
    if (config.min_component_faces == 0) {
        return Status::failure(
            ErrorCode::ConfigInvalid, "min_component_faces must be positive");
    }
    if (!std::isfinite(config.normal_tolerance_deg) ||
        config.normal_tolerance_deg < 0.0 ||
        config.normal_tolerance_deg > 180.0) {
        return Status::failure(
            ErrorCode::ConfigInvalid,
            "normal_tolerance_deg must be in [0, 180]");
    }
    return Status::success();
}

Status checkDiskReserve(const std::string& path, uint64_t reserve) {
    std::error_code ec;
    std::filesystem::space_info info = std::filesystem::space(path, ec);
    if (ec) {
        return Status::failure(
            ErrorCode::IoError, "cannot inspect free space for " + path + ": " + ec.message());
    }
    if (info.available < reserve) {
        return Status::failure(
            ErrorCode::IoError,
            "insufficient free space at " + path + ": available=" +
                std::to_string(info.available) + " reserve=" + std::to_string(reserve));
    }
    return Status::success();
}

Error firstTableMismatch(const Manifest& expected, const Manifest& actual) {
    Error error = makeError(
        ErrorCode::RecoveryInvalid,
        "committed chunk table differs from the deterministic input table");
    const size_t count = std::min(expected.chunks.size(), actual.chunks.size());
    for (size_t i = 0; i < count; ++i) {
        const ChunkDef& a = expected.chunks[i];
        const ChunkDef& b = actual.chunks[i];
        if (a.id != b.id || !(a.coord == b.coord) ||
            a.estimated_bytes != b.estimated_bytes ||
            a.faces != b.faces || a.vertices != b.vertices) {
            error.chunk = a.id;
            return error;
        }
    }
    if (expected.chunks.size() != actual.chunks.size()) {
        error.chunk =
            expected.chunks.size() < actual.chunks.size()
                ? static_cast<uint64_t>(expected.chunks.size())
                : static_cast<uint64_t>(actual.chunks.size());
    } else {
        error.chunk = 0;
    }
    return error;
}

Error firstBoundaryMismatch(const Manifest& expected, const Manifest& actual) {
    Error error = makeError(
        ErrorCode::RecoveryInvalid,
        "committed boundary mapping differs from the deterministic boundary protocol");
    const size_t count =
        std::min(expected.boundaries.size(), actual.boundaries.size());
    for (size_t i = 0; i < count; ++i) {
        const BoundaryDef& a = expected.boundaries[i];
        const BoundaryDef& b = actual.boundaries[i];
        if (a.chunk_a != b.chunk_a || a.chunk_b != b.chunk_b ||
            a.vertices != b.vertices || a.edges != b.edges || a.hash != b.hash) {
            error.chunk = std::min(a.chunk_a, a.chunk_b);
            return error;
        }
    }
    if (expected.boundaries.size() != actual.boundaries.size()) {
        if (expected.boundaries.size() < actual.boundaries.size()) {
            const BoundaryDef& pair = expected.boundaries.size() < actual.boundaries.size()
                                          ? actual.boundaries[expected.boundaries.size()]
                                          : actual.boundaries.back();
            error.chunk = std::min(pair.chunk_a, pair.chunk_b);
        } else {
            const BoundaryDef& pair = expected.boundaries.back();
            error.chunk = std::min(pair.chunk_a, pair.chunk_b);
        }
    } else {
        error.chunk = 0;
    }
    return error;
}

Result<Manifest> loadOrCreateManifest(
    const Manifest& expected,
    const std::string& output_dir,
    bool verify_only) {
    const std::string path = planPath(output_dir);
    std::error_code ec;
    if (!std::filesystem::exists(path, ec)) {
        if (verify_only) {
            return Result<Manifest>::failure(makeError(
                ErrorCode::RecoveryInvalid,
                "committed plan manifest is missing",
                0));
        }
        const auto bytes = serializeManifest(expected);
        Status status = atomicWriteFile(path, bytes);
        if (!status) return Result<Manifest>::failure(status.error);
        return Result<Manifest>::success(expected);
    }
    auto bytes = readFileBytes(path);
    if (!bytes) {
        Error error = bytes.error;
        error.code = ErrorCode::RecoveryInvalid;
        error.chunk = 0;
        return Result<Manifest>::failure(error);
    }
    auto parsed = parseManifestBytes(*bytes);
    if (!parsed) {
        Error error = parsed.error;
        error.code = ErrorCode::RecoveryInvalid;
        error.chunk = 0;
        return Result<Manifest>::failure(error);
    }
    if (parsed->input_hash != expected.input_hash) {
        return Result<Manifest>::failure(makeError(
            ErrorCode::RecoveryInvalid,
            "committed plan belongs to a different input mesh",
            0));
    }
    if (parsed->config_hash != expected.config_hash) {
        return Result<Manifest>::failure(makeError(
            ErrorCode::RecoveryInvalid,
            "committed plan was produced with a different configuration",
            0));
    }
    if (parsed->table_hash != expected.table_hash) {
        return Result<Manifest>::failure(firstTableMismatch(expected, *parsed));
    }
    if (parsed->boundary_hash != expected.boundary_hash) {
        return Result<Manifest>::failure(firstBoundaryMismatch(expected, *parsed));
    }
    if (parsed->chunk_size_x != expected.chunk_size_x ||
        parsed->chunk_size_y != expected.chunk_size_y ||
        parsed->chunk_size_z != expected.chunk_size_z) {
        return Result<Manifest>::failure(makeError(
            ErrorCode::RecoveryInvalid,
            "committed chunk sizes differ from the current configuration",
            0));
    }
    return parsed;
}

Status writeTrace(
    const std::string& output_dir,
    const Manifest& manifest,
    const std::vector<std::optional<ChunkBundle>>& bundles,
    const std::string& stitch_hash,
    bool complete,
    const Config& config) {
    std::ostringstream stream;
    stream << "format=meshrepair-trace-v1\n";
    stream << "input_hash=" << manifest.input_hash << '\n';
    stream << "table_hash=" << manifest.table_hash << '\n';
    stream << "boundary_hash=" << manifest.boundary_hash << '\n';
    stream << "plan_hash=" << manifest.plan_hash << '\n';
    stream << "budget parallelism=" << config.parallelism
           << " global_bytes=" << config.memory_budget_bytes
           << " block_bytes=" << config.block_memory_bytes << '\n';
    uint64_t committed = 0;
    for (const auto& bundle : bundles) {
        if (bundle) ++committed;
    }
    stream << "committed_count=" << committed << '\n';
    stream << "waiting_queue=" << (bundles.size() - committed) << '\n';

    size_t cursor = 0;
    uint64_t batch_id = 0;
    while (cursor < manifest.chunks.size()) {
        std::vector<size_t> batch;
        uint64_t reserved = 0;
        while (cursor < manifest.chunks.size() &&
               batch.size() < config.parallelism) {
            const uint64_t estimate = manifest.chunks[cursor].estimated_bytes;
            if (estimate > config.block_memory_bytes) {
                stream << "sched"
                       << " chunk=" << chunkName(manifest.chunks[cursor].id)
                       << " seq=" << cursor
                       << " estimate=" << estimate
                       << " admission=REJECT"
                       << " reason=BLOCK_BUDGET\n";
                ++cursor;
                continue;
            }
            if (!batch.empty() &&
                reserved > config.memory_budget_bytes - estimate) {
                stream << "sched"
                       << " chunk=" << chunkName(manifest.chunks[cursor].id)
                       << " seq=" << cursor
                       << " estimate=" << estimate
                       << " admission=WAIT_QUEUE"
                       << " reason=GLOBAL_BUDGET\n";
                break;
            }
            if (batch.empty() && estimate > config.memory_budget_bytes) {
                stream << "sched"
                       << " chunk=" << chunkName(manifest.chunks[cursor].id)
                       << " seq=" << cursor
                       << " estimate=" << estimate
                       << " admission=REJECT"
                       << " reason=GLOBAL_BUDGET\n";
                ++cursor;
                continue;
            }
            batch.push_back(cursor);
            reserved += estimate;
            ++cursor;
        }
        for (size_t index : batch) {
            stream << "sched"
                   << " chunk=" << chunkName(manifest.chunks[index].id)
                   << " seq=" << index
                   << " estimate=" << manifest.chunks[index].estimated_bytes
                   << " admission=START"
                   << " batch=" << batch_id
                   << " reserved=" << reserved << '\n';
        }
        if (batch.empty()) break;
        ++batch_id;
    }
    stream << "queue_empty=" << (committed == bundles.size() ? 1 : 0) << '\n';
    for (const ChunkDef& chunk : manifest.chunks) {
        const bool exists =
            chunk.id < bundles.size() && static_cast<bool>(bundles[chunk.id]);
        if (exists) {
            const ChunkBundle& bundle = *bundles[chunk.id];
            stream << "commit"
                   << " chunk=" << chunkName(chunk.id)
                   << " status=COMMITTED"
                   << " boundary_map_hash=" << localBoundaryHash(manifest, chunk.id)
                   << " bundle_hash=" << bundle.hash
                   << " chain_prev=" << bundle.prev_chain_hash
                   << '\n';
        }
    }
    stream << "stitch_hash=" << (stitch_hash.empty() ? "PENDING" : stitch_hash) << '\n';
    stream << "result=" << (complete ? "PASS" : "PENDING") << '\n';
    const std::string text = stream.str();
    return atomicWriteFile(
        tracePath(output_dir),
        std::vector<uint8_t>(text.begin(), text.end()));
}

Result<std::shared_ptr<Snapshot>> buildSnapshot(
    const std::vector<std::optional<ChunkBundle>>& bundles) {
    auto snapshot = std::make_shared<Snapshot>();
    std::map<uint64_t, Vec3> vertices;
    std::map<uint64_t, uint64_t> compact;
    std::vector<ChunkFaceRecord> records;
    for (const auto& optional_bundle : bundles) {
        if (!optional_bundle) continue;
        const ChunkBundle& bundle = *optional_bundle;
        snapshot->committed_chunks.push_back(bundle.id);
        for (const ChunkVertexRecord& vertex : bundle.vertices) {
            auto found = vertices.find(vertex.id);
            if (found == vertices.end()) {
                vertices[vertex.id] = vertex.position;
            } else if (!(found->second == vertex.position)) {
                return Result<std::shared_ptr<Snapshot>>::failure(makeError(
                    ErrorCode::TopologyConflict,
                    "snapshot has conflicting vertex positions",
                    bundle.id,
                    {},
                    vertex.id));
            }
        }
        for (const ChunkFaceRecord& face : bundle.faces) {
            records.push_back(face);
            snapshot->face_chunk.push_back(bundle.id);
        }
    }
    for (const auto& entry : vertices) {
        compact[entry.first] = snapshot->mesh.vertices.size();
        snapshot->mesh.vertices.push_back(entry.second);
    }
    for (const ChunkFaceRecord& record : records) {
        snapshot->mesh.faces.push_back(Face{{compact.at(record.v[0]),
                                             compact.at(record.v[1]),
                                             compact.at(record.v[2])}});
    }
    auto bytes = serializeMesh(snapshot->mesh, 1, 8);
    if (!bytes) {
        return Result<std::shared_ptr<Snapshot>>::failure(bytes.error);
    }
    snapshot->hash = sha256Hex(*bytes);
    snapshot->committed_count = snapshot->committed_chunks.size();
    return Result<std::shared_ptr<Snapshot>>::success(std::move(snapshot));
}

void publishSnapshot(
    const RepairOptions& options,
    const std::vector<std::optional<ChunkBundle>>& bundles) {
    if (!options.on_snapshot) return;
    auto snapshot = buildSnapshot(bundles);
    if (snapshot) {
        try {
            options.on_snapshot(*snapshot);
        } catch (...) {
            // A renderer callback must never corrupt or abort a committed repair.
        }
    }
}

void computeNormalDeviation(
    const Mesh& original,
    const RepairPlanData& plan,
    RepairReport& report) {
    std::map<uint64_t, Vec3> positions;
    for (const ChunkVertexRecord& vertex : plan.global_vertices) {
        positions[vertex.id] = vertex.position;
    }
    double sum = 0.0;
    double maximum = 0.0;
    uint64_t count = 0;
    for (const ChunkFaceRecord& face : plan.faces) {
        if (face.fill || face.source_face >= original.faces.size()) continue;
        const Face& source = original.faces[static_cast<size_t>(face.source_face)];
        const Vec3 original_normal = faceNormal(
            original.vertices[static_cast<size_t>(source.v[0])],
            original.vertices[static_cast<size_t>(source.v[1])],
            original.vertices[static_cast<size_t>(source.v[2])]);
        const Vec3 repaired_normal = faceNormal(
            positions.at(face.v[0]), positions.at(face.v[1]), positions.at(face.v[2]));
        const double deviation = angleBetween(original_normal, repaired_normal);
        sum += deviation;
        ++count;
        if (deviation > maximum) {
            maximum = deviation;
            report.normals.face = face.source_face;
            report.normals.chunk = face.owner_chunk;
        }
    }
    report.normals.mean_deg = count == 0 ? 0.0 : sum / static_cast<double>(count);
    report.normals.max_deg = maximum;
}

std::string jsonString(const std::string& value) {
    std::string result = "\"";
    for (char c : value) {
        switch (c) {
            case '"': result += "\\\""; break;
            case '\\': result += "\\\\"; break;
            case '\n': result += "\\n"; break;
            case '\r': result += "\\r"; break;
            case '\t': result += "\\t"; break;
            default: result += c; break;
        }
    }
    result += '"';
    return result;
}

std::string number(double value) {
    if (!std::isfinite(value)) return "null";
    std::ostringstream stream;
    stream << std::setprecision(17) << value;
    return stream.str();
}

std::string optionalU64(const std::optional<uint64_t>& value) {
    return value ? std::to_string(*value) : std::string("null");
}

std::string chunkTableJson(const Manifest& manifest) {
    std::ostringstream stream;
    stream << "{\"format\":\"meshrepair-chunk-table-v1\","
           << "\"table_version\":1,"
           << "\"input_hash\":" << jsonString(manifest.input_hash) << ","
           << "\"table_hash\":" << jsonString(manifest.table_hash) << ",\n"
           << "\"chunk_size\":[" << number(manifest.chunk_size_x) << ","
           << number(manifest.chunk_size_y) << ","
           << number(manifest.chunk_size_z) << "],\n"
           << "\"chunks\":[";
    for (size_t i = 0; i < manifest.chunks.size(); ++i) {
        const ChunkDef& chunk = manifest.chunks[i];
        if (i) stream << ',';
        stream << "\n{\"id\":" << chunk.id
               << ",\"cell\":[" << chunk.coord.x << "," << chunk.coord.y << ","
               << chunk.coord.z << "]"
               << ",\"estimated_bytes\":" << chunk.estimated_bytes
               << ",\"faces\":[";
        for (size_t j = 0; j < chunk.faces.size(); ++j) {
            if (j) stream << ',';
            stream << chunk.faces[j];
        }
        stream << "],\"vertices\":[";
        for (size_t j = 0; j < chunk.vertices.size(); ++j) {
            if (j) stream << ',';
            stream << chunk.vertices[j];
        }
        stream << "]}";
    }
    stream << "\n],\"straddling_faces\":[";
    for (size_t i = 0; i < manifest.straddles.size(); ++i) {
        const StraddleDef& straddle = manifest.straddles[i];
        if (i) stream << ',';
        stream << "\n{\"face\":" << straddle.face
               << ",\"owner_chunk\":" << straddle.owner_chunk
               << ",\"touched_cells\":[";
        for (size_t j = 0; j < straddle.touched.size(); ++j) {
            if (j) stream << ',';
            stream << '[' << straddle.touched[j].x << ','
                   << straddle.touched[j].y << ','
                   << straddle.touched[j].z << ']';
        }
        stream << "]}";
    }
    stream << "\n]}\n";
    return stream.str();
}

std::string boundaryMapJson(const Manifest& manifest) {
    std::ostringstream stream;
    stream << "{\"format\":\"meshrepair-boundary-map-v1\","
           << "\"boundary_version\":1,"
           << "\"table_hash\":" << jsonString(manifest.table_hash) << ","
           << "\"boundary_hash\":" << jsonString(manifest.boundary_hash) << ",\n"
           << "\"pairs\":[";
    for (size_t i = 0; i < manifest.boundaries.size(); ++i) {
        const BoundaryDef& pair = manifest.boundaries[i];
        if (i) stream << ',';
        stream << "\n{\"chunk\":" << pair.chunk_a
               << ",\"neighbor\":" << pair.chunk_b
               << ",\"pair_hash\":" << jsonString(pair.hash)
               << ",\"vertices\":[";
        for (size_t j = 0; j < pair.vertices.size(); ++j) {
            if (j) stream << ',';
            stream << pair.vertices[j];
        }
        stream << "],\"edges\":[";
        for (size_t j = 0; j < pair.edges.size(); ++j) {
            if (j) stream << ',';
            stream << '[' << pair.edges[j].a << ',' << pair.edges[j].b << ']';
        }
        stream << "]}";
    }
    stream << "\n]}\n";
    return stream.str();
}

}  // namespace

std::string detail::planPath(const std::string& output_dir) {
    return joinPath(output_dir, "plan.bin");
}

std::string detail::chunkFileName(uint32_t chunk_id) {
    return chunkName(chunk_id) + ".mrchunk";
}

std::string detail::chunkPath(const std::string& output_dir, uint32_t chunk_id) {
    return joinPath(joinPath(output_dir, "chunks"), chunkFileName(chunk_id));
}

std::string detail::tracePath(const std::string& output_dir) {
    return joinPath(output_dir, "trace.log");
}

std::string detail::finalMeshPath(const std::string& output_dir) {
    return joinPath(output_dir, "repaired.mesh");
}

std::string detail::reportPath(const std::string& output_dir) {
    return joinPath(output_dir, "report.json");
}

std::string detail::lockPathForInput(
    const Config& config,
    const std::string& input_hash) {
    const std::string directory =
        config.temp_dir.empty() ? std::filesystem::temp_directory_path().string()
                                : config.temp_dir;
    return joinPath(directory, "meshrepair-" + input_hash.substr(0, 24) + ".lock");
}

std::string detail::chunkSummaryJson(const ChunkSummary& chunk) {
    std::ostringstream stream;
    stream << "{\"id\":" << chunk.id << ","
           << "\"cell\":[" << chunk.cell_x << "," << chunk.cell_y << ","
           << chunk.cell_z << "],"
           << "\"estimated_bytes\":" << chunk.estimated_bytes << ","
           << "\"vertices\":" << chunk.vertex_count << ","
           << "\"faces\":" << chunk.face_count << ","
           << "\"boundary_hash\":" << jsonString(chunk.boundary_hash) << ","
           << "\"bundle_hash\":" << jsonString(chunk.bundle_hash) << ","
           << "\"status\":" << jsonString(chunk.committed ? "COMMITTED" : "PENDING")
           << "}";
    return stream.str();
}

std::string detail::holeSummaryJson(const HoleSummary& hole) {
    std::ostringstream stream;
    stream << "{\"id\":" << jsonString(hole.id) << ","
           << "\"owner_chunk\":" << hole.owner_chunk << ","
           << "\"involved_chunks\":[";
    for (size_t i = 0; i < hole.involved_chunks.size(); ++i) {
        if (i) stream << ",";
        stream << hole.involved_chunks[i];
    }
    stream << "],\"vertices\":" << hole.vertex_count << ","
           << "\"filled_faces\":" << hole.filled_faces << ","
           << "\"cross_chunk\":" << (hole.cross_chunk ? "true" : "false") << ","
           << "\"status\":" << jsonString(hole.status) << ","
           << "\"bbox_min\":[" << number(hole.bbox_min.x) << ","
           << number(hole.bbox_min.y) << "," << number(hole.bbox_min.z) << "],"
           << "\"bbox_max\":[" << number(hole.bbox_max.x) << ","
           << number(hole.bbox_max.y) << "," << number(hole.bbox_max.z) << "]}";
    return stream.str();
}

std::string reportToJson(const RepairReport& report) {
    std::ostringstream stream;
    stream << "{\n";
    stream << "  \"input_hash\":" << jsonString(report.input_hash) << ",\n";
    stream << "  \"table_hash\":" << jsonString(report.table_hash) << ",\n";
    stream << "  \"boundary_hash\":" << jsonString(report.boundary_hash) << ",\n";
    stream << "  \"table_version\":1,\n";
    stream << "  \"boundary_version\":1,\n";
    stream << "  \"plan_hash\":" << jsonString(report.plan_hash) << ",\n";
    stream << "  \"stitch_hash\":" << jsonString(report.stitch_hash) << ",\n";
    stream << "  \"output\":" << jsonString(report.output_path) << ",\n";
    stream << "  \"original_vertices\":" << report.original_vertices << ",\n";
    stream << "  \"original_faces\":" << report.original_faces << ",\n";
    stream << "  \"vertices_after_weld\":" << report.vertices_after_weld << ",\n";
    stream << "  \"welded_vertex_removals\":" << report.welded_vertex_removals << ",\n";
    stream << "  \"final_vertices\":" << report.final_vertices << ",\n";
    stream << "  \"final_faces\":" << report.final_faces << ",\n";
    stream << "  \"filled_faces\":" << report.filled_faces << ",\n";
    stream << "  \"split_edges\":" << report.split_edges << ",\n";
    stream << "  \"split_groups\":" << report.split_groups << ",\n";
    stream << "  \"split_vertex_copies\":" << report.split_vertex_copies << ",\n";
    stream << "  \"debris_faces_removed\":" << report.debris_faces_removed << ",\n";
    stream << "  \"stitch_valid\":" << (report.stitch_valid ? "true" : "false") << ",\n";
    stream << "  \"resumed\":" << (report.resumed ? "true" : "false") << ",\n";
    stream << "  \"cancelled\":" << (report.cancelled ? "true" : "false") << ",\n";
    stream << "  \"normal_deviation\":{\"mean_deg\":" << number(report.normals.mean_deg)
           << ",\"max_deg\":" << number(report.normals.max_deg)
           << ",\"tolerance_deg\":" << number(report.normals.tolerance_deg)
           << ",\"exceeded\":" << (report.normals.exceeded ? "true" : "false")
           << ",\"face\":" << optionalU64(report.normals.face)
           << ",\"chunk\":" << optionalU64(report.normals.chunk) << "},\n";
    stream << "  \"validation_error\":";
    if (report.validation_error) {
        stream << errorToJson(*report.validation_error);
    } else {
        stream << "null";
    }
    stream << ",\n  \"chunks\":[";
    for (size_t i = 0; i < report.chunks.size(); ++i) {
        if (i) stream << ",";
        stream << "\n    " << chunkSummaryJson(report.chunks[i]);
    }
    stream << "\n  ],\n  \"holes\":[";
    for (size_t i = 0; i < report.holes.size(); ++i) {
        if (i) stream << ",";
        stream << "\n    " << holeSummaryJson(report.holes[i]);
    }
    stream << "\n  ],\n  \"split_ownership\":[";
    for (size_t i = 0; i < report.split_ownership.size(); ++i) {
        const SplitOwnership& ownership = report.split_ownership[i];
        if (i) stream << ",";
        stream << "\n    {\"edge\":[" << ownership.edge_a << ","
               << ownership.edge_b << "],\"owner_chunk\":"
               << ownership.owner_chunk << ",\"group\":" << ownership.group
               << ",\"faces\":" << ownership.face_count << "}";
    }
    stream << "\n  ]\n}\n";
    return stream.str();
}

namespace {

struct EngineContext {
    const Mesh& mesh;
    const Config& config;
    const std::string& output_dir;
    const RepairOptions& options;
    const Topology& topology;
};

Result<RepairReport> runRepair(const EngineContext& context, bool verify_only) {
    const Mesh& mesh = context.mesh;
    const Config& config = context.config;
    const std::string& output_dir = context.output_dir;
    const RepairOptions& options = context.options;

    Status config_status = validateConfig(config);
    if (!config_status) return Result<RepairReport>::failure(config_status.error);
    Status directory_status = ensureDirectory(output_dir);
    if (!directory_status) return Result<RepairReport>::failure(directory_status.error);
    directory_status = ensureDirectory(joinPath(output_dir, "chunks"));
    if (!directory_status) return Result<RepairReport>::failure(directory_status.error);
    Status space_status = checkDiskReserve(output_dir, config.temp_reserve_bytes);
    if (!space_status) return Result<RepairReport>::failure(space_status.error);
    if (!config.temp_dir.empty()) {
        directory_status = ensureDirectory(config.temp_dir);
        if (!directory_status) {
            return Result<RepairReport>::failure(directory_status.error);
        }
        space_status =
            checkDiskReserve(config.temp_dir, config.temp_reserve_bytes);
        if (!space_status) {
            return Result<RepairReport>::failure(space_status.error);
        }
    }

    auto canonical = serializeMesh(mesh, 1, 8);
    if (!canonical) return Result<RepairReport>::failure(canonical.error);
    const std::string input_hash = sha256Hex(*canonical);
    const std::string config_hash = detail::configHash(config);

    auto expected_manifest =
        buildManifest(mesh, config, context.topology, input_hash, config_hash);
    if (!expected_manifest) {
        return Result<RepairReport>::failure(expected_manifest.error);
    }

    SessionLock lock;
    Status lock_status =
        lock.acquire(lockPathForInput(config, input_hash));
    if (!lock_status) return Result<RepairReport>::failure(lock_status.error);
    if (options.hold_lock_ms > 0) {
        std::this_thread::sleep_for(std::chrono::milliseconds(options.hold_lock_ms));
    }
    removeStaleTemps(output_dir);
    removeStaleTemps(joinPath(output_dir, "chunks"));

    auto manifest_result =
        loadOrCreateManifest(*expected_manifest, output_dir, verify_only);
    if (!manifest_result) {
        return Result<RepairReport>::failure(manifest_result.error);
    }
    const Manifest& manifest = *manifest_result;

    if (!verify_only) {
        const std::string table_json = chunkTableJson(manifest);
        const std::string boundary_json = boundaryMapJson(manifest);
        Status table_status = atomicWriteFile(
            joinPath(output_dir, "chunk_table.json"),
            std::vector<uint8_t>(table_json.begin(), table_json.end()));
        if (!table_status) {
            return Result<RepairReport>::failure(table_status.error);
        }
        Status boundary_status = atomicWriteFile(
            joinPath(output_dir, "boundary_map.json"),
            std::vector<uint8_t>(boundary_json.begin(), boundary_json.end()));
        if (!boundary_status) {
            return Result<RepairReport>::failure(boundary_status.error);
        }
    }

    for (const ChunkDef& chunk : manifest.chunks) {
        if (chunk.estimated_bytes > config.block_memory_bytes) {
            Error error = makeError(
                ErrorCode::BudgetExceeded,
                "chunk exceeds block_memory_bytes and was not started",
                chunk.id);
            error.estimated_bytes = chunk.estimated_bytes;
            return Result<RepairReport>::failure(std::move(error));
        }
    }

    std::vector<std::optional<ChunkBundle>> bundles(manifest.chunks.size());
    std::vector<bool> present(manifest.chunks.size(), false);
    for (uint32_t i = 0; i < manifest.chunks.size(); ++i) {
        present[i] = std::filesystem::exists(chunkPath(output_dir, i));
    }
    for (uint32_t i = 0; i < manifest.chunks.size(); ++i) {
        if (!present[i]) {
            for (uint32_t later = i + 1; later < present.size(); ++later) {
                if (present[later]) {
                    return Result<RepairReport>::failure(makeError(
                        ErrorCode::RecoveryInvalid,
                        "committed chunk chain has a gap",
                        i));
                }
            }
            break;
        }
    }

    std::string expected_chain = manifest.plan_hash;
    for (uint32_t i = 0; i < manifest.chunks.size(); ++i) {
        if (!present[i]) {
            break;
        }
        auto bundle = readChunkBundle(chunkPath(output_dir, i));
        if (!bundle) {
            Error error = bundle.error;
            error.code = ErrorCode::RecoveryInvalid;
            error.chunk = i;
            return Result<RepairReport>::failure(std::move(error));
        }
        if (bundle->id != i ||
            bundle->input_hash != manifest.input_hash ||
            bundle->table_hash != manifest.table_hash ||
            bundle->boundary_hash != manifest.boundary_hash ||
            bundle->local_boundary_hash != localBoundaryHash(manifest, i) ||
            bundle->prev_chain_hash != expected_chain) {
            return Result<RepairReport>::failure(makeError(
                ErrorCode::RecoveryInvalid,
                "chunk bundle header or hash chain does not match committed state",
                i));
        }
        expected_chain = bundle->hash;
        bundles[i] = std::move(*bundle);
    }
    const bool resumed = std::any_of(
        present.begin(), present.end(), [](bool value) { return value; });
    const bool all_committed = std::all_of(
        present.begin(), present.end(), [](bool value) { return value; });

    if (verify_only && !all_committed) {
        return Result<RepairReport>::failure(makeError(
            ErrorCode::RecoveryInvalid,
            "verify requires every chunk to have been committed",
            static_cast<uint64_t>(std::find(present.begin(), present.end(), false) -
                                  present.begin())));
    }

    RepairPlanData plan;
    if (!verify_only || all_committed) {
        auto computed = computeRepairPlan(
            mesh, config, manifest, context.topology);
        if (!computed) return Result<RepairReport>::failure(computed.error);
        plan = std::move(*computed);
    }

    if (!verify_only && !all_committed) {
        std::vector<uint32_t> missing;
        for (uint32_t i = 0; i < present.size(); ++i) {
            if (!present[i]) missing.push_back(i);
        }
        uint64_t new_commits = 0;
        size_t index = 0;
        while (index < missing.size()) {
            if (options.cancel &&
                options.cancel->load(std::memory_order_relaxed)) {
                return Result<RepairReport>::failure(makeError(
                    ErrorCode::Cancelled,
                    "repair cancelled at a chunk boundary"));
            }
            std::vector<uint32_t> batch;
            uint64_t reserved = 0;
            while (index < missing.size() &&
                   batch.size() < config.parallelism) {
                const uint32_t chunk_id = missing[index];
                const uint64_t estimate = manifest.chunks[chunk_id].estimated_bytes;
                if (estimate > config.block_memory_bytes) {
                    Error error = makeError(
                        ErrorCode::BudgetExceeded,
                        "chunk exceeds block_memory_bytes and was not started",
                        chunk_id);
                    error.estimated_bytes = estimate;
                    return Result<RepairReport>::failure(std::move(error));
                }
                if (!batch.empty() &&
                    (reserved > config.memory_budget_bytes - estimate)) {
                    break;
                }
                if (batch.empty() && estimate > config.memory_budget_bytes) {
                    Error error = makeError(
                        ErrorCode::BudgetExceeded,
                        "chunk exceeds the global memory budget",
                        chunk_id);
                    error.estimated_bytes = estimate;
                    return Result<RepairReport>::failure(std::move(error));
                }
                batch.push_back(chunk_id);
                reserved += estimate;
                ++index;
            }
            if (batch.empty()) {
                return Result<RepairReport>::failure(makeError(
                    ErrorCode::Internal,
                    "scheduler made no progress"));
            }
            std::vector<std::future<Result<ChunkBundle>>> futures;
            futures.reserve(batch.size());
            for (uint32_t chunk_id : batch) {
                futures.push_back(std::async(
                    std::launch::async,
                    [&plan, &manifest, chunk_id]() {
                        return makeChunkBundle(
                            plan,
                            manifest,
                            chunk_id,
                            std::string("scheduled"));
                    }));
            }
            std::vector<ChunkBundle> built;
            built.reserve(batch.size());
            for (auto& future : futures) {
                auto result = future.get();
                if (!result) return Result<RepairReport>::failure(result.error);
                built.push_back(std::move(*result));
            }
            for (ChunkBundle& bundle : built) {
                if (options.cancel &&
                    options.cancel->load(std::memory_order_relaxed)) {
                    return Result<RepairReport>::failure(makeError(
                        ErrorCode::Cancelled,
                        "repair cancelled at a commit boundary",
                        bundle.id));
                }
                const uint32_t chunk_id = bundle.id;
                const std::string prev =
                    chunk_id == 0 ? manifest.plan_hash
                                  : bundles[chunk_id - 1]->hash;
                bundle.prev_chain_hash = prev;
                const std::vector<uint8_t> bytes = serializeChunkBundle(bundle);
                const std::string digest =
                    sha256Hex(bytes.data(), bytes.size() - 32);
                std::error_code ec;
                if (std::filesystem::exists(chunkPath(output_dir, chunk_id), ec)) {
                    auto existing = readChunkBundle(chunkPath(output_dir, chunk_id));
                    if (!existing || existing->hash != digest) {
                        return Result<RepairReport>::failure(makeError(
                            ErrorCode::RecoveryInvalid,
                            "refusing to overwrite a different committed chunk",
                            chunk_id));
                    }
                } else {
                    Status write_status =
                        atomicWriteFile(chunkPath(output_dir, chunk_id), bytes);
                    if (!write_status) {
                        return Result<RepairReport>::failure(write_status.error);
                    }
                }
                bundle.hash = digest;
                bundles[chunk_id] = std::move(bundle);
                present[chunk_id] = true;
                ++new_commits;
                Status trace_status = writeTrace(
                    output_dir, manifest, bundles, std::string(), false, config);
                if (!trace_status) {
                    return Result<RepairReport>::failure(trace_status.error);
                }
                publishSnapshot(options, bundles);
                if (new_commits >= options.stop_after_commits) {
#ifndef _WIN32
                    ::_exit(137);
#else
                    std::_Exit(137);
#endif
                }
            }
        }
    }

    auto bundle_vector = [&]() {
        std::vector<ChunkBundle> result;
        result.reserve(bundles.size());
        for (const auto& bundle : bundles) {
            if (bundle) result.push_back(*bundle);
        }
        return result;
    }();
    auto stitch = stitchChunks(mesh, manifest, bundle_vector);
    if (!stitch) return Result<RepairReport>::failure(stitch.error);

    RepairReport report;
    report.input_hash = manifest.input_hash;
    report.table_hash = manifest.table_hash;
    report.boundary_hash = manifest.boundary_hash;
    report.plan_hash = manifest.plan_hash;
    report.stitch_hash = stitch->hash;
    report.output_path = finalMeshPath(output_dir);
    report.original_vertices = mesh.vertices.size();
    report.original_faces = mesh.faces.size();
    report.vertices_after_weld = plan.vertices_after_weld;
    report.welded_vertex_removals = plan.welded_vertex_removals;
    report.final_vertices = stitch->mesh.vertices.size();
    report.final_faces = stitch->mesh.faces.size();
    report.filled_faces = plan.filled_faces;
    report.split_edges = plan.split_edges;
    report.split_groups = plan.split_groups;
    report.split_vertex_copies = plan.split_vertex_copies;
    report.debris_faces_removed = plan.debris_faces_removed;
    report.stitch_valid = !stitch->error.has_value();
    report.resumed = resumed;
    report.normals.tolerance_deg = config.normal_tolerance_deg;
    report.holes = plan.holes;
    report.split_ownership = plan.split_ownership;
    for (const ChunkDef& chunk : manifest.chunks) {
        ChunkSummary summary;
        summary.id = chunk.id;
        summary.cell_x = chunk.coord.x;
        summary.cell_y = chunk.coord.y;
        summary.cell_z = chunk.coord.z;
        summary.estimated_bytes = chunk.estimated_bytes;
        summary.boundary_hash = localBoundaryHash(manifest, chunk.id);
        if (bundles[chunk.id]) {
            summary.committed = true;
            summary.vertex_count = bundles[chunk.id]->vertices.size();
            summary.face_count = bundles[chunk.id]->faces.size();
            summary.bundle_hash = bundles[chunk.id]->hash;
        }
        report.chunks.push_back(std::move(summary));
    }
    computeNormalDeviation(mesh, plan, report);
    if (report.normals.max_deg > config.normal_tolerance_deg + 1e-12) {
        report.normals.exceeded = true;
        Error error = makeError(
            ErrorCode::TopologyConflict,
            "normal deviation exceeds configured tolerance",
            report.normals.chunk,
            report.normals.face);
        report.validation_error = error;
    }

    if (!verify_only) {
        auto final_bytes = serializeMesh(stitch->mesh, 1, 8);
        if (!final_bytes) return Result<RepairReport>::failure(final_bytes.error);
        Status write_status = atomicWriteFile(finalMeshPath(output_dir), *final_bytes);
        if (!write_status) return Result<RepairReport>::failure(write_status.error);
        const std::string hash_line = stitch->hash + "  repaired.mesh\n";
        write_status = atomicWriteFile(
            joinPath(output_dir, "stitch.sha256"),
            std::vector<uint8_t>(hash_line.begin(), hash_line.end()));
        if (!write_status) return Result<RepairReport>::failure(write_status.error);
        write_status = writeTrace(
            output_dir, manifest, bundles, stitch->hash, true, config);
        if (!write_status) return Result<RepairReport>::failure(write_status.error);
        const std::string json = reportToJson(report);
        write_status = atomicWriteFile(
            reportPath(output_dir),
            std::vector<uint8_t>(json.begin(), json.end()));
        if (!write_status) return Result<RepairReport>::failure(write_status.error);
    }
    publishSnapshot(options, bundles);
    return Result<RepairReport>::success(std::move(report));
}

}  // namespace

Result<RepairReport> repairMesh(
    const Mesh& mesh,
    const Config& config,
    const std::string& output_dir,
    const RepairOptions& options) {
    auto topology = buildTopology(mesh);
    if (!topology) return Result<RepairReport>::failure(topology.error);
    EngineContext context{mesh, config, output_dir, options, *topology};
    return runRepair(context, options.verify_only);
}

Result<RepairReport> repairFile(
    const std::string& input_path,
    const std::string& config_path,
    const std::string& output_dir,
    const RepairOptions& options) {
    auto mesh = loadMesh(input_path);
    if (!mesh) return Result<RepairReport>::failure(mesh.error);
    auto config = loadConfig(config_path);
    if (!config) return Result<RepairReport>::failure(config.error);
    return repairMesh(*mesh, *config, output_dir, options);
}

Result<RepairReport> verifyCommitted(
    const Mesh& mesh,
    const Config& config,
    const std::string& output_dir) {
    RepairOptions options;
    options.verify_only = true;
    return repairMesh(mesh, config, output_dir, options);
}

Result<std::vector<HoleSummary>> evaluateHoleBoundaryGates(
    const Mesh& mesh,
    const Config& config,
    const std::vector<std::pair<uint32_t, uint32_t>>& committed_pairs) {
    auto topology = buildTopology(mesh);
    if (!topology) return Result<std::vector<HoleSummary>>::failure(topology.error);
    auto canonical = serializeMesh(mesh, 1, 8);
    if (!canonical) {
        return Result<std::vector<HoleSummary>>::failure(canonical.error);
    }
    const std::string input_hash = sha256Hex(*canonical);
    auto manifest = detail::buildManifest(
        mesh, config, *topology, input_hash, detail::configHash(config));
    if (!manifest) {
        return Result<std::vector<HoleSummary>>::failure(manifest.error);
    }
    auto plan = detail::computeRepairPlan(mesh, config, *manifest, *topology);
    if (!plan) return Result<std::vector<HoleSummary>>::failure(plan.error);

    std::set<std::pair<uint32_t, uint32_t>> committed;
    for (const auto& pair : committed_pairs) {
        committed.emplace(std::min(pair.first, pair.second),
                          std::max(pair.first, pair.second));
    }
    for (HoleSummary& hole : plan->holes) {
        if (!hole.cross_chunk) continue;
        for (uint32_t other : hole.involved_chunks) {
            if (other == hole.owner_chunk) continue;
            const auto pair = std::make_pair(
                std::min(hole.owner_chunk, other),
                std::max(hole.owner_chunk, other));
            if (committed.count(pair) == 0) {
                hole.status = "BOUNDARY_PENDING";
                Error error = makeError(
                    ErrorCode::BoundaryPending,
                    "cross-chunk hole requires both endpoint boundary maps to be committed",
                    hole.owner_chunk);
                error.hole_id = hole.id;
                return Result<std::vector<HoleSummary>>::failure(std::move(error));
            }
        }
    }
    return Result<std::vector<HoleSummary>>::success(std::move(plan->holes));
}

}  // namespace meshrepair
