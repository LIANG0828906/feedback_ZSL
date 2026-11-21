#include "internal.hpp"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cmath>
#include <cstdlib>
#include <cstdint>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <future>
#include <iostream>
#include <set>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

#ifndef _WIN32
#include <sys/wait.h>
#include <unistd.h>
#endif

namespace {

using namespace meshrepair;
using namespace meshrepair::detail;

int g_failures = 0;
int g_checks = 0;

#define CHECK(condition)                                                        \
    do {                                                                        \
        ++g_checks;                                                             \
        if (!(condition)) {                                                     \
            ++g_failures;                                                       \
            std::cerr << "FAIL " << __FILE__ << ":" << __LINE__ << "  "        \
                      << #condition << '\n';                                    \
        }                                                                       \
    } while (false)

#define CHECK_OK(expression)                                                    \
    do {                                                                        \
        auto _result = (expression);                                            \
        ++g_checks;                                                             \
        if (!_result) {                                                         \
            ++g_failures;                                                       \
            std::cerr << "FAIL " << __FILE__ << ":" << __LINE__ << "  "        \
                      << #expression << " -> "                                  \
                      << errorCodeName(_result.error.code) << ": "              \
                      << _result.error.message << '\n';                         \
        }                                                                       \
    } while (false)

void expectError(
    const Error& error,
    ErrorCode code,
    const std::string& where) {
    CHECK(error.code == code);
    if (error.code != code) {
        std::cerr << "  at " << where << ": got " << errorCodeName(error.code)
                  << " expected " << errorCodeName(code) << " ("
                  << error.message << ")\n";
    }
}

std::filesystem::path uniqueRoot() {
    static std::atomic<uint64_t> counter{0};
#ifndef _WIN32
    const uint64_t pid = static_cast<uint64_t>(::getpid());
#else
    const uint64_t pid = 0;
#endif
    return std::filesystem::temp_directory_path() /
           ("meshrepair-tests-" + std::to_string(pid) + "-" +
            std::to_string(counter.fetch_add(1)));
}

uint32_t readU32(const std::vector<uint8_t>& bytes, size_t offset) {
    return static_cast<uint32_t>(bytes[offset]) |
           (static_cast<uint32_t>(bytes[offset + 1]) << 8) |
           (static_cast<uint32_t>(bytes[offset + 2]) << 16) |
           (static_cast<uint32_t>(bytes[offset + 3]) << 24);
}

void writeU32(std::vector<uint8_t>& bytes, size_t offset, uint32_t value) {
    bytes[offset] = static_cast<uint8_t>(value & 0xff);
    bytes[offset + 1] = static_cast<uint8_t>((value >> 8) & 0xff);
    bytes[offset + 2] = static_cast<uint8_t>((value >> 16) & 0xff);
    bytes[offset + 3] = static_cast<uint8_t>((value >> 24) & 0xff);
}

void writeU64(std::vector<uint8_t>& bytes, size_t offset, uint64_t value) {
    for (int i = 0; i < 8; ++i) {
        bytes[offset + static_cast<size_t>(i)] =
            static_cast<uint8_t>((value >> (8 * i)) & 0xff);
    }
}

Config sampleConfig() {
    auto config = parseConfig(defaultConfigText());
    Config result = *config;
    result.temp_dir = uniqueRoot().string();
    std::filesystem::create_directories(result.temp_dir);
    result.parallelism = 4;
    return result;
}

void testHash() {
    CHECK(
        sha256Hex(std::string("abc")) ==
        "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
    std::vector<uint8_t> range(1000);
    for (size_t i = 0; i < range.size(); ++i) range[i] = static_cast<uint8_t>(i);
    CHECK(
        sha256Hex(range) ==
        "a8af099bf2e878609558dbf69d8f88f4a31040a8cf84b549a0cfa912f12ffc3f");
}

void testMeshValidation() {
    auto sample = generateSample(42);
    CHECK_OK(sample);
    auto encoded = serializeMesh(*sample, 0, 4);
    CHECK_OK(encoded);
    auto parsed = parseMesh(*encoded, "roundtrip");
    CHECK_OK(parsed);
    CHECK(parsed->vertices.size() == sample->vertices.size());
    CHECK(parsed->faces.size() == sample->faces.size());
    auto reencoded = serializeMesh(*parsed, 0, 4);
    CHECK_OK(reencoded);
    CHECK(*reencoded == *encoded);

    {
        auto bad = *encoded;
        const size_t face_offset = 48 + sample->vertices.size() * 12;
        writeU32(bad, face_offset, static_cast<uint32_t>(sample->vertices.size()));
        auto result = parseMesh(bad, "bad-index");
        CHECK(!result);
        expectError(result.error, ErrorCode::MeshInvalid, "bad-index");
        CHECK(result.error.face.has_value() && *result.error.face == 0);
    }
    {
        auto bad = *encoded;
        writeU32(bad, 48, 0x7fc00000u);
        auto result = parseMesh(bad, "nan");
        CHECK(!result);
        expectError(result.error, ErrorCode::MeshInvalid, "nan");
        CHECK(result.error.vertex.has_value() && *result.error.vertex == 0);
    }
    {
        auto bad = *encoded;
        writeU64(bad, 24, sample->faces.size() + 1);
        auto result = parseMesh(bad, "face-count");
        CHECK(!result);
        expectError(result.error, ErrorCode::MeshInvalid, "face-count");
        CHECK(result.error.face.has_value());
    }
    {
        auto bad = *encoded;
        bad.resize(bad.size() - 4);
        auto result = parseMesh(bad, "truncated");
        CHECK(!result);
        expectError(result.error, ErrorCode::MeshInvalid, "truncated");
        CHECK(result.error.face.has_value());
    }
}

void testConfigValidation() {
    auto good = parseConfig(defaultConfigText());
    CHECK_OK(good);
    auto missing = parseConfig(
        "parallelism=4\n"
        "memory_budget_bytes=100\n"
        "block_memory_bytes=50\n"
        "chunk_size_x=1\n"
        "chunk_size_y=1\n"
        "chunk_size_z=1\n");
    CHECK(!missing);
    expectError(missing.error, ErrorCode::ConfigInvalid, "missing field");

    auto zero = parseConfig(
        "parallelism=0\n"
        "memory_budget_bytes=100\n"
        "block_memory_bytes=50\n"
        "chunk_size_x=1\n"
        "chunk_size_y=1\n"
        "chunk_size_z=1\n"
        "weld_tolerance=0\n");
    CHECK(!zero);
    expectError(zero.error, ErrorCode::ConfigInvalid, "zero parallelism");

    auto tooLarge = parseConfig(
        "parallelism=2\n"
        "memory_budget_bytes=100\n"
        "block_memory_bytes=101\n"
        "chunk_size_x=1\n"
        "chunk_size_y=1\n"
        "chunk_size_z=1\n"
        "weld_tolerance=0\n");
    CHECK(!tooLarge);
    expectError(tooLarge.error, ErrorCode::ConfigInvalid, "block > global");
}

void testTopologyAndManifest() {
    auto sample = generateSample(9);
    CHECK_OK(sample);
    auto topology = buildTopology(*sample);
    CHECK_OK(topology);
    CHECK(topology->edge_faces.size() == 30);
    CHECK(topology->boundary_edges.size() == 13);
    CHECK(topology->nonmanifold_edges.size() == 1);

    Config a = sampleConfig();
    Config b = a;
    b.parallelism = 1;
    auto encoded = serializeMesh(*sample, 1, 8);
    CHECK_OK(encoded);
    const std::string input_hash = sha256Hex(*encoded);
    auto manifest_a = buildManifest(
        *sample, a, *topology, input_hash, configHash(a));
    auto manifest_b = buildManifest(
        *sample, b, *topology, input_hash, configHash(b));
    CHECK_OK(manifest_a);
    CHECK_OK(manifest_b);
    CHECK(manifest_a->table_hash == manifest_b->table_hash);
    CHECK(manifest_a->boundary_hash == manifest_b->boundary_hash);
    CHECK(!manifest_a->straddles.empty());
    CHECK(!manifest_a->boundaries.empty());
    for (const BoundaryDef& boundary : manifest_a->boundaries) {
        CHECK(boundary.chunk_a < boundary.chunk_b);
        CHECK(!boundary.vertices.empty());
        CHECK(!boundary.edges.empty());
        CHECK(std::is_sorted(boundary.vertices.begin(), boundary.vertices.end()));
        CHECK(std::is_sorted(boundary.edges.begin(), boundary.edges.end()));
    }
    const auto serialized_a = serializeManifest(*manifest_a);
    auto parsed_a = parseManifestBytes(serialized_a);
    CHECK_OK(parsed_a);
    CHECK(parsed_a->table_hash == manifest_a->table_hash);
    CHECK(parsed_a->boundary_hash == manifest_a->boundary_hash);
    CHECK(parsed_a->plan_hash == manifest_a->plan_hash);
}

void testRepairAndDeterminism() {
    const std::filesystem::path root = uniqueRoot();
    std::filesystem::create_directories(root);
    auto sample = generateSample(42);
    CHECK_OK(sample);
    Config config = sampleConfig();

    std::vector<std::shared_ptr<const Snapshot>> snapshots;
    RepairOptions options;
    options.on_snapshot = [&](std::shared_ptr<const Snapshot> snapshot) {
        snapshots.push_back(std::move(snapshot));
    };
    auto report = repairMesh(*sample, config, (root / "a").string(), options);
    CHECK_OK(report);
    if (!report) return;
    CHECK(report->original_vertices == 17);
    CHECK(report->original_faces == 16);
    CHECK(report->welded_vertex_removals == 1);
    CHECK(report->vertices_after_weld == 16);
    CHECK(report->filled_faces == 2);
    CHECK(report->split_edges == 1);
    CHECK(report->split_groups >= 2);
    CHECK(report->split_vertex_copies == 2);
    CHECK(report->split_ownership.size() == report->split_groups);
    for (const SplitOwnership& ownership : report->split_ownership) {
        CHECK(ownership.edge_a < ownership.edge_b);
        CHECK(ownership.face_count >= 1);
    }
    CHECK(report->debris_faces_removed == 1);
    CHECK(report->final_faces == 17);
    CHECK(report->final_vertices == 17);
    CHECK(report->stitch_valid);
    CHECK(report->holes.size() == 1);
    CHECK(report->holes[0].cross_chunk);
    CHECK(report->holes[0].vertex_count == 4);
    CHECK(report->holes[0].filled_faces == 2);
    CHECK(!report->chunks.empty());
    CHECK(!snapshots.empty());
    uint64_t previous = 0;
    for (const auto& snapshot : snapshots) {
        CHECK(snapshot);
        CHECK(snapshot->committed_count >= previous);
        previous = snapshot->committed_count;
    }
    CHECK(previous == report->chunks.size());

    auto final_mesh = loadMesh((root / "a" / "repaired.mesh").string());
    CHECK_OK(final_mesh);
    CHECK(final_mesh->faces.size() == 17);
    auto boundary_json =
        readFileBytes((root / "a" / "boundary_map.json").string());
    auto table_json = readFileBytes((root / "a" / "chunk_table.json").string());
    CHECK_OK(boundary_json);
    CHECK_OK(table_json);
    if (boundary_json) {
        const std::string text(boundary_json->begin(), boundary_json->end());
        CHECK(text.find("\"neighbor\"") != std::string::npos);
        CHECK(text.find(report->boundary_hash) != std::string::npos);
    }
    if (table_json) {
        const std::string text(table_json->begin(), table_json->end());
        CHECK(text.find("\"straddling_faces\"") != std::string::npos);
        CHECK(text.find(report->table_hash) != std::string::npos);
    }
    auto verified = verifyCommitted(
        *sample, config, (root / "a").string());
    CHECK_OK(verified);
    CHECK(verified && verified->stitch_hash == report->stitch_hash);

    auto committed_plan =
        readFileBytes(planPath((root / "a").string()));
    CHECK_OK(committed_plan);
    if (committed_plan) {
        auto parsed_plan = parseManifestBytes(*committed_plan);
        CHECK_OK(parsed_plan);
        if (parsed_plan) {
            std::vector<ChunkBundle> reversed_bundles;
            for (const ChunkDef& chunk : parsed_plan->chunks) {
                auto bundle =
                    readChunkBundle(chunkPath((root / "a").string(), chunk.id));
                CHECK_OK(bundle);
                if (bundle) reversed_bundles.push_back(std::move(*bundle));
            }
            std::reverse(reversed_bundles.begin(), reversed_bundles.end());
            auto reordered =
                stitchChunks(*sample, *parsed_plan, reversed_bundles);
            CHECK_OK(reordered);
            CHECK(reordered && reordered->hash == report->stitch_hash);
        }
    }

    auto repeat = repairMesh(*sample, config, (root / "a").string());
    CHECK_OK(repeat);
    CHECK(repeat && repeat->resumed);
    CHECK(repeat && repeat->stitch_hash == report->stitch_hash);

    Config serial = config;
    serial.parallelism = 1;
    auto serial_report =
        repairMesh(*sample, serial, (root / "b").string());
    CHECK_OK(serial_report);
    CHECK(serial_report && serial_report->table_hash == report->table_hash);
    CHECK(serial_report && serial_report->boundary_hash == report->boundary_hash);
    CHECK(serial_report && serial_report->stitch_hash == report->stitch_hash);
    auto bytes_a = readFileBytes((root / "a" / "repaired.mesh").string());
    auto bytes_b = readFileBytes((root / "b" / "repaired.mesh").string());
    CHECK_OK(bytes_a);
    CHECK_OK(bytes_b);
    CHECK(bytes_a && bytes_b && *bytes_a == *bytes_b);
    auto same_config =
        repairMesh(*sample, config, (root / "c").string());
    CHECK_OK(same_config);
    CHECK(same_config && same_config->stitch_hash == report->stitch_hash);
    auto trace_a = readFileBytes((root / "a" / "trace.log").string());
    auto trace_b = readFileBytes((root / "c" / "trace.log").string());
    CHECK_OK(trace_a);
    CHECK_OK(trace_b);
    CHECK(trace_a && trace_b && *trace_a == *trace_b);

    std::filesystem::remove_all(root);
}

void testBoundaryGates() {
    auto sample = generateSample(42);
    CHECK_OK(sample);
    Config config = sampleConfig();
    auto pending =
        evaluateHoleBoundaryGates(*sample, config, {});
    CHECK(!pending);
    expectError(pending.error, ErrorCode::BoundaryPending, "boundary gate");
    CHECK(pending.error.hole_id.has_value());
    CHECK(pending.error.chunk.has_value());

    auto all_pairs = evaluateHoleBoundaryGates(
        *sample, config,
        {{0, 1}, {0, 2}, {1, 2}});
    CHECK_OK(all_pairs);
    CHECK(all_pairs && all_pairs->size() == 1);
    CHECK(all_pairs && (*all_pairs)[0].status == "FILLED");
}

void testDuplicateFillInterception() {
    auto sample = generateSample(3);
    CHECK_OK(sample);
    Config config = sampleConfig();
    auto topology = buildTopology(*sample);
    CHECK_OK(topology);
    auto encoded = serializeMesh(*sample, 1, 8);
    CHECK_OK(encoded);
    auto manifest = buildManifest(
        *sample,
        config,
        *topology,
        sha256Hex(*encoded),
        configHash(config));
    CHECK_OK(manifest);

    ChunkBundle first;
    first.id = 0;
    first.input_hash = manifest->input_hash;
    first.table_hash = manifest->table_hash;
    first.boundary_hash = manifest->boundary_hash;
    first.vertices = {
        {0, sample->vertices[0]},
        {1, sample->vertices[1]},
        {2, sample->vertices[2]}};
    first.faces = {
        {kGeneratedSourceFace, {{0, 1, 2}}, 0, "HOLE_TEST", true}};
    first.filled_faces = 1;
    ChunkBundle second = first;
    second.id = 1;
    second.faces = {
        {kGeneratedSourceFace, {{2, 1, 0}}, 1, "HOLE_TEST", true}};
    const std::vector<ChunkBundle> bundles{first, second};
    auto stitched = stitchChunks(*sample, *manifest, bundles);
    CHECK(!stitched);
    expectError(stitched.error, ErrorCode::TopologyConflict, "duplicate fill");
    CHECK(stitched.error.chunk.has_value() && *stitched.error.chunk == 1);
    CHECK(stitched.error.hole_id.has_value());
    CHECK(*stitched.error.hole_id == "HOLE_TEST");
}

void testBudgetAndLocking() {
    const std::filesystem::path root = uniqueRoot();
    std::filesystem::create_directories(root);
    auto sample = generateSample(42);
    CHECK_OK(sample);
    Config config = sampleConfig();
    config.block_memory_bytes = 1024;
    auto report = repairMesh(*sample, config, (root / "budget").string());
    CHECK(!report);
    expectError(report.error, ErrorCode::BudgetExceeded, "budget");
    CHECK(report.error.chunk.has_value());
    CHECK(report.error.estimated_bytes.has_value());
    CHECK(*report.error.estimated_bytes > 1024);

    Config global_limited = config;
    global_limited.block_memory_bytes = 8000;
    global_limited.memory_budget_bytes = 8000;
    global_limited.parallelism = 4;
    auto queued = repairMesh(
        *sample, global_limited, (root / "global-budget").string());
    CHECK_OK(queued);
    auto limited_trace =
        readFileBytes((root / "global-budget" / "trace.log").string());
    CHECK_OK(limited_trace);
    if (limited_trace) {
        const std::string text(limited_trace->begin(), limited_trace->end());
        CHECK(text.find("reason=GLOBAL_BUDGET") != std::string::npos);
        CHECK(text.find("admission=WAIT_QUEUE") != std::string::npos);
    }

    SessionLock first;
    const std::string lock_path = (root / "lock").string();
    CHECK_OK(first.acquire(lock_path));
    SessionLock second;
    auto lock = second.acquire(lock_path);
    CHECK(!lock);
    expectError(lock.error, ErrorCode::Busy, "session lock");
    first.release();
    CHECK_OK(second.acquire(lock_path));
    second.release();

    Config held = sampleConfig();
    RepairOptions hold_options;
    hold_options.hold_lock_ms = 500;
    auto held_future = std::async(
        std::launch::async,
        [&]() {
            return repairMesh(*sample, held, (root / "held").string(), hold_options);
        });
    std::this_thread::sleep_for(std::chrono::milliseconds(100));
    auto busy = repairMesh(*sample, held, (root / "busy").string());
    CHECK(!busy);
    expectError(busy.error, ErrorCode::Busy, "engine lock");
    auto held_result = held_future.get();
    CHECK_OK(held_result);
    std::filesystem::remove_all(root);
}

void testRecoveryValidation() {
    const std::filesystem::path root = uniqueRoot();
    std::filesystem::create_directories(root);
    auto sample = generateSample(11);
    CHECK_OK(sample);
    Config config = sampleConfig();
    auto report = repairMesh(*sample, config, (root / "base").string());
    CHECK_OK(report);

    std::filesystem::copy(
        root / "base",
        root / "corrupt",
        std::filesystem::copy_options::recursive |
            std::filesystem::copy_options::overwrite_existing);
    auto chunk_bytes =
        readFileBytes(chunkPath((root / "corrupt").string(), 1));
    CHECK_OK(chunk_bytes);
    if (chunk_bytes) {
        (*chunk_bytes)[chunk_bytes->size() - 1] ^= 0x5a;
        CHECK_OK(atomicWriteFile(chunkPath((root / "corrupt").string(), 1), *chunk_bytes));
    }
    auto corrupt = repairMesh(*sample, config, (root / "corrupt").string());
    CHECK(!corrupt);
    expectError(corrupt.error, ErrorCode::RecoveryInvalid, "chunk corruption");
    CHECK(corrupt.error.chunk.has_value() && *corrupt.error.chunk == 1);

    std::filesystem::copy(
        root / "base",
        root / "gap",
        std::filesystem::copy_options::recursive |
            std::filesystem::copy_options::overwrite_existing);
    std::filesystem::remove(chunkPath((root / "gap").string(), 1));
    auto gap = repairMesh(*sample, config, (root / "gap").string());
    CHECK(!gap);
    expectError(gap.error, ErrorCode::RecoveryInvalid, "chain gap");
    CHECK(gap.error.chunk.has_value() && *gap.error.chunk == 1);

    std::filesystem::copy(
        root / "base",
        root / "plan-corrupt",
        std::filesystem::copy_options::recursive |
            std::filesystem::copy_options::overwrite_existing);
    auto plan_bytes = readFileBytes(planPath((root / "plan-corrupt").string()));
    CHECK_OK(plan_bytes);
    if (plan_bytes) {
        (*plan_bytes)[100] ^= 0xff;
        CHECK_OK(atomicWriteFile(planPath((root / "plan-corrupt").string()), *plan_bytes));
    }
    auto plan_corrupt =
        repairMesh(*sample, config, (root / "plan-corrupt").string());
    CHECK(!plan_corrupt);
    expectError(plan_corrupt.error, ErrorCode::RecoveryInvalid, "plan corruption");
    CHECK(plan_corrupt.error.chunk.has_value() && *plan_corrupt.error.chunk == 0);
    std::filesystem::remove_all(root);
}

void testCancellation() {
    const std::filesystem::path root = uniqueRoot();
    std::filesystem::create_directories(root);
    auto sample = generateSample(5);
    CHECK_OK(sample);
    Config config = sampleConfig();
    std::atomic<bool> cancel{true};
    RepairOptions options;
    options.cancel = &cancel;
    auto report = repairMesh(
        *sample, config, (root / "cancel").string(), options);
    CHECK(!report);
    expectError(report.error, ErrorCode::Cancelled, "cancel");
    CHECK(!std::filesystem::exists(
        chunkPath((root / "cancel").string(), 0)));
    std::filesystem::remove_all(root);
}

void testHardKillResume() {
#ifndef _WIN32
    const std::filesystem::path root = uniqueRoot();
    std::filesystem::create_directories(root);
    auto sample = generateSample(77);
    CHECK_OK(sample);
    auto sample_bytes = serializeMesh(*sample, 0, 4);
    CHECK_OK(sample_bytes);
    const std::string sample_path = (root / "sample.mesh").string();
    CHECK_OK(atomicWriteFile(sample_path, *sample_bytes));
    const std::string config_path = (root / "config.cfg").string();
    CHECK_OK(writeDefaultConfig(config_path));

    const std::string cli = MESHREPAIR_CLI_PATH;
    const std::string clean = (root / "clean").string();
    const std::string resumed = (root / "resumed").string();
    const std::string clean_cmd =
        cli + " repair --input " + sample_path + " --config " + config_path +
        " --out " + clean + " >/dev/null";
    const int clean_status = std::system(clean_cmd.c_str());
    CHECK(WIFEXITED(clean_status) && WEXITSTATUS(clean_status) == 0);

    const std::string kill_cmd =
        cli + " repair --input " + sample_path + " --config " + config_path +
        " --out " + resumed + " --stop-after 2 >/dev/null";
    const int kill_status = std::system(kill_cmd.c_str());
    CHECK(WIFEXITED(kill_status) && WEXITSTATUS(kill_status) == 137);
    CHECK(!std::filesystem::exists(
        std::filesystem::path(resumed) / "repaired.mesh"));
    CHECK(std::filesystem::exists(
        std::filesystem::path(resumed) / "chunks" / "chunk_000000.mrchunk"));

    const std::string resume_cmd =
        cli + " repair --input " + sample_path + " --config " + config_path +
        " --out " + resumed + " >/dev/null";
    const int resume_status = std::system(resume_cmd.c_str());
    CHECK(WIFEXITED(resume_status) && WEXITSTATUS(resume_status) == 0);

    auto clean_mesh = readFileBytes((std::filesystem::path(clean) / "repaired.mesh").string());
    auto resumed_mesh =
        readFileBytes((std::filesystem::path(resumed) / "repaired.mesh").string());
    auto clean_trace = readFileBytes((std::filesystem::path(clean) / "trace.log").string());
    auto resumed_trace =
        readFileBytes((std::filesystem::path(resumed) / "trace.log").string());
    CHECK_OK(clean_mesh);
    CHECK_OK(resumed_mesh);
    CHECK_OK(clean_trace);
    CHECK_OK(resumed_trace);
    CHECK(clean_mesh && resumed_mesh && *clean_mesh == *resumed_mesh);
    CHECK(clean_trace && resumed_trace && *clean_trace == *resumed_trace);
    std::filesystem::remove_all(root);
#endif
}

void testSampleDeterminism() {
    auto a = generateSample(123);
    auto b = generateSample(123);
    auto c = generateSample(124);
    CHECK_OK(a);
    CHECK_OK(b);
    CHECK_OK(c);
    auto ea = serializeMesh(*a, 0, 4);
    auto eb = serializeMesh(*b, 0, 4);
    auto ec = serializeMesh(*c, 0, 4);
    CHECK_OK(ea);
    CHECK_OK(eb);
    CHECK_OK(ec);
    CHECK(ea && eb && *ea == *eb);
    CHECK(ec && ea && *ec != *ea);
}

}  // namespace

int main() {
    testHash();
    testMeshValidation();
    testConfigValidation();
    testTopologyAndManifest();
    testRepairAndDeterminism();
    testBoundaryGates();
    testDuplicateFillInterception();
    testBudgetAndLocking();
    testRecoveryValidation();
    testCancellation();
    testHardKillResume();
    testSampleDeterminism();

    std::cout << "checks=" << g_checks << " failures=" << g_failures << '\n';
    return g_failures == 0 ? 0 : 1;
}
