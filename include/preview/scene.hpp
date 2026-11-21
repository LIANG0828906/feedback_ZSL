#pragma once

#include "preview/hash.hpp"
#include "preview/math.hpp"

#include <cstdint>
#include <memory>
#include <string>
#include <unordered_map>
#include <vector>

namespace preview {

using BlockId = std::uint32_t;
constexpr BlockId kInvalidBlockId = 0xffffffffu;

struct SceneVertex {
    float x = 0.0f;
    float y = 0.0f;
    float z = 0.0f;
};

struct SceneBlock {
    BlockId id = kInvalidBlockId;
    BlockId parent = kInvalidBlockId;
    Bounds3 bounds;
    std::uint32_t priority = 0;
    bool occluder = false;
    std::vector<BlockId> children;
    std::vector<SceneVertex> vertices;
    std::vector<std::uint32_t> indices;

    std::uint64_t contentHash() const noexcept;
};

struct Scene {
    std::uint32_t formatVersion = 1;
    std::uint32_t flags = 0;
    std::vector<BlockId> roots;
    std::vector<SceneBlock> blocks;
    std::unordered_map<BlockId, std::size_t> blockIndex;
    std::uint64_t contentHash = 0;

    const SceneBlock* findBlock(BlockId id) const noexcept;
};

std::uint64_t hashSceneContent(const std::vector<BlockId>& roots,
                               const std::vector<SceneBlock>& blocks) noexcept;
std::uint64_t combineBlockHashes(const std::vector<std::uint64_t>& sortedHashes) noexcept;

}  // namespace preview
