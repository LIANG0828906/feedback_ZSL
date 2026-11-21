#include "preview/scene.hpp"

#include <algorithm>
#include <cstring>

namespace preview {

namespace {

void hashFloat(HashBuilder& hash, float value) noexcept {
    std::uint32_t bits = 0;
    static_assert(sizeof(bits) == sizeof(value), "Unexpected float width");
    std::memcpy(&bits, &value, sizeof(bits));
    hash.addPod(bits);
}

void hashBounds(HashBuilder& hash, const Bounds3& bounds) noexcept {
    hashFloat(hash, static_cast<float>(bounds.min.x));
    hashFloat(hash, static_cast<float>(bounds.min.y));
    hashFloat(hash, static_cast<float>(bounds.min.z));
    hashFloat(hash, static_cast<float>(bounds.max.x));
    hashFloat(hash, static_cast<float>(bounds.max.y));
    hashFloat(hash, static_cast<float>(bounds.max.z));
}

}  // namespace

std::uint64_t SceneBlock::contentHash() const noexcept {
    HashBuilder hash;
    hash.addPod(id);
    hash.addPod(parent);
    hashBounds(hash, bounds);
    hash.addPod(priority);
    const std::uint8_t occluderByte = occluder ? 1u : 0u;
    hash.addPod(occluderByte);
    hash.addPod(static_cast<std::uint64_t>(children.size()));
    for (BlockId child : children) {
        hash.addPod(child);
    }
    hash.addPod(static_cast<std::uint64_t>(vertices.size()));
    for (const SceneVertex& vertex : vertices) {
        hashFloat(hash, vertex.x);
        hashFloat(hash, vertex.y);
        hashFloat(hash, vertex.z);
    }
    hash.addPod(static_cast<std::uint64_t>(indices.size()));
    for (std::uint32_t index : indices) {
        hash.addPod(index);
    }
    return hash.value();
}

const SceneBlock* Scene::findBlock(BlockId id) const noexcept {
    const auto found = blockIndex.find(id);
    if (found == blockIndex.end()) {
        return nullptr;
    }
    return &blocks[found->second];
}

std::uint64_t hashSceneContent(const std::vector<BlockId>& roots,
                               const std::vector<SceneBlock>& blocks) noexcept {
    HashBuilder hash;
    hash.addPod(static_cast<std::uint64_t>(roots.size()));
    for (BlockId root : roots) {
        hash.addPod(root);
    }
    std::vector<std::uint64_t> blockHashes;
    blockHashes.reserve(blocks.size());
    for (const SceneBlock& block : blocks) {
        blockHashes.push_back(block.contentHash());
    }
    std::sort(blockHashes.begin(), blockHashes.end());
    hash.addPod(static_cast<std::uint64_t>(blockHashes.size()));
    for (std::uint64_t blockHash : blockHashes) {
        hash.addPod(blockHash);
    }
    return hash.value();
}

std::uint64_t combineBlockHashes(const std::vector<std::uint64_t>& sortedHashes) noexcept {
    HashBuilder hash;
    hash.addPod(static_cast<std::uint64_t>(sortedHashes.size()));
    for (std::uint64_t value : sortedHashes) {
        hash.addPod(value);
    }
    return hash.value();
}

}  // namespace preview
