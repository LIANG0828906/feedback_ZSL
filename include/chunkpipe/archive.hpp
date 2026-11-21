#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "chunkpipe/error.hpp"
#include "chunkpipe/model.hpp"

namespace chunkpipe {

struct ArchiveReadOptions {
    bool rejectTrailingBytes = true;
};

Result<SceneArchive> readArchiveFromFile(const std::filesystem::path& path,
                                         const ArchiveReadOptions& options = {});
Result<SceneArchive> readArchiveFromBytes(const std::vector<std::uint8_t>& bytes,
                                          const ArchiveReadOptions& options = {});

Result<std::vector<std::uint8_t>> serializeArchive(const SceneArchive& scene,
                                                   std::uint16_t targetVersion);
Result<void> writeArchiveToFile(const std::filesystem::path& path,
                                const SceneArchive& scene,
                                std::uint16_t targetVersion);

Result<void> checkVersionLoss(const SceneArchive& scene, std::uint16_t targetVersion);
Result<void> validateScene(const SceneArchive& scene);

std::uint32_t derivePriority(const Aabb& bounds);
std::string archiveVersionName(std::uint16_t version);

} // namespace chunkpipe

