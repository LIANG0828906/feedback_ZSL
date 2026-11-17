#pragma once

#include "render.hpp"
#include "scene.hpp"

#include <cstdint>
#include <filesystem>
#include <map>
#include <optional>
#include <span>
#include <string>
#include <vector>

namespace mosaic {

struct StoredChunk {
    ChunkResult result;
    std::string fileDigest;
    std::string rgbaDigest;
    std::string ownershipDigest;
};

struct SnapshotLoadResult {
    bool snapshotPresent = false;
    bool valid = false;
    bool discarded = false;
    std::string discardReason;
    std::map<std::uint32_t, StoredChunk> chunks;
    std::uint32_t reusedChunks = 0;
    std::uint32_t resumeCount = 0;
};

std::optional<std::vector<std::uint8_t>> readBinaryFile(
    const std::filesystem::path& path,
    std::string& error);

bool atomicWriteFile(
    const std::filesystem::path& path,
    std::span<const std::uint8_t> bytes,
    std::string& error);

bool atomicWriteTextFile(
    const std::filesystem::path& path,
    const std::string& text,
    std::string& error);

std::string bytesDigest(std::span<const std::uint8_t> bytes);
std::string bytesDigest(const std::vector<std::uint8_t>& bytes);

SnapshotLoadResult loadSnapshot(
    const std::filesystem::path& cacheDirectory,
    const Scene& scene,
    const std::string& sceneDigest);

StoredChunk storeChunk(
    const std::filesystem::path& cacheDirectory,
    const std::string& sceneDigest,
    const ChunkResult& chunk,
    std::string& error);

bool writeSnapshot(
    const std::filesystem::path& cacheDirectory,
    const Scene& scene,
    const std::string& sceneDigest,
    const std::map<std::uint32_t, StoredChunk>& chunks,
    const std::string& state,
    std::uint32_t resumeCount,
    std::string& error);

bool writeRecoveryReport(
    const std::filesystem::path& outputDirectory,
    const Scene& scene,
    const std::string& sceneDigest,
    const std::string& outcome,
    const SnapshotLoadResult& loadResult,
    std::uint32_t reusedChunks,
    std::uint32_t recomputedChunks,
    const std::map<std::uint32_t, StoredChunk>& chunks,
    std::string& error);

void discardCache(const std::filesystem::path& cacheDirectory);

std::string chunkFileRelativePath(std::uint32_t chunkIndex);

}  // namespace mosaic
