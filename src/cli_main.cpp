#include "battle/sample.h"
#include "battle/scene.h"
#include "battle/trace.h"

#include <charconv>
#include <cstdint>
#include <iostream>
#include <map>
#include <string>
#include <vector>

namespace {

void print_usage() {
    std::cout
        << "battle_cli - deterministic tactical pick laboratory\n\n"
        << "Usage:\n"
        << "  battle_cli generate --out DIR [--seed N] [--cases N]\n"
        << "  battle_cli run --scene FILE --ops FILE --trace FILE --snapshots DIR\n"
        << "  battle_cli replay --trace FILE --snapshots DIR\n"
        << "  battle_cli inspect --scene FILE\n\n"
        << "All numeric scene, camera and operation evidence is stored as IEEE-754\n"
        << "bit patterns so the same operation sequence yields byte-identical logs.\n";
}

bool parse_u64(const std::string& text, std::uint64_t& value) {
    if (text.empty()) {
        return false;
    }
    const auto result = std::from_chars(
        text.data(), text.data() + text.size(), value, 10);
    return result.ec == std::errc() &&
           result.ptr == text.data() + text.size();
}

struct Arguments {
    std::map<std::string, std::string> values;
    std::vector<std::string> positional;
};

Arguments parse_arguments(int argc, char** argv) {
    Arguments arguments;
    for (int i = 2; i < argc; ++i) {
        const std::string token = argv[i];
        if (token.rfind("--", 0) == 0) {
            if (i + 1 >= argc) {
                arguments.values[token] = "";
                continue;
            }
            arguments.values[token] = argv[++i];
        } else {
            arguments.positional.push_back(token);
        }
    }
    return arguments;
}

bool require_option(const Arguments& arguments, const std::string& name,
                    std::string& value) {
    const auto it = arguments.values.find(name);
    if (it == arguments.values.end() || it->second.empty()) {
        std::cerr << "missing required option " << name << "\n";
        return false;
    }
    value = it->second;
    return true;
}

int fail(const battle::Error& error) {
    std::cerr << battle::status_name(error.code) << ": " << error.message;
    if (!error.injection_point.empty()) {
        std::cerr << " [at " << error.injection_point << "]";
    }
    if (error.entity.kind != battle::EntityKind::None) {
        std::cerr << " [entity " << battle::entity_ref_string(error.entity) << "]";
    }
    std::cerr << "\n";
    return static_cast<int>(error.code);
}

int run_generate(const Arguments& arguments) {
    std::string output;
    if (!require_option(arguments, "--out", output)) {
        return 1;
    }
    std::uint64_t seed = 20261121;
    std::uint64_t cases = 3;
    const auto seed_it = arguments.values.find("--seed");
    if (seed_it != arguments.values.end() &&
        !parse_u64(seed_it->second, seed)) {
        std::cerr << "invalid --seed\n";
        return 1;
    }
    const auto cases_it = arguments.values.find("--cases");
    if (cases_it != arguments.values.end() &&
        (!parse_u64(cases_it->second, cases) || cases > 16)) {
        std::cerr << "invalid --cases\n";
        return 1;
    }
    battle::Outcome<battle::GenerationReport> report =
        battle::generate_samples(seed, output, static_cast<std::uint32_t>(cases));
    if (!report) {
        return fail(report.error());
    }
    std::cout << "generated_cases=" << report.value().cases.size()
              << " seed=" << seed
              << " manifest_hash=" << report.value().manifest_hash << "\n";
    for (const std::string& name : report.value().cases) {
        std::cout << "  " << output << "/" << name << "\n";
    }
    return 0;
}

int run_experiment_command(const Arguments& arguments) {
    std::string scene;
    std::string operations;
    std::string trace;
    std::string snapshots;
    if (!require_option(arguments, "--scene", scene) ||
        !require_option(arguments, "--ops", operations) ||
        !require_option(arguments, "--trace", trace) ||
        !require_option(arguments, "--snapshots", snapshots)) {
        return 1;
    }
    battle::Outcome<battle::RunReport> report =
        battle::run_experiment(scene, operations, trace, snapshots);
    if (!report) {
        return fail(report.error());
    }
    std::cout << "final_version=" << report.value().final_version
              << " records=" << report.value().records
              << " trace_chain=" << report.value().final_trace_hash << "\n";
    return 0;
}

int run_replay_command(const Arguments& arguments) {
    std::string trace;
    std::string snapshots;
    if (!require_option(arguments, "--trace", trace) ||
        !require_option(arguments, "--snapshots", snapshots)) {
        return 1;
    }
    battle::Outcome<battle::ReplayReport> report =
        battle::replay_trace_files(trace, snapshots);
    if (!report) {
        return fail(report.error());
    }
    std::cout << "replayed_records=" << report.value().records
              << " final_hash=" << report.value().final_hash << "\n";
    return 0;
}

int run_inspect_command(const Arguments& arguments) {
    std::string scene_path;
    if (!require_option(arguments, "--scene", scene_path)) {
        return 1;
    }
    battle::Outcome<battle::Scene> scene = battle::Scene::load_binary(scene_path);
    if (!scene) {
        return fail(scene.error());
    }
    battle::RegionState state = battle::RegionState::from_scene(scene.value());
    std::cout << "grid=" << scene.value().grid.width << "x"
              << scene.value().grid.levels << "x"
              << scene.value().grid.depth
              << " units=" << scene.value().units.size()
              << " covers=" << scene.value().covers.size()
              << " version=" << state.version
              << " hash=" << state.content_hash << "\n";
    return 0;
}

}  // namespace

int main(int argc, char** argv) {
    if (argc < 2) {
        print_usage();
        return 0;
    }
    const std::string command = argv[1];
    if (command == "--help" || command == "-h" || command == "help") {
        print_usage();
        return 0;
    }
    const Arguments arguments = parse_arguments(argc, argv);
    if (command == "generate") {
        return run_generate(arguments);
    }
    if (command == "run") {
        return run_experiment_command(arguments);
    }
    if (command == "replay") {
        return run_replay_command(arguments);
    }
    if (command == "inspect") {
        return run_inspect_command(arguments);
    }
    std::cerr << "unknown command: " << command << "\n";
    print_usage();
    return 1;
}
