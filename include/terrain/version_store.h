#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "terrain/status.h"
#include "terrain/types.h"

namespace terrain {

enum class CommitFault {
    None,
    BeforeSnapshotRename,
    AfterHeightSnapshot,
    BeforeManifest
};

struct VersionManifest {
    std::uint64_t version = 0;
    std::uint64_t patch_sequence = 0;
    std::string patch_hash;
    std::string height_file;
    std::string height_hash;
    std::string material_file;
    std::string material_hash;
    std::string prev_manifest_hash;
    std::string content_chain;
    std::vector<std::uint64_t> invalid_blocks;
    bool fallback_full = false;
    std::string manifest_hash;
};

class VersionStore {
public:
    explicit VersionStore(std::string root);

    Status initializeFromFiles(const std::string& base_height,
                               const std::string& base_material);
    Status loadCurrent(HeightField* height, MaterialImage* material,
                       VersionManifest* manifest);
    Status loadManifest(VersionManifest* manifest) const;
    Status commit(const HeightField& height, const MaterialImage& material,
                  const CommitMetadata& metadata,
                  CommitFault fault = CommitFault::None);
    Status verifyChain();
    std::uint64_t currentVersion() const { return current_version_; }
    bool initialized() const { return initialized_; }
    const std::string& root() const { return root_; }

    static CommitFault faultFromEnvironment();

private:
    std::string versionsDirectory() const;
    std::string manifestsDirectory() const;
    std::string currentManifestPath() const;
    std::string snapshotPath(std::uint64_t version,
                             const char* suffix) const;
    std::string manifestPath(std::uint64_t version) const;

    Status parseManifest(const std::string& text,
                         VersionManifest* manifest) const;
    std::string serializeManifest(const VersionManifest& manifest) const;
    Status verifyManifestFiles(const VersionManifest& manifest,
                               HeightField* height,
                               MaterialImage* material) const;
    static std::string computeContentChain(const std::string& previous,
                                           const std::string& patch_hash,
                                           const std::string& height_hash,
                                           const std::string& material_hash,
                                           std::uint64_t sequence);

    std::string root_;
    std::uint64_t current_version_ = 0;
    bool initialized_ = false;
};

}  // namespace terrain

