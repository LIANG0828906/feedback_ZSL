#include "meshrepair/api.hpp"

#include <cstdlib>
#include <filesystem>
#include <iostream>
#include <map>
#include <string>
#include <vector>

namespace {

using meshrepair::Error;
using meshrepair::ErrorCode;
using meshrepair::RepairOptions;
using meshrepair::RepairReport;

struct Arguments {
    std::string command;
    std::map<std::string, std::string> values;
    std::vector<std::string> flags;
};

Arguments parseArguments(int argc, char** argv) {
    Arguments args;
    if (argc > 1) args.command = argv[1];
    for (int i = 2; i < argc; ++i) {
        const std::string token = argv[i];
        if (token.rfind("--", 0) != 0) {
            args.flags.push_back(token);
            continue;
        }
        const std::string key = token.substr(2);
        if (i + 1 < argc && std::string(argv[i + 1]).rfind("--", 0) != 0) {
            args.values[key] = argv[++i];
        } else {
            args.flags.push_back(key);
        }
    }
    return args;
}

bool hasFlag(const Arguments& args, const std::string& name) {
    for (const std::string& flag : args.flags) {
        if (flag == name) return true;
    }
    return false;
}

std::string required(const Arguments& args, const std::string& name) {
    auto found = args.values.find(name);
    if (found == args.values.end()) {
        std::cerr << "error: missing --" << name << '\n';
        std::exit(64);
    }
    return found->second;
}

std::string optional(
    const Arguments& args,
    const std::string& name,
    const std::string& fallback = {}) {
    auto found = args.values.find(name);
    return found == args.values.end() ? fallback : found->second;
}

uint64_t optionalU64(const Arguments& args, const std::string& name, uint64_t fallback) {
    const std::string value = optional(args, name);
    if (value.empty()) return fallback;
    try {
        size_t consumed = 0;
        const unsigned long long parsed = std::stoull(value, &consumed, 10);
        if (consumed != value.size()) throw std::invalid_argument("bad");
        return static_cast<uint64_t>(parsed);
    } catch (...) {
        std::cerr << "error: --" << name << " requires an integer\n";
        std::exit(64);
    }
}

int exitCode(ErrorCode code) {
    switch (code) {
        case ErrorCode::None: return 0;
        case ErrorCode::MeshInvalid: return 10;
        case ErrorCode::ConfigInvalid: return 11;
        case ErrorCode::BoundaryPending: return 12;
        case ErrorCode::TopologyConflict: return 13;
        case ErrorCode::BudgetExceeded: return 14;
        case ErrorCode::RecoveryInvalid: return 15;
        case ErrorCode::Busy: return 16;
        case ErrorCode::IoError: return 17;
        case ErrorCode::Cancelled: return 18;
        case ErrorCode::Internal: return 19;
    }
    return 19;
}

int fail(const Error& error) {
    std::cout << "{\"ok\":false,\"error\":" << meshrepair::errorToJson(error) << "}\n";
    std::cerr << "error=" << meshrepair::errorCodeName(error.code)
              << " message=" << error.message;
    if (error.chunk) std::cerr << " chunk=" << *error.chunk;
    if (error.face) std::cerr << " face=" << *error.face;
    if (error.vertex) std::cerr << " vertex=" << *error.vertex;
    if (error.edge_a) std::cerr << " edge=" << *error.edge_a;
    if (error.edge_b) std::cerr << ":" << *error.edge_b;
    if (error.hole_id) std::cerr << " hole=" << *error.hole_id;
    if (error.estimated_bytes) std::cerr << " estimated=" << *error.estimated_bytes;
    std::cerr << '\n';
    return exitCode(error.code);
}

void printSummary(const RepairReport& report, bool json) {
    if (json) {
        std::cout << meshrepair::reportToJson(report);
        return;
    }
    std::cout << "ok=true\n";
    std::cout << "input_hash=" << report.input_hash << '\n';
    std::cout << "table_hash=" << report.table_hash << '\n';
    std::cout << "boundary_hash=" << report.boundary_hash << '\n';
    std::cout << "stitch_hash=" << report.stitch_hash << '\n';
    std::cout << "original_vertices=" << report.original_vertices << '\n';
    std::cout << "vertices_after_weld=" << report.vertices_after_weld << '\n';
    std::cout << "final_vertices=" << report.final_vertices << '\n';
    std::cout << "original_faces=" << report.original_faces << '\n';
    std::cout << "final_faces=" << report.final_faces << '\n';
    std::cout << "filled_faces=" << report.filled_faces << '\n';
    std::cout << "split_edges=" << report.split_edges << '\n';
    std::cout << "debris_faces_removed=" << report.debris_faces_removed << '\n';
    std::cout << "normal_mean_deg=" << report.normals.mean_deg << '\n';
    std::cout << "normal_max_deg=" << report.normals.max_deg << '\n';
    std::cout << "stitch_valid=" << (report.stitch_valid ? "true" : "false") << '\n';
    std::cout << "resumed=" << (report.resumed ? "true" : "false") << '\n';
    if (report.validation_error) {
        std::cout << "marked_error=" << meshrepair::errorCodeName(
            report.validation_error->code) << '\n';
    }
}

void usage() {
    std::cout
        << "meshrepair_cli commands:\n"
        << "  generate --out sample.mesh [--seed 42] [--config sample.cfg]\n"
        << "  repair --input sample.mesh --config sample.cfg --out run-dir\n"
        << "         [--stop-after N] [--hold-lock-ms N] [--json]\n"
        << "  verify --input sample.mesh --config sample.cfg --out run-dir [--json]\n"
        << "  info --input sample.mesh\n";
}

}  // namespace

