#pragma once

#include "battle/trace.h"

#include <cstdint>
#include <string>
#include <vector>

namespace battle {

struct RunReport {
    std::uint64_t final_version = 0;
    std::uint64_t records = 0;
    std::string final_trace_hash;
};

struct GenerationReport {
    std::vector<std::string> cases;
    std::string manifest_hash;
};

Outcome<RunReport> run_experiment(const std::string& scene_path,
                                  const std::string& operations_path,
                                  const std::string& trace_path,
                                  const std::string& snapshot_directory);

Outcome<GenerationReport> generate_samples(std::uint64_t seed,
                                           const std::string& output_directory,
                                           std::uint32_t case_count);

}  // namespace battle

