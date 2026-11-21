#pragma once

#include <filesystem>
#include <map>
#include <string>
#include <vector>

#include "chunkpipe/error.hpp"
#include "chunkpipe/model.hpp"
#include "chunkpipe/visibility.hpp"

namespace chunkpipe {

struct VisibilityDiff {
    std::uint32_t chunkId = 0;
    std::uint16_t fromVersion = 0;
    std::uint16_t toVersion = 0;
    std::uint8_t fromMask = 0;
    std::uint8_t toMask = 0;
    std::string cause;
    std::vector<std::string> fields;
};

struct CompareReport {
    std::uint32_t sceneId = 0;
    Camera camera;
    std::vector<std::uint16_t> versions;
    std::map<std::uint16_t, std::uint64_t> maskHashes;
    std::map<std::uint16_t, std::uint64_t> snapshotHashes;
    std::map<std::uint16_t, CullStats> stats;
    std::vector<VisibilityDiff> differences;
    std::vector<VisibilityDiff> metadataDifferences;
};

Result<CompareReport> compareArchives(const std::vector<std::filesystem::path>& paths,
                                      const Camera& camera,
                                      std::uint32_t maxLoadedChunks);

std::string compareReportJson(const CompareReport& report);
std::string visibilityDiffJsonLines(const CompareReport& report);

} // namespace chunkpipe

