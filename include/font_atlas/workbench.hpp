#pragma once

#include "font_atlas/engine.hpp"
#include "font_atlas/types.hpp"

#include <filesystem>
#include <string>
#include <vector>

namespace font_atlas {

Result<WorkbenchConfig> loadWorkbenchConfig(const std::filesystem::path& path);
std::string workbenchConfigJson(const WorkbenchConfig& config);
Result<void> saveWorkbenchConfig(const std::filesystem::path& path,
                                 const WorkbenchConfig& config);

struct BatchRasterResult {
    std::vector<RasterResult> glyphs;
    std::vector<Diagnostic> failures;
    AtlasManifest atlas;
};

struct BatchRasterOptions {
    bool resume = false;
    bool continueOnGlyphFailure = false;
    bool persist = true;
    std::uint64_t hardExitAfterCommittedChunks = 0;
};

Result<BatchRasterResult> rasterizeFontToAtlas(
    FontEngine& engine, const WorkbenchConfig& config,
    const std::filesystem::path& outputDirectory,
    const std::filesystem::path& stateDirectory,
    const BatchRasterOptions& options,
    const CancelCheck& cancelled = {},
    const ProgressCallback& progress = {});

Result<void> generateSampleSet(const std::filesystem::path& outputDirectory,
                               std::uint64_t seed);

}  // namespace font_atlas

