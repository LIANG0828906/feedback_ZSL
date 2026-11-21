#pragma once

#include <string>
#include <vector>

#include "terrain/status.h"

namespace terrain {

struct SamplePaths {
    std::string root;
    std::string height_field;
    std::string material_image;
    std::string patch_directory;
    std::vector<std::string> height_patches;
    std::vector<std::string> material_patches;
    std::string out_of_bounds_patch;
    std::string stale_patch;
    std::string store_directory;
};

Status generateDeterministicSamples(const std::string& root,
                                    SamplePaths* paths);

}  // namespace terrain

