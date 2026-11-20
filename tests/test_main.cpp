#include "core/Deviation.h"
#include "core/Editing.h"
#include "core/Format.h"
#include "core/Project.h"
#include "core/SampleGenerator.h"
#include "core/TaskGate.h"
#include "core/Topology.h"
#include "core/Welding.h"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cmath>
#include <filesystem>
#include <fstream>
#include <functional>
#include <iostream>
#include <iterator>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

#ifdef _WIN32
#include <process.h>
#else
#include <unistd.h>
#endif

namespace {

using namespace meshedit;

int gFailures = 0;
int gChecks = 0;
std::string gCase;

void check(bool condition, const std::string& expression, int line) {
    ++gChecks;
    if (!condition) {
        ++gFailures;
        std::cerr << "FAIL [" << gCase << "] line " << line << ": " << expression << '\n';
    }
}

#define CHECK(expression) check((expression), #expression, __LINE__)

std::string readBytes(const std::string& path) {
    std::ifstream input(path, std::ios::binary);
    return std::string(std::istreambuf_iterator<char>(input),
                       std::istreambuf_iterator<char>());
}

void writeBytes(const std::string& path, const std::string& bytes) {
    std::ofstream output(path, std::ios::binary | std::ios::trunc);
    output.write(bytes.data(), static_cast<std::streamsize>(bytes.size()));
}

std::filesystem::path makeTempRoot() {
#ifdef _WIN32
    const int pid = _getpid();
#else
    const int pid = static_cast<int>(::getpid());
#endif
    const auto root = std::filesystem::current_path() /
        ("mesheditor_test_" + std::to_string(pid));
    std::error_code error;
    std::filesystem::remove_all(root, error);
    std::filesystem::create_directories(root);
    return root;
}

int currentProcessId() {
#ifdef _WIN32
    return _getpid();
#else
    return static_cast<int>(::getpid());
#endif
}

Mesh triangleMesh() {
    Mesh mesh;
    mesh.name = "triangle_pair";
    mesh.positions = {
        {0, 0, 0}, {1, 0, 0}, {0, 1, 0},
        {0, 0, 1}, {0, 1, 0}, {0, 0, 0}};
    mesh.texcoords = {{0, 0}, {1, 0}, {0, 1}, {0.5, 0.5}};
    mesh.groups = {"front", "back"};
    Face first;
    first.corners = {Corner{0, 0, -1}, Corner{1, 1, -1}, Corner{2, 2, -1}};
    first.group = "front";
    first.material = "mat_a";
    Face second;
    second.corners = {Corner{5, 3, -1}, Corner{4, 1, -1}, Corner{3, 2, -1}};
    second.group = "back";
    second.material = "mat_b";
    mesh.faces = {first, second};
    recomputeNormals(mesh);
    return mesh;
}

void testObjParsing(const std::filesystem::path& root) {
    gCase = "obj-parsing";
    const std::string good = (root / "good.obj").string();
    writeBytes(good,
        "# mesh-editor-version 2\n"
        "o sample\n"
        "v 0 0 0\n"
        "v 1 0 0\n"
        "v 0 1 0\n"
        "vt 0 0\n"
        "vt 1 0\n"
        "vt 0 1\n"
        "vn 0 0 1\n"
        "g surface\n"
        "usemtl red\n"
        "f 1/1/1 2/2/1 3/3/1\n");
    Result<LoadResult> loaded = loadMesh(good);
    CHECK(loaded.ok());
    if (loaded.ok()) {
        CHECK(loaded->mesh.positions.size() == 3);
        CHECK(loaded->mesh.texcoords.size() == 3);
        CHECK(loaded->mesh.normals.size() == 1);
        CHECK(loaded->mesh.faces.size() == 1);
        CHECK(loaded->mesh.faces[0].group == "surface");
        CHECK(loaded->mesh.faces[0].material == "red");
        CHECK(loaded->mesh.faces[0].corners[2].texcoord == 2);
    }

    const std::string polygon = (root / "polygon.obj").string();
    writeBytes(polygon,
        "v 0 0 0\n"
        "v 1 0 0\n"
        "v 1 1 0\n"
        "v 0 1 0\n"
        "vt 0 0\n"
        "usemtl red\n"
        "f 1/1 2/1 3/1 4/1\n"
        "usemtl off\n"
        "f 1/1 3/1 2/1\n");
    Result<LoadResult> poly = loadMesh(polygon);
    CHECK(poly.ok());
    if (poly.ok()) {
        CHECK(poly->mesh.faces.size() == 3);
        CHECK(poly->mesh.faces[2].material.empty());
        CHECK(poly->report.toText().find("triangulated 1") != std::string::npos);
    }

    const std::string bad = (root / "bad.obj").string();
    writeBytes(bad,
        "v 0 0 0\n"
        "v 1 0 0\n"
        "v 0 1 0\n"
        "f 1 2 9\n");
    Result<LoadResult> invalid = loadMesh(bad);
    CHECK(!invalid.ok());
    CHECK(invalid.error.code == Code::ParseInvalid);
    CHECK(invalid.error.line == 4);
    CHECK(!invalid.value.has_value());

    const std::string noFace = (root / "noface.obj").string();
    writeBytes(noFace, "v 0 0 0\nv 1 0 0\nv 0 1 0\n");
    Result<LoadResult> noFaces = loadMesh(noFace);
    CHECK(!noFaces.ok());
    CHECK(noFaces.error.code == Code::ParseInvalid);
}

void testStlParsing(const std::filesystem::path& root) {
    gCase = "stl-parsing";
    const std::string ascii = (root / "one.stl").string();
    writeBytes(ascii,
        "solid one\n"
        "  facet normal 0 0 1\n"
        "    outer loop\n"
        "      vertex 0 0 0\n"
        "      vertex 1 0 0\n"
        "      vertex 0 1 0\n"
        "    endloop\n"
        "  endfacet\n"
        "endsolid one\n");
    Result<LoadResult> loaded = loadMesh(ascii);
    CHECK(loaded.ok());
    if (loaded.ok()) {
        CHECK(loaded->mesh.positions.size() == 3);
        CHECK(loaded->mesh.faces.size() == 1);
        CHECK(loaded->mesh.normals.size() == 1);
        CHECK(loaded->mesh.normals[0].z == 1.0);
    }

    const std::string bad = (root / "bad.stl").string();
    writeBytes(bad,
        "solid bad\n"
        "  facet normal 0 0 1\n"
        "    outer loop\n"
        "      vertex 0 0 0\n"
        "      vertex 1 0\n"
        "    endloop\n"
        "  endfacet\n"
        "endsolid bad\n");
    Result<LoadResult> invalid = loadMesh(bad);
    CHECK(!invalid.ok());
    CHECK(invalid.error.code == Code::ParseInvalid);
    CHECK(invalid.error.line == 5);

    const std::string binary = (root / "one_bin.stl").string();
    Result<ExportResult> exported = writeStl(triangleMesh(), binary, StlVariant::Binary);
    CHECK(exported.ok());
    Result<LoadResult> reloaded = loadMesh(binary);
    CHECK(reloaded.ok());
    if (reloaded.ok()) CHECK(reloaded->mesh.faces.size() == 2);

    const std::string truncated = (root / "truncated.stl").string();
    std::string bytes = readBytes(binary);
    writeBytes(truncated, bytes.substr(0, bytes.size() - 3));
    Result<LoadResult> broken = loadMesh(truncated);
    CHECK(!broken.ok());
    CHECK(broken.error.code == Code::ParseInvalid);

    Result<ExportResult> stlLoss = writeStl(triangleMesh(),
        (root / "loss.stl").string(), StlVariant::Ascii);
    CHECK(stlLoss.ok());
    if (stlLoss.ok()) {
        bool hasMaterialLoss = false;
        bool hasUvLoss = false;
        for (const auto& loss : stlLoss->report.losses) {
            hasMaterialLoss = hasMaterialLoss ||
                loss.message.find("material") != std::string::npos;
            hasUvLoss = hasUvLoss ||
                loss.message.find("texture") != std::string::npos;
        }
        CHECK(hasMaterialLoss);
        CHECK(hasUvLoss);
    }

    Mesh normalMesh;
    normalMesh.positions = {{0, 0, 0}, {1, 0, 0}, {0, 1, 0}};
    normalMesh.normals = {{0.2, 0.3, 1.0}};
    normalMesh.faces = {
        Face{{Corner{0, -1, 0}, Corner{1, -1, 0}, Corner{2, -1, 0}}}};
    const std::string normalPath = (root / "normal.stl").string();
    Result<ExportResult> normalExport = writeStl(normalMesh, normalPath,
                                                  StlVariant::Ascii);
    CHECK(normalExport.ok());
    Result<LoadResult> normalLoad = loadMesh(normalPath);
    CHECK(normalLoad.ok());
    if (normalLoad.ok()) {
        CHECK(normalLoad->mesh.normals.size() == 1);
        CHECK(std::fabs(normalLoad->mesh.normals[0].z -
                        meshedit::normalized(normalMesh.normals[0]).z) < 1e-6);
    }
}

void testDeterministicIo(const std::filesystem::path& root) {
    gCase = "deterministic-io";
    const Mesh mesh = triangleMesh();
    const std::string first = (root / "first.obj").string();
    const std::string second = (root / "second.obj").string();
    CHECK(writeObj(mesh, first, Version::V2).ok());
    CHECK(writeObj(mesh, second, Version::V2).ok());
    CHECK(readBytes(first) == readBytes(second));

    const std::string stl1 = (root / "first.stl").string();
    const std::string stl2 = (root / "second.stl").string();
    CHECK(writeStl(mesh, stl1, StlVariant::Binary).ok());
    CHECK(writeStl(mesh, stl2, StlVariant::Binary).ok());
    CHECK(readBytes(stl1) == readBytes(stl2));
}

void testWelding(const std::filesystem::path& root) {
    gCase = "welding";
    (void)root;
    Mesh mesh;
    mesh.positions = {
        {0, 0, 0}, {1, 0, 0}, {0, 1, 0},
        {0, 0, 0}, {1, 0, 0}, {1, 1, 0}};
    mesh.faces = {
        Face{{Corner{0, -1, -1}, Corner{1, -1, -1}, Corner{2, -1, -1}}, "a", "", 1},
        Face{{Corner{3, -1, -1}, Corner{4, -1, -1}, Corner{5, -1, -1}}, "b", "", 2}};
    Result<WeldReport> welded = weldMesh(mesh, WeldOptions{0.0, true});
    CHECK(welded.ok());
    if (welded.ok()) {
        CHECK(welded->oldVertexCount == 6);
        CHECK(welded->newVertexCount == 4);
        CHECK(welded->mergedVertexCount == 2);
        CHECK(welded->vertexMergeMap.size() == 6);
        CHECK(welded->vertexMergeMap[3] == welded->vertexMergeMap[0]);
        CHECK(welded->vertexMergeMap[4] == welded->vertexMergeMap[1]);
        CHECK(welded->affectedFaceCount == 1);
        CHECK(welded->boundaryEdgeCount == 4);
        CHECK(welded->boundaryLoopCount == 1);
    }
    CHECK(mesh.normals.size() == mesh.positions.size());
    const Topology topology = buildTopology(mesh);
    CHECK(topology.boundaryEdges.size() == 4);
    CHECK(topology.nonManifoldEdges.empty());

    Mesh near;
    near.positions = {{0, 0, 0}, {0.001, 0, 0}, {1, 0, 0}};
    near.faces = {Face{{Corner{0, -1, -1}, Corner{1, -1, -1}, Corner{2, -1, -1}}}};
    Result<WeldReport> nearWeld = weldMesh(near, WeldOptions{0.01, true});
    CHECK(nearWeld.ok());
    if (nearWeld.ok()) CHECK(nearWeld->newVertexCount == 2);

    Mesh invalid;
    invalid.positions = {{0, 0, 0}};
    CHECK(!weldMesh(invalid, WeldOptions{-1, true}).ok());
}

void testVersionCompatibility(const std::filesystem::path& root,
                              const std::filesystem::path& samples) {
    gCase = "version-compat";
    Result<LoadResult> legacy = loadMesh((samples / "legacy_v1.obj").string());
    CHECK(legacy.ok());
    if (legacy.ok()) {
        CHECK(legacy->report.upgradedOnLoad);
        CHECK(legacy->report.sourceVersion == 1);
        CHECK(legacy->mesh.sourceVersion == 2);
    }

    Result<LoadResult> seam = loadMesh((samples / "seam_version_loss.obj").string());
    CHECK(seam.ok());
    const std::string out = (root / "loss.obj").string();
    Result<ExportResult> loss = writeObj(seam->mesh, out, Version::V1);
    CHECK(!loss.ok());
    CHECK(loss.error.code == Code::VersionLoss);
    CHECK(std::find(loss.error.affectedGroups.begin(), loss.error.affectedGroups.end(),
                    "seam") != loss.error.affectedGroups.end());
    CHECK(!std::filesystem::exists(out));

    Result<LoadResult> simple = loadMesh((samples / "legacy_v1.obj").string());
    CHECK(simple.ok());
    const std::string v1out = (root / "compatible_v1.obj").string();
    CHECK(writeObj(simple->mesh, v1out, Version::V1).ok());
    Result<LoadResult> reread = loadMesh(v1out);
    CHECK(reread.ok());
    if (reread.ok()) CHECK(reread->report.sourceVersion == 1);
}

void testUndoRedo(const std::filesystem::path& root,
                  const std::filesystem::path& samples) {
    gCase = "undo-redo";
    (void)root;
    Result<LoadResult> loaded = loadMesh((samples / "cube_v2.obj").string());
    CHECK(loaded.ok());
    if (!loaded.ok()) return;
    MeshDocument document;
    document.load(loaded->mesh);
    const MeshMetrics before = document.metrics();
    Selection selection = selectionFromFaces(document.current, {0, 1, 2});
    TransformSpec transform;
    transform.translation = {0.3, 0.2, -0.1};
    transform.rotationAxis = {0, 0, 1};
    transform.rotationDegrees = 30.0;
    transform.scale = {1.2, 0.8, 1.0};
    transform.pivot = {0.5, 0.5, 0.5};
    Result<EditOutcome> outcome = applyTransform(document.current, selection, transform);
    CHECK(outcome.ok());
    if (!outcome.ok()) return;
    CHECK(!metricsEqual(before, outcome->afterMetrics));
    document.commit(outcome.get());
    const MeshMetrics after = document.metrics();
    CHECK(document.history.undoCount() == 1);
    CHECK(document.undo());
    CHECK(metricsEqual(before, document.metrics(), 0.0));
    CHECK(document.history.redoCount() == 1);
    CHECK(document.redo());
    CHECK(metricsEqual(after, document.metrics(), 0.0));
    CHECK(document.history.undoCount() == 1);

    Selection empty;
    CHECK(!applyTransform(document.current, empty, transform).ok());
}

void testSimplification(const std::filesystem::path& root,
                        const std::filesystem::path& samples) {
    gCase = "simplification";
    (void)root;
    Result<LoadResult> loaded = loadMesh((samples / "cube_v2.obj").string());
    CHECK(loaded.ok());
    if (!loaded.ok()) return;
    MeshDocument document;
    document.load(loaded->mesh);
    const MeshMetrics before = document.metrics();
    SimplifyOptions options;
    options.targetRatio = 0.5;
    Result<EditOutcome> simplified = applySimplify(document.current, options);
    CHECK(simplified.ok());
    if (!simplified.ok()) return;
    CHECK(simplified->mesh.faces.size() < before.faceCount);
    CHECK(simplified->mesh.positions.size() < before.vertexCount);
    CHECK(!simplified->mesh.faces.empty());
    CHECK(simplified->affectedFaceCount > 0);
    CHECK(simplified->mesh.normals.size() == simplified->mesh.positions.size());
    document.commit(simplified.get());
    CHECK(document.undo());
    CHECK(metricsEqual(before, document.metrics(), 0.0));

    SimplifyOptions invalid;
    invalid.targetRatio = 1.0;
    CHECK(!applySimplify(document.current, invalid).ok());
}

void testDeviation(const std::filesystem::path& root,
                   const std::filesystem::path& samples) {
    gCase = "deviation";
    Result<LoadResult> loaded = loadMesh((samples / "cube_v2.obj").string());
    CHECK(loaded.ok());
    if (!loaded.ok()) return;
    ValidationConfig config;
    config.distanceTolerance = 1e-9;
    config.samplingDensity = 3;
    Result<ValidationReport> identical = validateMeshes(loaded->mesh, loaded->mesh, config);
    CHECK(identical.ok());
    if (identical.ok()) {
        CHECK(identical->forward.maximum <= 1e-15);
        CHECK(identical->reverse.maximum <= 1e-15);
        CHECK(identical->normalAngleDegrees.maximum <= 1e-9);
        CHECK(identical->passed);
        CHECK(identical->combined.samples > 0);
        CHECK(identical->combined.p95 <= 1e-15);
        CHECK(identical->toJson().find("\"p95\"") != std::string::npos);
    }

    Mesh shifted = loaded->mesh;
    for (Vec3& point : shifted.positions) point.z += 0.05;
    config.distanceTolerance = 1e-4;
    Result<ValidationReport> moved = validateMeshes(loaded->mesh, shifted, config);
    CHECK(moved.ok());
    if (moved.ok()) {
        CHECK(moved->forward.maximum >= 0.049);
        CHECK(!moved->passed);
        CHECK(!moved->violatingCandidateFaces.empty());
        CHECK(!moved->violatingTruthFaces.empty());
    }

    Result<LoadResult> stl = loadMesh((samples / "cube_binary.stl").string());
    CHECK(stl.ok());
    if (stl.ok()) {
        config.distanceTolerance = 1e-6;
        config.normalAngleToleranceDegrees = 1.0;
        Result<ValidationReport> stlReport = validateMeshes(loaded->mesh, stl->mesh, config);
        CHECK(stlReport.ok());
        if (stlReport.ok()) {
            CHECK(stlReport->forward.maximum <= 1e-6);
            CHECK(stlReport->reverse.maximum <= 1e-6);
            CHECK(stlReport->metricsWithinTolerance);
            CHECK(stlReport->passed);
        }
    }
    const std::string reportPath = (root / "validation.json").string();
    if (identical.ok()) {
        CHECK(writeValidationReport(identical.get(), reportPath).ok());
        CHECK(readBytes(reportPath).find("\"normalAngleDegrees\"") != std::string::npos);
    }
}

void testProject(const std::filesystem::path& root,
                 const std::filesystem::path& samples) {
    gCase = "project";
    Result<LoadResult> loaded = loadMesh((samples / "cube_v2.obj").string());
    CHECK(loaded.ok());
    if (!loaded.ok()) return;
    MeshDocument document;
    document.load(loaded->mesh);
    Selection selection = selectionFromFaces(document.current, {0});
    TransformSpec transform;
    transform.translation = {0.04, 0, 0};
    Result<EditOutcome> edit = applyTransform(document.current, selection, transform);
    CHECK(edit.ok());
    if (!edit.ok()) return;
    document.commit(edit.get());
    document.undo();
    document.redo();

    ProjectData project;
    project.current = document.current;
    project.baseline = document.baseline;
    project.undoHistory = document.history.undoRecords();
    project.redoHistory = document.history.redoRecords();
    project.exportVersion = 1;
    project.weldTolerance = 5e-7;
    project.validation.distanceTolerance = 2e-4;
    project.validation.samplingDensity = 6;
    const std::string path = (root / "session.mep").string();
    CHECK(ProjectIo::saveAtomic(path, project).ok());
    CHECK(!std::filesystem::exists(path + ".tmp." +
                                   std::to_string(currentProcessId())));
    Result<ProjectData> restored = ProjectIo::load(path);
    CHECK(restored.ok());
    if (restored.ok()) {
        CHECK(metricsEqual(computeMetrics(restored->current),
                           computeMetrics(project.current), 0.0));
        CHECK(restored->undoHistory.size() == 1);
        CHECK(restored->exportVersion == 1);
        CHECK(restored->weldTolerance == 5e-7);
        CHECK(restored->validation.samplingDensity == 6);
    }

    Mesh activeScene = loaded->mesh;
    const MeshMetrics activeMetrics = computeMetrics(activeScene);
    std::string corruptBytes = readBytes(path);
    CHECK(corruptBytes.size() > 40);
    corruptBytes.back() = static_cast<char>(corruptBytes.back() ^ 0x11);
    const std::string corrupt = (root / "corrupt.mep").string();
    writeBytes(corrupt, corruptBytes);
    Result<ProjectData> rejected = ProjectIo::load(corrupt);
    CHECK(!rejected.ok());
    CHECK(rejected.error.code == Code::ProjectMismatch);
    CHECK(metricsEqual(activeMetrics, computeMetrics(activeScene), 0.0));

    std::string versionBytes = readBytes(path);
    versionBytes[8] = 99;
    const std::string wrongVersion = (root / "wrong_version.mep").string();
    writeBytes(wrongVersion, versionBytes);
    Result<ProjectData> wrong = ProjectIo::load(wrongVersion);
    CHECK(!wrong.ok());
    CHECK(wrong.error.code == Code::ProjectMismatch);
}

void testTaskGate() {
    gCase = "task-gate";
    TaskGate gate;
    CancellationToken token1;
    CancellationToken token2;
    TaskGate::Lease first = gate.acquire(token1);
    CHECK(first.active());
    CHECK(gate.busy());
    CHECK(!gate.tryAcquire());
    TaskGate::Lease duplicate = gate.acquire(token2);
    CHECK(!duplicate.active());
    token1.cancel();
    CHECK(token1.cancelled());
    first.release();
    CHECK(!gate.busy());
    CHECK(gate.tryAcquire());
    gate.release();
    TaskGate::Lease second = gate.acquire(token2);
    CHECK(second.active());
    second.release();

    TaskGate threadedGate;
    CancellationToken heldToken;
    TaskGate::Lease held = threadedGate.acquire(heldToken);
    std::atomic<bool> secondSawBusy{false};
    std::thread worker([&] {
        CancellationToken token;
        auto lease = threadedGate.acquire(token);
        secondSawBusy.store(!lease.active(), std::memory_order_release);
    });
    worker.join();
    CHECK(secondSawBusy.load(std::memory_order_acquire));
    held.release();
}

void testSamples(const std::filesystem::path& root) {
    gCase = "samples";
    const auto first = root / "samples_a";
    const auto second = root / "samples_b";
    Result<SampleManifest> a = generateDeterministicSamples(first.string());
    Result<SampleManifest> b = generateDeterministicSamples(second.string());
    CHECK(a.ok());
    CHECK(b.ok());
    if (a.ok() && b.ok()) {
        CHECK(a->fileCount == b->fileCount);
        CHECK(a->fileCount >= 9);
        CHECK(a->totalBytes == b->totalBytes);
    }
    for (const std::string& name :
         {"cube_v2.obj", "nonmanifold_degenerate.obj", "seam_version_loss.obj",
          "legacy_v1.obj", "large_grid.obj", "cube_ascii.stl", "cube_binary.stl",
          "nonmanifold_degenerate.stl", "degenerate_ascii.stl"}) {
        CHECK(std::filesystem::exists(first / name));
        CHECK(std::filesystem::exists(second / name));
        if (std::filesystem::exists(first / name) &&
            std::filesystem::exists(second / name)) {
            CHECK(readBytes((first / name).string()) ==
                  readBytes((second / name).string()));
        }
    }
}

} // namespace

int main() {
    const auto root = makeTempRoot();
    testSamples(root);
    const auto samples = root / "samples_a";
    testObjParsing(root);
    testStlParsing(root);
    testDeterministicIo(root);
    testWelding(root);
    testVersionCompatibility(root, samples);
    testUndoRedo(root, samples);
    testSimplification(root, samples);
    testDeviation(root, samples);
    testProject(root, samples);
    testTaskGate();

    if (gFailures == 0) {
        std::cout << "PASS " << gChecks << " checks\n";
        std::error_code ignored;
        std::filesystem::remove_all(root, ignored);
        return 0;
    }
    std::cerr << "FAILED " << gFailures << " of " << gChecks << " checks\n";
    std::cerr << "artifacts retained at " << root.string() << '\n';
    return 1;
}
