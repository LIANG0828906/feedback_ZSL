#pragma once

#include "font_atlas/types.hpp"

#include <filesystem>
#include <string>
#include <vector>

namespace font_atlas {

Result<AtlasManifest> packAtlas(const std::vector<RasterResult>& glyphs,
                                const AtlasOptions& options,
                                const std::filesystem::path& outputDirectory);

std::string atlasManifestJson(const AtlasManifest& manifest);

}  // namespace font_atlas

