#pragma once

#include <filesystem>
#include <string>
#include <vector>

#include "chunkpipe/error.hpp"
#include "chunkpipe/visibility.hpp"

namespace chunkpipe {

struct GeneratedSamples {
    std::vector<std::filesystem::path> archives;
    std::filesystem::path baselinePath;
    Camera camera;
};

Result<GeneratedSamples> generateDeterministicSamples(const std::filesystem::path& directory);

} // namespace chunkpipe

