#include "preview/scene_io.hpp"

#include <algorithm>
#include <array>
#include <cmath>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <functional>
#include <limits>
#include <unordered_set>

namespace preview {

namespace {

constexpr std::array<std::uint8_t, 8> kSceneMagic = {
    'Z', 'S', 'P', 'V', 'S', 'C', 'N', '1'
};

constexpr std::uint32_t kMaxBlocks = 1'000'000u;
constexpr std::uint32_t kMaxChildren = 1'000'000u;
constexpr std::uint32_t kMaxVertices = 16'000'000u;
constexpr std::uint32_t kMaxIndices = 48'000'000u;

class ByteReader {
public:
    explicit ByteReader(const std::vector<std::uint8_t>& bytes) : bytes_(bytes) {}

    bool readU32(std::uint32_t* value) {
        if (!has(4)) {
            return false;
        }
        *value = static_cast<std::uint32_t>(bytes_[position_]) |
                 (static_cast<std::uint32_t>(bytes_[position_ + 1]) << 8u) |
                 (static_cast<std::uint32_t>(bytes_[position_ + 2]) << 16u) |
                 (static_cast<std::uint32_t>(bytes_[position_ + 3]) << 24u);
        position_ += 4;
        return true;
    }

    bool readU64(std::uint64_t* value) {
        std::uint32_t low = 0;
        std::uint32_t high = 0;
        if (!readU32(&low) || !readU32(&high)) {
            return false;
        }
        *value = static_cast<std::uint64_t>(low) |
                 (static_cast<std::uint64_t>(high) << 32u);
        return true;
    }

    bool readFloat(float* value) {
        std::uint32_t bits = 0;
        if (!readU32(&bits)) {
            return false;
        }
        std::memcpy(value, &bits, sizeof(bits));
        return true;
    }

    bool readBytes(std::uint8_t* destination, std::size_t size) {
        if (!has(size)) {
            return false;
        }
        std::memcpy(destination, bytes_.data() + position_, size);
        position_ += size;
        return true;
    }

    bool skip(std::size_t size) {
        if (!has(size)) {
            return false;
        }
        position_ += size;
        return true;
    }

    std::size_t offset() const noexcept { return position_; }
    std::size_t remaining() const noexcept { return bytes_.size() - position_; }

private:
    bool has(std::size_t size) const noexcept {
        return size <= bytes_.size() - position_;
    }

    const std::vector<std::uint8_t>& bytes_;
    std::size_t position_ = 0;
};

class ByteWriter {
public:
    void writeU32(std::uint32_t value) {
        bytes_.push_back(static_cast<std::uint8_t>(value & 0xffu));
        bytes_.push_back(static_cast<std::uint8_t>((value >> 8u) & 0xffu));
        bytes_.push_back(static_cast<std::uint8_t>((value >> 16u) & 0xffu));
        bytes_.push_back(static_cast<std::uint8_t>((value >> 24u) & 0xffu));
    }

    void writeU64(std::uint64_t value) {
        writeU32(static_cast<std::uint32_t>(value & 0xffffffffu));
        writeU32(static_cast<std::uint32_t>(value >> 32u));
    }

    void writeFloat(float value) {
        std::uint32_t bits = 0;
        std::memcpy(&bits, &value, sizeof(bits));
        writeU32(bits);
    }

    void writeBytes(const std::uint8_t* data, std::size_t size) {
        bytes_.insert(bytes_.end(), data, data + size);
    }

