#include "core/Deviation.h"
#include "core/Editing.h"
#include "core/Format.h"
#include "core/Project.h"
#include "core/SampleGenerator.h"
#include "core/TaskGate.h"
#include "core/Topology.h"
#include "core/Welding.h"

#include <algorithm>
#include <cstdint>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <locale>
#include <map>
#include <set>
#include <sstream>
#include <string>
#include <vector>

namespace {

using namespace meshedit;

std::string jsonEscape(const std::string& text) {
    std::string result;
    for (unsigned char ch : text) {
        switch (ch) {
        case '"': result += "\\\""; break;
        case '\\': result += "\\\\"; break;
        case '\n': result += "\\n"; break;
        case '\r': result += "\\r"; break;
        case '\t': result += "\\t"; break;
        default:
            if (ch < 0x20) {
                static const char* hex = "0123456789abcdef";
                result += "\\u00";
                result.push_back(hex[(ch >> 4) & 0xf]);
                result.push_back(hex[ch & 0xf]);
            } else {
                result.push_back(static_cast<char>(ch));
            }
        }
    }
    return result;
}

std::string errorJson(const Error& error) {
    std::ostringstream out;
    out << "{\"ok\":false,\"code\":\"" << codeName(error.code)
        << "\",\"message\":\"" << jsonEscape(error.message)
        << "\",\"line\":" << error.line << ",\"groups\":[";
    for (std::size_t i = 0; i < error.affectedGroups.size(); ++i) {
        if (i) out << ',';
        out << '"' << jsonEscape(error.affectedGroups[i]) << '"';
    }
    out << "]}";
    return out.str();
}

std::string hashFile(const std::string& path) {
    std::ifstream input(path, std::ios::binary);
    std::uint64_t hash = 1469598103934665603ull;
    char buffer[8192];
    while (input) {
        input.read(buffer, sizeof(buffer));
        const std::streamsize count = input.gcount();
        for (std::streamsize i = 0; i < count; ++i) {
            hash ^= static_cast<unsigned char>(buffer[i]);
            hash *= 1099511628211ull;
        }
    }
    std::ostringstream out;
    out << std::hex << std::setfill('0') << std::setw(16) << hash;
    return out.str();
}

bool hasFlag(const std::vector<std::string>& args, const std::string& flag) {
    return std::find(args.begin(), args.end(), flag) != args.end();
}

std::string option(const std::vector<std::string>& args, const std::string& name,
                   const std::string& fallback = {}) {
    for (std::size_t i = 0; i + 1 < args.size(); ++i) {
        if (args[i] == name) return args[i + 1];
    }
    return fallback;
}

double doubleOption(const std::vector<std::string>& args, const std::string& name,
                    double fallback) {
    const std::string text = option(args, name);
    if (text.empty()) return fallback;
    double value = 0.0;
    return parseDouble(text, value) ? value : fallback;
}

int intOption(const std::vector<std::string>& args, const std::string& name, int fallback) {
    const std::string text = option(args, name);
    if (text.empty()) return fallback;
    int value = 0;
    return parseInteger(text, value) ? value : fallback;
}

StlVariant stlVariant(const std::vector<std::string>& args) {
    return option(args, "--stl", "binary") == "ascii" ? StlVariant::Ascii
                                                     : StlVariant::Binary;
}

void appendEvent(std::ofstream& trace, const std::string& event) {
    trace << event << '\n';
    trace.flush();
}

std::string loadSummary(const LoadResult& loaded) {
    std::ostringstream out;
    out << "{\"positions\":" << loaded.mesh.positions.size()
        << ",\"texcoords\":" << loaded.mesh.texcoords.size()
        << ",\"normals\":" << loaded.mesh.normals.size()
        << ",\"faces\":" << loaded.mesh.faces.size()
        << ",\"groups\":" << loaded.mesh.groups.size()
        << ",\"sourceVersion\":" << loaded.report.sourceVersion
        << ",\"internalVersion\":" << loaded.mesh.sourceVersion
        << ",\"upgraded\":" << (loaded.report.upgradedOnLoad ? "true" : "false")
        << ",\"conversion\":" << loaded.report.toJson() << '}';
    return out.str();
}

int printUsage() {
    std::cout
        << "MeshEditor CLI\n"
        << "  mesheditor_cli generate <out-dir>\n"
        << "  mesheditor_cli info <mesh>\n"
        << "  mesheditor_cli weld <mesh> <out> --tolerance <t> [--version 1|2] [--stl binary|ascii]\n"
        << "  mesheditor_cli convert <mesh> <out> [--version 1|2] [--stl binary|ascii]\n"
        << "  mesheditor_cli validate <truth> <candidate> [--tolerance t] [--density n] [--report file]\n"
        << "  mesheditor_cli project-roundtrip <mesh> <project>\n"
        << "  mesheditor_cli regression <workspace-root>\n";
    return 1;
}

int commandGenerate(const std::vector<std::string>& args) {
    if (args.size() < 3) return printUsage();
    Result<SampleManifest> result = generateDeterministicSamples(args[2]);
    if (!result.ok()) {
        std::cerr << errorJson(result.error) << '\n';
        return 3;
    }
    std::cout << result->toJson() << '\n';
    return 0;
}

int commandInfo(const std::vector<std::string>& args) {
    if (args.size() < 3) return printUsage();
    Result<LoadResult> loaded = loadMesh(args[2]);
    if (!loaded.ok()) {
        std::cout << errorJson(loaded.error) << '\n';
        return 3;
    }
    const Topology topology = buildTopology(loaded->mesh);
    std::cout << "{\"ok\":true,\"metrics\":" << metricsJson(computeMetrics(loaded->mesh))
              << ",\"topology\":" << topologySummaryJson(topology)
              << ",\"load\":" << loadSummary(loaded.get()) << "}\n";
    return 0;
}

ExportOptions exportOptions(const std::string& output,
                            const std::vector<std::string>& args) {
    ExportOptions options;
    options.format = formatFromPath(output);
    options.stlVariant = stlVariant(args);
    const int version = intOption(args, "--version", 2);
    options.version = version == 1 ? Version::V1 : Version::V2;
    return options;
}

int commandConvert(const std::vector<std::string>& args, bool weldFirst) {
    if (args.size() < 4) return printUsage();
    Result<LoadResult> loaded = loadMesh(args[2]);
    if (!loaded.ok()) {
        std::cout << errorJson(loaded.error) << '\n';
        return 3;
    }
    Mesh mesh = std::move(loaded->mesh);
    std::string weldJson = "null";
    if (weldFirst) {
        const double tolerance = doubleOption(args, "--tolerance", 1e-6);
        Result<WeldReport> welded = weldMesh(mesh, WeldOptions{tolerance, true});
        if (!welded.ok()) {
            std::cout << errorJson(welded.error) << '\n';
            return 3;
        }
        weldJson = welded->toJson();
    }
    const ExportOptions options = exportOptions(args[3], args);
    Result<ExportResult> exported = exportMesh(mesh, args[3], options);
    if (!exported.ok()) {
        std::cout << errorJson(exported.error) << '\n';
        return 3;
    }
    std::cout << "{\"ok\":true,\"bytes\":" << exported->bytesWritten
              << ",\"hash\":\"" << hashFile(args[3]) << "\",\"weld\":" << weldJson
              << ",\"report\":" << exported->report.toJson() << "}\n";
    return 0;
}

int commandValidate(const std::vector<std::string>& args) {
    if (args.size() < 4) return printUsage();
    Result<LoadResult> truth = loadMesh(args[2]);
    Result<LoadResult> candidate = loadMesh(args[3]);
    if (!truth.ok()) {
        std::cout << errorJson(truth.error) << '\n';
        return 3;
    }
    if (!candidate.ok()) {
        std::cout << errorJson(candidate.error) << '\n';
        return 3;
    }
    ValidationConfig config;
    config.distanceTolerance = doubleOption(args, "--tolerance", 1e-4);
    config.samplingDensity = intOption(args, "--density", 4);
    config.normalAngleToleranceDegrees =
        doubleOption(args, "--normal-angle", config.normalAngleToleranceDegrees);
    Result<ValidationReport> report = validateMeshes(truth->mesh, candidate->mesh, config);
    if (!report.ok()) {
        std::cout << errorJson(report.error) << '\n';
        return 3;
    }
    const std::string reportPath = option(args, "--report");
    if (!reportPath.empty()) {
        Result<bool> written = writeValidationReport(report.get(), reportPath);
        if (!written.ok()) {
            std::cout << errorJson(written.error) << '\n';
            return 3;
        }
    }
    std::cout << report->toJson();
    return report->passed ? 0 : 4;
}

int commandProjectRoundtrip(const std::vector<std::string>& args) {
    if (args.size() < 4) return printUsage();
    Result<LoadResult> loaded = loadMesh(args[2]);
    if (!loaded.ok()) {
        std::cout << errorJson(loaded.error) << '\n';
        return 3;
    }
    MeshDocument document;
    document.load(std::move(loaded->mesh));
    Selection selection;
    selection.vertices = {0};
    TransformSpec transform;
    transform.translation = {0.1, 0.0, 0.0};
    Result<EditOutcome> edited = applyTransform(document.current, selection, transform);
    if (!edited.ok()) {
        std::cout << errorJson(edited.error) << '\n';
        return 3;
    }
    document.commit(edited.get());
    ProjectData project;
    project.current = document.current;
    project.baseline = document.baseline;
    project.undoHistory = document.history.undoRecords();
    project.redoHistory = document.history.redoRecords();
    project.exportVersion = 1;
    project.weldTolerance = 2e-5;
    project.validation.distanceTolerance = 1e-4;
    project.validation.samplingDensity = 3;
    Result<bool> saved = ProjectIo::saveAtomic(args[3], project);
    if (!saved.ok()) {
        std::cout << errorJson(saved.error) << '\n';
        return 3;
    }
    Result<ProjectData> restored = ProjectIo::load(args[3]);
    if (!restored.ok()) {
        std::cout << errorJson(restored.error) << '\n';
        return 3;
    }
    bool match = metricsEqual(computeMetrics(restored->current), computeMetrics(project.current)) &&
        restored->undoHistory.size() == project.undoHistory.size();
    std::cout << "{\"ok\":" << (match ? "true" : "false")
              << ",\"hash\":\"" << hashFile(args[3])
              << "\",\"undoHistory\":" << restored->undoHistory.size()
              << ",\"exportVersion\":" << restored->exportVersion
              << ",\"weldTolerance\":" << formatDouble(restored->weldTolerance)
              << ",\"validationDensity\":" << restored->validation.samplingDensity
              << "}\n";
    return match ? 0 : 5;
}

int commandRegression(const std::vector<std::string>& args) {
    if (args.size() < 3) return printUsage();
    const std::string root = args[2];
    const std::string samples = root + "/samples";
    const std::string logs = root + "/logs";
    std::error_code ec;
    std::filesystem::create_directories(samples, ec);
    std::filesystem::create_directories(logs, ec);
    if (ec) {
        std::cerr << "cannot create regression directories: " << ec.message() << '\n';
        return 3;
    }
    std::ofstream trace(logs + "/regression_trace.jsonl",
                        std::ios::binary | std::ios::trunc);
    if (!trace) {
        std::cerr << "cannot create regression trace\n";
        return 3;
    }
    bool failed = false;
    auto fail = [&](const std::string& stage, const Error& error) {
        failed = true;
        appendEvent(trace, "{\"event\":\"failure\",\"stage\":\"" +
                     jsonEscape(stage) + "\",\"error\":" + errorJson(error) + "}");
    };

    Result<SampleManifest> manifest = generateDeterministicSamples(samples);
    if (!manifest.ok()) {
        fail("generate", manifest.error);
        return 3;
    }
    appendEvent(trace, "{\"event\":\"generate\",\"files\":" +
                 std::to_string(manifest->fileCount) + ",\"bytes\":" +
                 std::to_string(manifest->totalBytes) + "}");

    const std::vector<std::string> sampleNames = {
        "cube_v2.obj", "nonmanifold_degenerate.obj", "seam_version_loss.obj",
        "legacy_v1.obj", "large_grid.obj", "cube_ascii.stl", "cube_binary.stl",
        "nonmanifold_degenerate.stl", "degenerate_ascii.stl"};
    std::map<std::string, Mesh> parsedMeshes;
    for (const std::string& name : sampleNames) {
        Result<LoadResult> loaded = loadMesh(samples + "/" + name);
        if (!loaded.ok()) {
            fail("parse:" + name, loaded.error);
            continue;
        }
        parsedMeshes.emplace(name, loaded->mesh);
        appendEvent(trace, "{\"event\":\"parse\",\"file\":\"" + name +
                     "\",\"ok\":true,\"load\":" + loadSummary(loaded.get()) + "}");
    }

    if (parsedMeshes.count("nonmanifold_degenerate.obj")) {
        Mesh mesh = parsedMeshes.at("nonmanifold_degenerate.obj");
        const Topology before = buildTopology(mesh);
        Result<WeldReport> welded = weldMesh(mesh, WeldOptions{0.0, true});
        if (!welded.ok()) {
            fail("weld", welded.error);
        } else {
            appendEvent(trace, "{\"event\":\"weld\",\"file\":\"nonmanifold_degenerate.obj\","
                         "\"beforeBoundaryEdges\":" + std::to_string(before.boundaryEdges.size()) +
                         ",\"report\":" + welded->toJson() + "}");
        }
    }

    if (parsedMeshes.count("legacy_v1.obj")) {
        Result<LoadResult> legacy = loadMesh(samples + "/legacy_v1.obj");
        if (!legacy.ok()) {
            fail("legacy-upgrade", legacy.error);
        } else {
            appendEvent(trace, "{\"event\":\"version_upgrade\",\"source\":1,"
                         "\"internal\":" + std::to_string(legacy->mesh.sourceVersion) +
                         ",\"report\":" + legacy->report.toJson() + "}");
        }
    }

    if (parsedMeshes.count("seam_version_loss.obj")) {
        const std::string forbiddenOutput = logs + "/must_not_exist_v1.obj";
        std::error_code ignored;
        std::filesystem::remove(forbiddenOutput, ignored);
        ExportOptions options;
        options.format = MeshFormat::Obj;
        options.version = Version::V1;
        Result<ExportResult> loss = exportMesh(parsedMeshes.at("seam_version_loss.obj"),
                                               forbiddenOutput, options);
        if (loss.ok() || loss.error.code != Code::VersionLoss) {
            fail("version-loss", loss.ok()
                ? Error{Code::Internal, "expected E_VERSION_LOSS", 0, {}}
                : loss.error);
        } else {
            if (std::filesystem::exists(forbiddenOutput)) failed = true;
            appendEvent(trace, "{\"event\":\"version_loss\",\"code\":\"" +
                         std::string(codeName(loss.error.code)) + "\",\"groups\":[" +
                         [&] {
                             std::string groups;
                             for (std::size_t i = 0; i < loss.error.affectedGroups.size(); ++i) {
                                 if (i) groups += ',';
                                 groups += '"' + jsonEscape(loss.error.affectedGroups[i]) + '"';
                             }
                             return groups;
                         }() + "],\"fileCreated\":" +
                         std::string(std::filesystem::exists(forbiddenOutput)
                                         ? "true" : "false") + "}");
        }
    }

    if (parsedMeshes.count("cube_v2.obj")) {
        const Mesh& truth = parsedMeshes.at("cube_v2.obj");
        ExportOptions objOptions;
        objOptions.format = MeshFormat::Obj;
        objOptions.version = Version::V2;
        const std::string exported = logs + "/cube_reloaded.obj";
        Result<ExportResult> objExport = exportMesh(truth, exported, objOptions);
        if (!objExport.ok()) {
            fail("obj-export", objExport.error);
        } else {
            Result<LoadResult> reloaded = loadMesh(exported);
            if (!reloaded.ok()) {
                fail("obj-reload", reloaded.error);
            } else {
                ValidationConfig config;
                config.distanceTolerance = 1e-9;
                config.samplingDensity = 3;
                config.normalAngleToleranceDegrees = 1e-6;
                Result<ValidationReport> report = validateMeshes(truth, reloaded->mesh, config);
                if (!report.ok()) {
                    fail("obj-validate", report.error);
                } else {
                    Result<bool> written =
                        writeValidationReport(report.get(), logs + "/validation_report.json");
                    if (!written.ok()) fail("report-write", written.error);
                    if (!report->passed) failed = true;
                    appendEvent(trace, "{\"event\":\"obj_roundtrip\",\"hash\":\"" +
                                 hashFile(exported) + "\",\"validation\":" +
                                 report->toJson() + "}");
                }
            }
        }
    }

    if (parsedMeshes.count("cube_ascii.stl")) {
        const Mesh& truth = parsedMeshes.at("cube_v2.obj");
        ValidationConfig config;
        config.distanceTolerance = 1e-6;
        config.samplingDensity = 3;
        config.normalAngleToleranceDegrees = 1.0;
        Result<ValidationReport> ascii =
            validateMeshes(truth, parsedMeshes.at("cube_ascii.stl"), config);
        Result<ValidationReport> binary =
            validateMeshes(truth, parsedMeshes.at("cube_binary.stl"), config);
        if (!ascii.ok() || !binary.ok()) {
            fail("stl-validate", ascii.ok() ? binary.error : ascii.error);
        } else {
            if (!ascii->passed || !binary->passed) failed = true;
            appendEvent(trace, "{\"event\":\"stl_roundtrip\",\"ascii\":" +
                         ascii->toJson() + ",\"binary\":" + binary->toJson() + "}");
        }
    }

    if (parsedMeshes.count("cube_v2.obj")) {
        MeshDocument document;
        document.load(parsedMeshes.at("cube_v2.obj"));
        const MeshMetrics before = document.metrics();
        Selection selection = selectionFromFaces(document.current, {0, 1});
        TransformSpec transform;
        transform.translation = {0.25, -0.5, 0.125};
        transform.rotationAxis = {0.0, 0.0, 1.0};
        transform.rotationDegrees = 17.0;
        transform.scale = {1.1, 0.9, 1.0};
        transform.pivot = document.current.positions[0];
        Result<EditOutcome> edited = applyTransform(document.current, selection, transform);
        if (!edited.ok()) {
            fail("transform", edited.error);
        } else {
            document.commit(edited.get());
            const MeshMetrics editedMetrics = document.metrics();
            document.undo();
            const bool exactUndo = metricsEqual(before, document.metrics(), 0.0);
            document.redo();
            const bool exactRedo = metricsEqual(editedMetrics, document.metrics(), 0.0);
            if (!exactUndo || !exactRedo) failed = true;
            appendEvent(trace, "{\"event\":\"edit_undo_redo\",\"exactUndo\":" +
                         std::string(exactUndo ? "true" : "false") +
                         ",\"exactRedo\":" + (exactRedo ? "true" : "false") +
                         ",\"before\":" + metricsJson(before) +
                         ",\"after\":" + metricsJson(editedMetrics) + "}");
        }
    }

    if (parsedMeshes.count("large_grid.obj")) {
        const MeshMetrics before = computeMetrics(parsedMeshes.at("large_grid.obj"));
        SimplifyOptions options;
        options.targetRatio = 0.75;
        Result<EditOutcome> simplified =
            applySimplify(parsedMeshes.at("large_grid.obj"), options);
        if (!simplified.ok()) {
            fail("simplify", simplified.error);
        } else {
            appendEvent(trace, "{\"event\":\"simplify\",\"beforeFaces\":" +
                         std::to_string(before.faceCount) + ",\"afterFaces\":" +
                         std::to_string(simplified->mesh.faces.size()) +
                         ",\"affectedFaces\":" +
                         std::to_string(simplified->affectedFaceCount) + "}");
        }
    }

    {
        ProjectData project;
        if (parsedMeshes.count("cube_v2.obj")) project.current = parsedMeshes.at("cube_v2.obj");
        project.baseline = project.current;
        Selection selection;
        selection.vertices = {0};
        TransformSpec transform;
        transform.translation = {0.5, 0.0, 0.0};
        Result<EditOutcome> edited = applyTransform(project.current, selection, transform);
        if (edited.ok()) {
            EditRecord record;
            record.operation = "transform";
            record.detail = edited->detail;
            record.before = project.current;
            record.after = edited->mesh;
            record.beforeMetrics = edited->beforeMetrics;
            record.afterMetrics = edited->afterMetrics;
            project.current = edited->mesh;
            project.undoHistory.push_back(record);
        }
        project.exportVersion = 2;
        project.weldTolerance = 1e-6;
        project.validation.distanceTolerance = 1e-5;
        project.validation.samplingDensity = 4;
        const std::string projectPath = logs + "/session.mep";
        Result<bool> saved = ProjectIo::saveAtomic(projectPath, project);
        Result<ProjectData> restored = saved.ok() ? ProjectIo::load(projectPath)
                                                 : Result<ProjectData>(saved.error);
        if (!saved.ok() || !restored.ok()) {
            fail("project", saved.ok() ? restored.error : saved.error);
        } else {
            bool match = metricsEqual(computeMetrics(restored->current),
                                      computeMetrics(project.current), 0.0) &&
                restored->undoHistory.size() == project.undoHistory.size() &&
                restored->exportVersion == project.exportVersion;
            std::string corruptHash = "error";
            std::ifstream source(projectPath, std::ios::binary);
            std::vector<unsigned char> bytes((std::istreambuf_iterator<char>(source)),
                                             std::istreambuf_iterator<char>());
            if (bytes.size() > 30) {
                bytes.back() ^= 0x40;
                const std::string corruptPath = logs + "/session_corrupt.mep";
                std::ofstream corrupt(corruptPath, std::ios::binary | std::ios::trunc);
                corrupt.write(reinterpret_cast<const char*>(bytes.data()),
                              static_cast<std::streamsize>(bytes.size()));
                corrupt.close();
                Result<ProjectData> rejected = ProjectIo::load(corruptPath);
                corruptHash = rejected.ok() ? "NOT_REJECTED" : codeName(rejected.error.code);
                if (rejected.ok()) failed = true;
            }
            if (!match) failed = true;
            appendEvent(trace, "{\"event\":\"project_roundtrip\",\"ok\":" +
                         std::string(match ? "true" : "false") +
                         ",\"hash\":\"" + hashFile(projectPath) +
                         "\",\"undoHistory\":" +
                         std::to_string(restored->undoHistory.size()) +
                         ",\"corruptResult\":\"" + corruptHash + "\"}");
        }
    }

    {
        TaskGate gate;
        CancellationToken token1;
        CancellationToken token2;
        TaskGate::Lease lease = gate.acquire(token1);
        TaskGate::Lease duplicate = gate.acquire(token2);
        const bool busyRejected = lease.active() && !duplicate.active() &&
            !token1.cancelled();
        if (!busyRejected) failed = true;
        appendEvent(trace, "{\"event\":\"task_gate\",\"firstAcquired\":" +
                     std::string(lease.active() ? "true" : "false") +
                     ",\"duplicateCode\":\"" +
                     (duplicate.active() ? "E_OK" : "E_BUSY") + "\"}");
        lease.release();
    }

    appendEvent(trace, "{\"event\":\"summary\",\"result\":\"" +
                 std::string(failed ? "FAIL" : "PASS") + "\"}");
    return failed ? 4 : 0;
}

} // namespace

int main(int argc, char** argv) {
    std::locale::global(std::locale::classic());
    std::vector<std::string> args;
    for (int i = 0; i < argc; ++i) args.emplace_back(argv[i]);
    if (args.size() < 2) return printUsage();
    const std::string command = args[1];
    if (command == "generate") return commandGenerate(args);
    if (command == "info") return commandInfo(args);
    if (command == "weld") return commandConvert(args, true);
    if (command == "convert") return commandConvert(args, false);
    if (command == "validate") return commandValidate(args);
    if (command == "project-roundtrip") return commandProjectRoundtrip(args);
    if (command == "regression") return commandRegression(args);
    return printUsage();
}