int main(int argc, char** argv) {
    const Arguments args = parseArguments(argc, argv);
    if (args.command.empty() || args.command == "help" ||
        hasFlag(args, "help")) {
        usage();
        return args.command.empty() ? 64 : 0;
    }

    if (args.command == "generate") {
        const std::string out = required(args, "out");
        const uint64_t seed = optionalU64(args, "seed", 42);
        auto status = meshrepair::writeSample(out, seed);
        if (!status) return fail(status.error);
        const std::string config = optional(args, "config");
        if (!config.empty()) {
            status = meshrepair::writeDefaultConfig(config);
            if (!status) return fail(status.error);
        }
        std::cout << "ok=true\n";
        std::cout << "sample=" << out << '\n';
        std::cout << "seed=" << seed << '\n';
        if (!config.empty()) std::cout << "config=" << config << '\n';
        return 0;
    }

    if (args.command == "info") {
        auto mesh = meshrepair::loadMesh(required(args, "input"));
        if (!mesh) return fail(mesh.error);
        auto topology = meshrepair::buildTopology(*mesh);
        if (!topology) return fail(topology.error);
        std::cout << "ok=true\n";
        std::cout << "vertices=" << mesh->vertices.size() << '\n';
        std::cout << "faces=" << mesh->faces.size() << '\n';
        std::cout << "directed_edges=" << topology->directed_edges.size() << '\n';
        std::cout << "undirected_edges=" << topology->edge_faces.size() << '\n';
        std::cout << "boundary_edges=" << topology->boundary_edges.size() << '\n';
        std::cout << "nonmanifold_edges=" << topology->nonmanifold_edges.size() << '\n';
        return 0;
    }

    if (args.command == "repair" || args.command == "verify") {
        const std::string input = required(args, "input");
        const std::string config = required(args, "config");
        const std::string out = required(args, "out");
        RepairOptions options;
        options.stop_after_commits =
            static_cast<uint32_t>(optionalU64(args, "stop-after", UINT32_MAX));
        options.hold_lock_ms =
            static_cast<uint32_t>(optionalU64(args, "hold-lock-ms", 0));
        options.verify_only = args.command == "verify" || hasFlag(args, "verify");
        auto report = meshrepair::repairFile(input, config, out, options);
        if (!report) return fail(report.error);
        printSummary(*report, hasFlag(args, "json"));
        if (report->validation_error) {
            return exitCode(report->validation_error->code);
        }
        return 0;
    }

    std::cerr << "error: unknown command " << args.command << '\n';
    usage();
    return 64;
}