    std::vector<std::uint8_t>& bytes() noexcept { return bytes_; }
    const std::vector<std::uint8_t>& bytes() const noexcept { return bytes_; }

private:
    std::vector<std::uint8_t> bytes_;
};

Diagnostic invalid(std::string field, std::string message, BlockId blockId,
                   std::size_t offset) {
    return makeDiagnostic(StatusCode::E_SCENE_INVALID, std::move(field),
                          std::move(message), blockId, offset);
}

bool finiteBounds(const Bounds3& bounds) {
    return std::isfinite(bounds.min.x) && std::isfinite(bounds.min.y) &&
           std::isfinite(bounds.min.z) && std::isfinite(bounds.max.x) &&
           std::isfinite(bounds.max.y) && std::isfinite(bounds.max.z) &&
           bounds.min.x <= bounds.max.x && bounds.min.y <= bounds.max.y &&
           bounds.min.z <= bounds.max.z;
}

bool validateReferences(const std::vector<BlockId>& roots,
                        std::vector<SceneBlock>& blocks,
                        const std::unordered_map<BlockId, std::size_t>& index,
                        Diagnostic* diagnostic) {
    std::unordered_set<BlockId> rootSet;
    for (BlockId root : roots) {
        if (index.find(root) == index.end()) {
            *diagnostic = invalid("roots", "root block reference is dangling",
                                  root, 0);
            return false;
        }
        if (!rootSet.insert(root).second) {
            *diagnostic = invalid("roots", "duplicate root reference", root, 0);
            return false;
        }
    }

    for (const SceneBlock& block : blocks) {
        if (block.parent == kInvalidBlockId) {
            if (rootSet.find(block.id) == rootSet.end()) {
                *diagnostic = invalid("blocks[" + std::to_string(block.id) + "].parent",
                                      "root block is missing from root table",
                                      block.id, 0);
                return false;
            }
        } else {
            const auto parentIt = index.find(block.parent);
            if (parentIt == index.end()) {
                *diagnostic = invalid("blocks[" + std::to_string(block.id) + "].parent",
                                      "parent block reference is dangling",
                                      block.id, 0);
                return false;
            }
            if (block.parent == block.id) {
                *diagnostic = invalid("blocks[" + std::to_string(block.id) + "].parent",
                                      "block cannot parent itself", block.id, 0);
                return false;
            }
            const SceneBlock& parent = blocks[parentIt->second];
            if (std::find(parent.children.begin(), parent.children.end(), block.id) ==
                parent.children.end()) {
                *diagnostic = invalid(
                    "blocks[" + std::to_string(block.id) + "].parent",
                    "parent reference is not present in the parent child table",
                    block.id, 0);
                return false;
            }
            if (rootSet.find(block.id) != rootSet.end()) {
                *diagnostic = invalid("roots", "root block also declares a parent",
                                      block.id, 0);
                return false;
            }
        }
    }

    for (SceneBlock& block : blocks) {
        std::unordered_set<BlockId> childSet;
        for (BlockId child : block.children) {
            if (index.find(child) == index.end()) {
                *diagnostic = invalid(
                    "blocks[" + std::to_string(block.id) + "].children",
                    "child block reference is dangling", block.id, 0);
                return false;
            }
            if (!childSet.insert(child).second) {
                *diagnostic = invalid(
                    "blocks[" + std::to_string(block.id) + "].children",
                    "duplicate child reference", block.id, 0);
                return false;
            }
            if (blocks[index.at(child)].parent != block.id) {
                *diagnostic = invalid(
                    "blocks[" + std::to_string(block.id) + "].children",
                    "child parent reference does not point back to this block",
                    block.id, 0);
                return false;
            }
        }
    }

    std::vector<std::uint8_t> state(blocks.size(), 0);
    std::function<bool(std::size_t)> visit = [&](std::size_t blockIndex) -> bool {
        if (state[blockIndex] == 1) {
            *diagnostic = invalid(
                "blocks[" + std::to_string(blocks[blockIndex].id) + "].children",
                "hierarchy cycle detected", blocks[blockIndex].id, 0);
            return false;
        }
        if (state[blockIndex] == 2) {
            return true;
        }
        state[blockIndex] = 1;
        for (BlockId child : blocks[blockIndex].children) {
            if (!visit(index.at(child))) {
                return false;
            }
        }
        state[blockIndex] = 2;
        return true;
    };

    for (std::size_t i = 0; i < blocks.size(); ++i) {
        if (!visit(i)) {
            return false;
        }
    }

    for (SceneBlock& block : blocks) {
        for (BlockId childId : block.children) {
            const SceneBlock& child = blocks[index.at(childId)];
            const double tolerance =
                std::max(1.0, block.bounds.maxExtent()) * 1e-5;
            if (!block.bounds.contains(child.bounds, tolerance)) {
                *diagnostic = invalid(
                    "blocks[" + std::to_string(child.id) + "].bounds",
                    "parent block bounds do not contain child block bounds (parent " +
                        std::to_string(block.id) + ")",
                    child.id, 0);
                return false;
            }
        }
        if (std::find(roots.begin(), roots.end(), block.id) != roots.end()) {
            continue;
        }
        if (block.parent == kInvalidBlockId) {
            *diagnostic = invalid("blocks[" + std::to_string(block.id) + "].parent",
                                  "non-root block has no parent", block.id, 0);
            return false;
        }
    }

    return true;
}

}  // namespace

SceneLoadResult parseSceneBinary(const std::vector<std::uint8_t>& bytes) {
    SceneLoadResult result;
    if (bytes.size() < kSceneHeaderSize) {
        result.diagnostic = invalid("header", "scene file is shorter than header",
                                    kInvalidBlockId, 0);
        return result;
    }
    for (std::size_t i = 0; i < kSceneMagic.size(); ++i) {
        if (bytes[i] != kSceneMagic[i]) {
            result.diagnostic = invalid("header.magic", "invalid scene magic",
                                        kInvalidBlockId, i);
            return result;
        }
    }

    ByteReader reader(bytes);
    if (!reader.skip(kSceneMagic.size())) {
        result.diagnostic = invalid("header.magic", "truncated scene header",
                                    kInvalidBlockId, 0);
        return result;
    }
    std::uint32_t version = 0;
    std::uint32_t flags = 0;
    std::uint32_t blockCount = 0;
    std::uint32_t rootCount = 0;
    std::uint64_t payloadBytes = 0;
    std::uint64_t expectedContentHash = 0;
    if (!reader.readU32(&version) || !reader.readU32(&flags) ||
        !reader.readU32(&blockCount) || !reader.readU32(&rootCount) ||
        !reader.readU64(&payloadBytes) || !reader.readU64(&expectedContentHash)) {
        result.diagnostic = invalid("header", "truncated scene header",
                                    kInvalidBlockId, reader.offset());
        return result;
    }
    if (version != kSceneFormatVersion) {
        result.diagnostic = invalid("header.version", "unsupported scene format version",
                                    kInvalidBlockId, 8);
        return result;
    }
    if (blockCount == 0 || blockCount > kMaxBlocks) {
        result.diagnostic = invalid("header.block_count", "invalid block count",
                                    kInvalidBlockId, 16);
        return result;
    }
    if (rootCount > blockCount) {
        result.diagnostic = invalid("header.root_count", "root count exceeds block count",
                                    kInvalidBlockId, 20);
        return result;
    }
    if (payloadBytes != static_cast<std::uint64_t>(bytes.size() - kSceneHeaderSize)) {
        result.diagnostic = invalid("header.payload_bytes",
                                    "payload length does not match file length",
                                    kInvalidBlockId, 24);
        return result;
    }

    std::vector<BlockId> roots;
    roots.reserve(rootCount);
    for (std::uint32_t i = 0; i < rootCount; ++i) {
        std::uint32_t root = 0;
        if (!reader.readU32(&root)) {
            result.diagnostic = invalid("roots", "truncated root table",
                                        kInvalidBlockId, reader.offset());
            return result;
        }
        roots.push_back(root);
    }

    std::vector<SceneBlock> blocks;
    blocks.reserve(blockCount);
    std::unordered_map<BlockId, std::size_t> index;
    for (std::uint32_t blockOrdinal = 0; blockOrdinal < blockCount; ++blockOrdinal) {
        const std::size_t blockStart = reader.offset();
        SceneBlock block;
        std::uint32_t parent = 0;
        std::array<float, 3> minValues{};
        std::array<float, 3> maxValues{};
        std::uint32_t occluder = 0;
        std::uint32_t childCount = 0;
        std::uint32_t vertexCount = 0;
        std::uint32_t indexCount = 0;
        if (!reader.readU32(&block.id) || !reader.readU32(&parent) ||
            !reader.readFloat(&minValues[0]) ||
            !reader.readFloat(&minValues[1]) ||
            !reader.readFloat(&minValues[2]) ||
            !reader.readFloat(&maxValues[0]) ||
            !reader.readFloat(&maxValues[1]) ||
            !reader.readFloat(&maxValues[2]) ||
            !reader.readU32(&block.priority) || !reader.readU32(&occluder) ||
            !reader.readU32(&childCount) || !reader.readU32(&vertexCount) ||
            !reader.readU32(&indexCount)) {
            result.diagnostic = invalid(
                "blocks[" + std::to_string(blockOrdinal) + "].header",
                "truncated block header", kInvalidBlockId, blockStart);
            return result;
        }
        block.parent = parent;
        block.bounds.min = {minValues[0], minValues[1], minValues[2]};
        block.bounds.max = {maxValues[0], maxValues[1], maxValues[2]};
        if (block.id == kInvalidBlockId) {
            result.diagnostic = invalid(
                "blocks[" + std::to_string(blockOrdinal) + "].id",
                "block id uses reserved sentinel", block.id, blockStart);
            return result;
        }
        if (index.find(block.id) != index.end()) {
            result.diagnostic = invalid(
                "blocks[" + std::to_string(block.id) + "].id",
                "duplicate block id", block.id, blockStart);
            return result;
        }
        if (!finiteBounds(block.bounds)) {
            result.diagnostic = invalid(
                "blocks[" + std::to_string(block.id) + "].bounds",
                "bounds are not finite or min exceeds max", block.id, blockStart);
            return result;
        }
        if (occluder > 1) {
            result.diagnostic = invalid(
                "blocks[" + std::to_string(block.id) + "].occluder",
                "occluder flag must be zero or one", block.id, reader.offset() - 12);
            return result;
        }
        block.occluder = occluder == 1u;
        if (childCount > kMaxChildren) {
            result.diagnostic = invalid(
                "blocks[" + std::to_string(block.id) + "].child_count",
                "child count exceeds safety limit", block.id, reader.offset() - 12);
            return result;
        }
        if (vertexCount > kMaxVertices) {
            result.diagnostic = invalid(
                "blocks[" + std::to_string(block.id) + "].vertex_count",
                "vertex count exceeds safety limit", block.id, reader.offset() - 8);
            return result;
        }
        if (indexCount > kMaxIndices) {
            result.diagnostic = invalid(
                "blocks[" + std::to_string(block.id) + "].index_count",
                "index count exceeds safety limit", block.id, reader.offset() - 4);
            return result;
        }
        if (indexCount % 3u != 0u) {
            result.diagnostic = invalid(
                "blocks[" + std::to_string(block.id) + "].index_count",
                "index count must be a multiple of three", block.id,
                reader.offset() - 4);
            return result;
        }
        const std::uint64_t requiredBytes =
            static_cast<std::uint64_t>(childCount) * 4u +
            static_cast<std::uint64_t>(vertexCount) * 12u +
            static_cast<std::uint64_t>(indexCount) * 4u;
        if (requiredBytes > reader.remaining()) {
            result.diagnostic = invalid(
                "blocks[" + std::to_string(block.id) + "].payload",
                "block payload is truncated", block.id, reader.offset());
            return result;
        }

        block.children.reserve(childCount);
        for (std::uint32_t i = 0; i < childCount; ++i) {
            std::uint32_t child = 0;
            if (!reader.readU32(&child)) {
                result.diagnostic = invalid(
                    "blocks[" + std::to_string(block.id) + "].children",
                    "truncated child reference table", block.id, reader.offset());
                return result;
            }
            block.children.push_back(child);
        }
        block.vertices.reserve(vertexCount);
        for (std::uint32_t i = 0; i < vertexCount; ++i) {
            SceneVertex vertex;
            if (!reader.readFloat(&vertex.x) || !reader.readFloat(&vertex.y) ||
                !reader.readFloat(&vertex.z)) {
                result.diagnostic = invalid(
                    "blocks[" + std::to_string(block.id) + "].vertices",
                    "truncated vertex buffer", block.id, reader.offset());
                return result;
            }
            if (!std::isfinite(vertex.x) || !std::isfinite(vertex.y) ||
                !std::isfinite(vertex.z)) {
                result.diagnostic = invalid(
                    "blocks[" + std::to_string(block.id) + "].vertices[" +
                        std::to_string(i) + "]",
                    "vertex contains a non-finite component", block.id,
                    reader.offset() - 12);
                return result;
            }
            block.vertices.push_back(vertex);
        }
        block.indices.reserve(indexCount);
        for (std::uint32_t i = 0; i < indexCount; ++i) {
            std::uint32_t indexValue = 0;
            if (!reader.readU32(&indexValue)) {
                result.diagnostic = invalid(
                    "blocks[" + std::to_string(block.id) + "].indices",
                    "truncated index buffer", block.id, reader.offset());
                return result;
            }
            if (indexValue >= vertexCount) {
                result.diagnostic = invalid(
                    "blocks[" + std::to_string(block.id) + "].indices[" +
                        std::to_string(i) + "]",
                    "index is outside the vertex buffer", block.id,
                    reader.offset() - 4);
                return result;
            }
            block.indices.push_back(indexValue);
        }

        index.emplace(block.id, blocks.size());
        blocks.push_back(std::move(block));
    }

    if (reader.remaining() != 0) {
        result.diagnostic = invalid("payload", "trailing bytes after block payload",
                                    kInvalidBlockId, reader.offset());
        return result;
    }

    if (!validateReferences(roots, blocks, index, &result.diagnostic)) {
        return result;
    }

    const std::uint64_t actualContentHash = hashSceneContent(roots, blocks);
    if (actualContentHash != expectedContentHash) {
        result.diagnostic = invalid("header.content_hash",
                                    "scene content hash mismatch",
                                    kInvalidBlockId, 32);
        return result;
    }

    auto scene = std::make_shared<Scene>();
    scene->formatVersion = version;
    scene->flags = flags;
    scene->roots = std::move(roots);
    scene->blocks = std::move(blocks);
    scene->blockIndex = std::move(index);
    scene->contentHash = actualContentHash;
    result.scene = std::move(scene);
    result.diagnostic = makeDiagnostic(StatusCode::E_OK, "", "");
    return result;
}

SceneLoadResult loadSceneFile(const std::string& path) {
    SceneLoadResult result;
    std::ifstream stream(path, std::ios::binary | std::ios::ate);
    if (!stream) {
        result.diagnostic = makeDiagnostic(StatusCode::E_IO, "scene.path",
                                           "failed to open scene file");
        return result;
    }
    const std::streamoff end = stream.tellg();
    if (end < 0 || static_cast<std::uint64_t>(end) >
                       static_cast<std::uint64_t>(std::numeric_limits<std::size_t>::max())) {
        result.diagnostic = makeDiagnostic(StatusCode::E_IO, "scene.path",
                                           "failed to query scene file size");
        return result;
    }
    std::vector<std::uint8_t> bytes(static_cast<std::size_t>(end));
    stream.seekg(0, std::ios::beg);
    if (!bytes.empty() &&
        !stream.read(reinterpret_cast<char*>(bytes.data()),
                     static_cast<std::streamsize>(bytes.size()))) {
        result.diagnostic = makeDiagnostic(StatusCode::E_IO, "scene.path",
                                           "failed to read scene file");
        return result;
    }
    return parseSceneBinary(bytes);
}

bool encodeSceneBinary(const std::vector<BlockId>& roots,
                       const std::vector<SceneBlock>& blocks,
                       std::uint32_t flags,
                       std::vector<std::uint8_t>* output,
                       Diagnostic* diagnostic) {
    if (output == nullptr || blocks.empty() || blocks.size() > kMaxBlocks ||
        roots.size() > blocks.size()) {
        if (diagnostic != nullptr) {
            *diagnostic = invalid("arguments", "invalid scene encode arguments",
                                  kInvalidBlockId, 0);
        }
        return false;
    }

    ByteWriter payload;
    for (BlockId root : roots) {
        payload.writeU32(root);
    }
    for (const SceneBlock& block : blocks) {
        if (block.children.size() > kMaxChildren ||
            block.vertices.size() > kMaxVertices ||
            block.indices.size() > kMaxIndices ||
            block.indices.size() % 3u != 0u) {
            if (diagnostic != nullptr) {
                *diagnostic = invalid(
                    "blocks[" + std::to_string(block.id) + "].payload",
                    "block payload exceeds encode limits", block.id, 0);
            }
            return false;
        }
        payload.writeU32(block.id);
        payload.writeU32(block.parent);
        payload.writeFloat(static_cast<float>(block.bounds.min.x));
        payload.writeFloat(static_cast<float>(block.bounds.min.y));
        payload.writeFloat(static_cast<float>(block.bounds.min.z));
        payload.writeFloat(static_cast<float>(block.bounds.max.x));
        payload.writeFloat(static_cast<float>(block.bounds.max.y));
        payload.writeFloat(static_cast<float>(block.bounds.max.z));
        payload.writeU32(block.priority);
        payload.writeU32(block.occluder ? 1u : 0u);
        payload.writeU32(static_cast<std::uint32_t>(block.children.size()));
        payload.writeU32(static_cast<std::uint32_t>(block.vertices.size()));
        payload.writeU32(static_cast<std::uint32_t>(block.indices.size()));
        for (BlockId child : block.children) {
            payload.writeU32(child);
        }
        for (const SceneVertex& vertex : block.vertices) {
            payload.writeFloat(vertex.x);
            payload.writeFloat(vertex.y);
            payload.writeFloat(vertex.z);
        }
        for (std::uint32_t index : block.indices) {
            payload.writeU32(index);
        }
    }

    ByteWriter file;
    file.writeBytes(kSceneMagic.data(), kSceneMagic.size());
    file.writeU32(kSceneFormatVersion);
    file.writeU32(flags);
    file.writeU32(static_cast<std::uint32_t>(blocks.size()));
    file.writeU32(static_cast<std::uint32_t>(roots.size()));
    file.writeU64(static_cast<std::uint64_t>(payload.bytes().size()));
    file.writeU64(hashSceneContent(roots, blocks));
    file.writeBytes(payload.bytes().data(), payload.bytes().size());
    *output = std::move(file.bytes());
    if (diagnostic != nullptr) {
        *diagnostic = makeDiagnostic(StatusCode::E_OK, "", "");
    }
    return true;
}

bool writeSceneFile(const std::string& path,
                    const std::vector<BlockId>& roots,
                    const std::vector<SceneBlock>& blocks,
                    std::uint32_t flags,
                    Diagnostic* diagnostic) {
    std::vector<std::uint8_t> bytes;
    if (!encodeSceneBinary(roots, blocks, flags, &bytes, diagnostic)) {
        return false;
    }
    const std::string temporary = path + ".tmp";
    {
        std::ofstream stream(temporary, std::ios::binary | std::ios::trunc);
        if (!stream) {
            if (diagnostic != nullptr) {
                *diagnostic = makeDiagnostic(StatusCode::E_IO, "scene.path",
                                             "failed to create scene temporary file");
            }
            return false;
        }
        stream.write(reinterpret_cast<const char*>(bytes.data()),
                     static_cast<std::streamsize>(bytes.size()));
        stream.flush();
        if (!stream) {
            if (diagnostic != nullptr) {
                *diagnostic = makeDiagnostic(StatusCode::E_IO, "scene.path",
                                             "failed to write scene file");
            }
            std::remove(temporary.c_str());
            return false;
        }
    }
    std::error_code error;
    std::filesystem::rename(temporary, path, error);
    if (error) {
        std::error_code removeError;
        std::filesystem::remove(path, removeError);
        std::filesystem::rename(temporary, path, error);
    }
    if (error) {
        if (diagnostic != nullptr) {
            *diagnostic = makeDiagnostic(StatusCode::E_IO, "scene.path",
                                         "failed to atomically replace scene file");
        }
        std::error_code removeError;
        std::filesystem::remove(temporary, removeError);
        return false;
    }
    if (diagnostic != nullptr) {
        *diagnostic = makeDiagnostic(StatusCode::E_OK, "", "");
    }
    return true;
}

}  // namespace preview
