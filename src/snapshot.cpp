#include "preview/snapshot.hpp"

#include "preview/hash.hpp"

#include <algorithm>
#include <array>
#include <cmath>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <limits>
#include <sstream>
#include <system_error>
#include <unordered_set>

#if defined(_WIN32)
#ifndef NOMINMAX
#define NOMINMAX
#endif
#include <windows.h>
#endif

namespace preview {

namespace {

constexpr std::array<std::uint8_t, 8> kSnapshotMagic = {
    'Z', 'S', 'P', 'V', 'S', 'N', 'P', '1'
};
constexpr std::size_t kSnapshotHeaderSize = 32;
constexpr std::uint32_t kMaxSnapshotBlocks = 1'000'000u;
constexpr std::uint32_t kMaxSnapshotLayers = 64u;
constexpr std::uint32_t kMaxStringBytes = 1'000'000u;

class Reader {
public:
    explicit Reader(const std::vector<std::uint8_t>& bytes) : bytes_(bytes) {}

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

    bool readI32(std::int32_t* value) {
        std::uint32_t raw = 0;
        if (!readU32(&raw)) {
            return false;
        }
        std::memcpy(value, &raw, sizeof(raw));
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

    bool readDouble(double* value) {
        std::uint64_t raw = 0;
        if (!readU64(&raw)) {
            return false;
        }
        std::memcpy(value, &raw, sizeof(raw));
        return true;
    }

    bool readString(std::string* value) {
        std::uint32_t size = 0;
        if (!readU32(&size) || size > kMaxStringBytes || !has(size)) {
            return false;
        }
        value->assign(reinterpret_cast<const char*>(bytes_.data() + position_),
                      size);
        position_ += size;
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

    std::size_t offset() const noexcept { return position_; }
    std::size_t remaining() const noexcept { return bytes_.size() - position_; }

private:
    bool has(std::size_t size) const noexcept {
        return size <= bytes_.size() - position_;
    }

    const std::vector<std::uint8_t>& bytes_;
    std::size_t position_ = 0;
};

class Writer {
public:
    void writeU32(std::uint32_t value) {
        bytes_.push_back(static_cast<std::uint8_t>(value & 0xffu));
        bytes_.push_back(static_cast<std::uint8_t>((value >> 8u) & 0xffu));
        bytes_.push_back(static_cast<std::uint8_t>((value >> 16u) & 0xffu));
        bytes_.push_back(static_cast<std::uint8_t>((value >> 24u) & 0xffu));
    }

    void writeI32(std::int32_t value) {
        std::uint32_t raw = 0;
        std::memcpy(&raw, &value, sizeof(raw));
        writeU32(raw);
    }

    void writeU64(std::uint64_t value) {
        writeU32(static_cast<std::uint32_t>(value & 0xffffffffu));
        writeU32(static_cast<std::uint32_t>(value >> 32u));
    }

    void writeI64(std::int64_t value) {
        std::uint64_t raw = 0;
        std::memcpy(&raw, &value, sizeof(raw));
        writeU64(raw);
    }

    void writeDouble(double value) {
        std::uint64_t raw = 0;
        std::memcpy(&raw, &value, sizeof(raw));
        writeU64(raw);
    }

    void writeString(const std::string& value) {
        writeU32(static_cast<std::uint32_t>(value.size()));
        bytes_.insert(bytes_.end(), value.begin(), value.end());
    }

    void writeBytes(const std::uint8_t* data, std::size_t size) {
        bytes_.insert(bytes_.end(), data, data + size);
    }

    std::vector<std::uint8_t>& bytes() noexcept { return bytes_; }

private:
    std::vector<std::uint8_t> bytes_;
};

SnapshotLoadResult corrupt(std::string field, std::string message,
                           std::size_t offset = 0) {
    SnapshotLoadResult result;
    result.code = StatusCode::E_SNAPSHOT_CORRUPT;
    result.diagnostic = makeDiagnostic(result.code, std::move(field),
                                       std::move(message), kInvalidBlockId,
                                       offset);
    return result;
}

std::uint64_t loadedContentHash(std::uint64_t sceneContentHash,
                                const std::vector<SnapshotBlockRecord>& blocks) {
    std::vector<SnapshotBlockRecord> sorted = blocks;
    std::sort(sorted.begin(), sorted.end(),
              [](const SnapshotBlockRecord& a, const SnapshotBlockRecord& b) {
                  return a.id < b.id;
              });
    HashBuilder hash;
    hash.addPod(sceneContentHash);
    hash.addPod(static_cast<std::uint64_t>(sorted.size()));
    for (const SnapshotBlockRecord& block : sorted) {
        hash.addPod(block.id);
        hash.addPod(block.contentHash);
    }
    return hash.value();
}

bool sameView(const SnapshotDocument& document, const Camera& camera) {
    if (document.camera.position.x != camera.position.x ||
        document.camera.position.y != camera.position.y ||
        document.camera.position.z != camera.position.z ||
        document.camera.target.x != camera.target.x ||
        document.camera.target.y != camera.target.y ||
        document.camera.target.z != camera.target.z ||
        document.camera.up.x != camera.up.x ||
        document.camera.up.y != camera.up.y ||
        document.camera.up.z != camera.up.z ||
        document.camera.fovYDegrees != camera.fovYDegrees ||
        document.camera.nearPlane != camera.nearPlane ||
        document.camera.farPlane != camera.farPlane ||
        document.camera.width != camera.width ||
        document.camera.height != camera.height) {
        return false;
    }
    const Mat4 current = camera.viewMatrix();
    for (std::size_t i = 0; i < 16; ++i) {
        if (document.viewMatrix.m[i] != current.m[i]) {
            return false;
        }
    }
    return true;
}

bool sameLoadedSet(const std::vector<SnapshotBlockRecord>& blocks,
                   const std::vector<BlockId>& currentIds) {
    std::vector<BlockId> a;
    a.reserve(blocks.size());
    for (const SnapshotBlockRecord& block : blocks) {
        a.push_back(block.id);
    }
    std::vector<BlockId> b = currentIds;
    std::sort(a.begin(), a.end());
    std::sort(b.begin(), b.end());
    a.erase(std::unique(a.begin(), a.end()), a.end());
    b.erase(std::unique(b.begin(), b.end()), b.end());
    return a == b;
}

void writeSummary(Writer* writer, const FrameErrorSummary& summary) {
    writer->writeString(summary.summaryVersion);
    writer->writeU64(summary.snapshotVersion);
    writer->writeU64(summary.pyramidVersion);
    writer->writeU64(summary.loadContentHash);
    writer->writeU64(summary.maskHash);
    writer->writeU64(summary.totalBlocks);
    writer->writeU64(summary.visibleBlocks);
    writer->writeU64(summary.occludedBlocks);
    writer->writeU64(summary.uncertainBlocks);
    writer->writeU64(summary.culledBlocks);
    writer->writeU64(summary.layerSwitches);
    writer->writeU64(summary.criticalBlocks);
    writer->writeDouble(summary.bboxTransformErrorContribution);
    writer->writeDouble(summary.pyramidQuantizationErrorContribution);
    writer->writeDouble(summary.layerSwitchJumpContribution);
    writer->writeDouble(summary.frustumBoundaryErrorContribution);
    writer->writeDouble(summary.occlusionErrorUpperBoundMax);
    writer->writeU64(summary.maskDeviationUpperBoundPixels);
}

bool readSummary(Reader* reader, FrameErrorSummary* summary) {
    return reader->readString(&summary->summaryVersion) &&
           reader->readU64(&summary->snapshotVersion) &&
           reader->readU64(&summary->pyramidVersion) &&
           reader->readU64(&summary->loadContentHash) &&
           reader->readU64(&summary->maskHash) &&
           reader->readU64(&summary->totalBlocks) &&
           reader->readU64(&summary->visibleBlocks) &&
           reader->readU64(&summary->occludedBlocks) &&
           reader->readU64(&summary->uncertainBlocks) &&
           reader->readU64(&summary->culledBlocks) &&
           reader->readU64(&summary->layerSwitches) &&
           reader->readU64(&summary->criticalBlocks) &&
           reader->readDouble(&summary->bboxTransformErrorContribution) &&
           reader->readDouble(&summary->pyramidQuantizationErrorContribution) &&
           reader->readDouble(&summary->layerSwitchJumpContribution) &&
           reader->readDouble(&summary->frustumBoundaryErrorContribution) &&
           reader->readDouble(&summary->occlusionErrorUpperBoundMax) &&
           reader->readU64(&summary->maskDeviationUpperBoundPixels);
}

bool atomicallyReplace(const std::string& temporary, const std::string& target,
                       std::error_code* error) {
#if defined(_WIN32)
    const std::filesystem::path temporaryPath(temporary);
    const std::filesystem::path targetPath(target);
    if (MoveFileExW(temporaryPath.wstring().c_str(),
                    targetPath.wstring().c_str(),
                    MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH) != 0) {
        error->clear();
        return true;
    }
    *error = std::error_code(static_cast<int>(GetLastError()),
                             std::system_category());
    return false;
#else
    std::filesystem::rename(temporary, target, *error);
    return !*error;
#endif
}

}  // namespace

SnapshotCurrentState currentStateFromSnapshot(const LoadSnapshot& snapshot,
                                              const Camera& camera) {
    SnapshotCurrentState state;
    state.camera = camera;
    state.loadedBlockIds = snapshot.ids();
    state.loadContentHash = snapshot.contentHash;
    return state;
}

SnapshotDocument makeSnapshotDocument(const LoadSnapshot& loadSnapshot,
                                      const Camera& camera,
                                      const DepthPyramid& pyramid,
                                      std::uint64_t maskHash,
                                      int maskWidth,
                                      int maskHeight,
                                      const FrameErrorSummary& summary) {
    SnapshotDocument document;
    document.snapshotVersion = loadSnapshot.version;
    document.pyramidVersion = pyramid.version;
    document.camera = camera;
    document.viewMatrix = camera.viewMatrix();
    document.sceneContentHash =
        loadSnapshot.scene ? loadSnapshot.scene->contentHash : 0;
    document.loadContentHash = loadSnapshot.contentHash;
    for (const LoadedBlockRef& block : loadSnapshot.blocks) {
        document.blocks.push_back(
            SnapshotBlockRecord{block.id, block.contentHash});
    }
    for (const DepthPyramidLayer& layer : pyramid.layers) {
        document.layers.push_back(SnapshotLayerRecord{
            layer.level, layer.width, layer.height, layer.quantStep,
            pyramid.key.nearPlane, pyramid.key.quantizedDepthMax});
    }
    document.pyramidContentHash = pyramid.contentHash();
    document.maskHash = maskHash;
    document.maskWidth = maskWidth;
    document.maskHeight = maskHeight;
    document.summary = summary;
    document.summary.snapshotVersion = loadSnapshot.version;
    document.summary.pyramidVersion = pyramid.version;
    document.summary.loadContentHash = loadSnapshot.contentHash;
    document.summary.maskHash = maskHash;
    return document;
}

SnapshotSaveResult saveSnapshotAtomic(const std::string& path,
                                      const SnapshotDocument& document) {
    SnapshotSaveResult result;
    if (path.empty()) {
        result.code = StatusCode::E_INVALID_ARGUMENT;
        result.diagnostic =
            makeDiagnostic(result.code, "snapshot.path", "snapshot path is empty");
        return result;
    }
    const std::vector<std::uint8_t> encoded = encodeSnapshotDocument(document);
    const std::string temporary = path + ".tmp";
    {
        std::ofstream stream(temporary, std::ios::binary | std::ios::trunc);
        if (!stream) {
            result.code = StatusCode::E_IO;
            result.diagnostic = makeDiagnostic(
                result.code, "snapshot.path",
                "failed to create snapshot temporary file");
            return result;
        }
        stream.write(reinterpret_cast<const char*>(encoded.data()),
                     static_cast<std::streamsize>(encoded.size()));
        stream.flush();
        if (!stream) {
            result.code = StatusCode::E_IO;
            result.diagnostic = makeDiagnostic(
                result.code, "snapshot.path", "failed to write snapshot file");
            std::error_code removeError;
            std::filesystem::remove(temporary, removeError);
            return result;
        }
    }
    std::error_code replaceError;
    if (!atomicallyReplace(temporary, path, &replaceError)) {
        result.code = StatusCode::E_IO;
        result.diagnostic = makeDiagnostic(
            result.code, "snapshot.path",
            "failed to atomically replace snapshot file: " +
                replaceError.message());
        std::error_code removeError;
        std::filesystem::remove(temporary, removeError);
        return result;
    }
    result.code = StatusCode::E_OK;
    result.diagnostic = makeDiagnostic(StatusCode::E_OK, "", "");
    return result;
}

SnapshotLoadResult loadSnapshot(const std::string& path,
                                const SnapshotCurrentState& currentState) {
    std::ifstream stream(path, std::ios::binary | std::ios::ate);
    if (!stream) {
        SnapshotLoadResult result;
        result.code = StatusCode::E_IO;
        result.diagnostic = makeDiagnostic(result.code, "snapshot.path",
                                           "failed to open snapshot file");
        return result;
    }
    const std::streamoff end = stream.tellg();
    if (end < 0 || static_cast<std::uint64_t>(end) >
                       static_cast<std::uint64_t>(
                           std::numeric_limits<std::size_t>::max())) {
        SnapshotLoadResult result;
        result.code = StatusCode::E_IO;
        result.diagnostic = makeDiagnostic(result.code, "snapshot.path",
                                           "failed to query snapshot size");
        return result;
    }
    std::vector<std::uint8_t> bytes(static_cast<std::size_t>(end));
    stream.seekg(0, std::ios::beg);
    if (!bytes.empty() &&
        !stream.read(reinterpret_cast<char*>(bytes.data()),
                     static_cast<std::streamsize>(bytes.size()))) {
        SnapshotLoadResult result;
        result.code = StatusCode::E_IO;
        result.diagnostic = makeDiagnostic(result.code, "snapshot.path",
                                           "failed to read snapshot file");
        return result;
    }
    SnapshotLoadResult result = decodeSnapshotDocument(bytes);
    if (!result) {
        return result;
    }

    if (!sameView(result.document, currentState.camera)) {
        result.changes.push_back("view");
    }
    if (!sameLoadedSet(result.document.blocks, currentState.loadedBlockIds)) {
        result.changes.push_back("loaded_set");
    }
    if (result.document.loadContentHash != currentState.loadContentHash) {
        result.changes.push_back("load_content_hash");
    }
    result.requiresRerender = !result.changes.empty();
    return result;
}

std::vector<std::uint8_t> encodeSnapshotDocument(
    const SnapshotDocument& document) {
    Writer payload;
    payload.writeU32(document.formatVersion);
    payload.writeU64(document.snapshotVersion);
    payload.writeU64(document.pyramidVersion);
    payload.writeDouble(document.camera.position.x);
    payload.writeDouble(document.camera.position.y);
    payload.writeDouble(document.camera.position.z);
    payload.writeDouble(document.camera.target.x);
    payload.writeDouble(document.camera.target.y);
    payload.writeDouble(document.camera.target.z);
    payload.writeDouble(document.camera.up.x);
    payload.writeDouble(document.camera.up.y);
    payload.writeDouble(document.camera.up.z);
    payload.writeDouble(document.camera.fovYDegrees);
    payload.writeDouble(document.camera.nearPlane);
    payload.writeDouble(document.camera.farPlane);
    payload.writeI32(document.camera.width);
    payload.writeI32(document.camera.height);
    for (double value : document.viewMatrix.m) {
        payload.writeDouble(value);
    }
    payload.writeU64(document.sceneContentHash);
    payload.writeU64(document.loadContentHash);
    payload.writeU32(static_cast<std::uint32_t>(document.blocks.size()));
    for (const SnapshotBlockRecord& block : document.blocks) {
        payload.writeU32(block.id);
        payload.writeU64(block.contentHash);
    }
    payload.writeU32(static_cast<std::uint32_t>(document.layers.size()));
    for (const SnapshotLayerRecord& layer : document.layers) {
        payload.writeI32(layer.level);
        payload.writeI32(layer.width);
        payload.writeI32(layer.height);
        payload.writeDouble(layer.quantStep);
        payload.writeDouble(layer.depthMin);
        payload.writeDouble(layer.depthMax);
    }
    payload.writeU64(document.pyramidContentHash);
    payload.writeU64(document.maskHash);
    payload.writeI32(document.maskWidth);
    payload.writeI32(document.maskHeight);
    writeSummary(&payload, document.summary);

    Writer file;
    file.writeBytes(kSnapshotMagic.data(), kSnapshotMagic.size());
    file.writeU32(kSnapshotFormatVersion);
    file.writeU32(0);
    file.writeU64(static_cast<std::uint64_t>(payload.bytes().size()));
    file.writeU64(fnv1a64(payload.bytes().data(), payload.bytes().size()));
    file.writeBytes(payload.bytes().data(), payload.bytes().size());
    return std::move(file.bytes());
}

SnapshotLoadResult decodeSnapshotDocument(
    const std::vector<std::uint8_t>& bytes) {
    if (bytes.size() < kSnapshotHeaderSize) {
        return corrupt("header", "snapshot is shorter than header");
    }
    for (std::size_t i = 0; i < kSnapshotMagic.size(); ++i) {
        if (bytes[i] != kSnapshotMagic[i]) {
            return corrupt("header.magic", "invalid snapshot magic", i);
        }
    }
    Reader header(bytes);
    std::uint8_t magic[8];
    if (!header.readBytes(magic, sizeof(magic))) {
        return corrupt("header.magic", "truncated snapshot header");
    }
    std::uint32_t containerVersion = 0;
    std::uint32_t reserved = 0;
    std::uint64_t payloadSize = 0;
    std::uint64_t checksum = 0;
    if (!header.readU32(&containerVersion) || !header.readU32(&reserved) ||
        !header.readU64(&payloadSize) || !header.readU64(&checksum)) {
        return corrupt("header", "truncated snapshot header", header.offset());
    }
    if (containerVersion != kSnapshotFormatVersion) {
        return corrupt("header.version", "unsupported snapshot container version",
                       8);
    }
    if (payloadSize != header.remaining()) {
        return corrupt("header.payload_bytes",
                       "snapshot payload length does not match file length", 16);
    }
    const std::uint64_t actualChecksum =
        fnv1a64(bytes.data() + kSnapshotHeaderSize,
                static_cast<std::size_t>(payloadSize));
    if (actualChecksum != checksum) {
        return corrupt("header.checksum", "snapshot payload checksum mismatch", 24);
    }

    std::vector<std::uint8_t> payloadBytes(
        bytes.begin() + static_cast<std::ptrdiff_t>(kSnapshotHeaderSize),
        bytes.end());
    Reader reader(payloadBytes);
    SnapshotDocument document;
    std::uint32_t payloadVersion = 0;
    if (!reader.readU32(&payloadVersion)) {
        return corrupt("payload.version", "truncated snapshot payload");
    }
    if (payloadVersion != kSnapshotFormatVersion) {
        return corrupt("payload.version", "unsupported snapshot document version",
                       reader.offset() - 4);
    }
    document.formatVersion = payloadVersion;
    if (!reader.readU64(&document.snapshotVersion) ||
        !reader.readU64(&document.pyramidVersion) ||
        !reader.readDouble(&document.camera.position.x) ||
        !reader.readDouble(&document.camera.position.y) ||
        !reader.readDouble(&document.camera.position.z) ||
        !reader.readDouble(&document.camera.target.x) ||
        !reader.readDouble(&document.camera.target.y) ||
        !reader.readDouble(&document.camera.target.z) ||
        !reader.readDouble(&document.camera.up.x) ||
        !reader.readDouble(&document.camera.up.y) ||
        !reader.readDouble(&document.camera.up.z) ||
        !reader.readDouble(&document.camera.fovYDegrees) ||
        !reader.readDouble(&document.camera.nearPlane) ||
        !reader.readDouble(&document.camera.farPlane) ||
        !reader.readI32(&document.camera.width) ||
        !reader.readI32(&document.camera.height)) {
        return corrupt("payload.view", "truncated view data", reader.offset());
    }
    for (double& value : document.viewMatrix.m) {
        if (!reader.readDouble(&value)) {
            return corrupt("payload.view_matrix", "truncated view matrix",
                           reader.offset());
        }
    }
    if (!reader.readU64(&document.sceneContentHash) ||
        !reader.readU64(&document.loadContentHash)) {
        return corrupt("payload.content_hash", "truncated content hash",
                       reader.offset());
    }
    std::uint32_t blockCount = 0;
    if (!reader.readU32(&blockCount) || blockCount > kMaxSnapshotBlocks) {
        return corrupt("payload.blocks", "invalid snapshot block count",
                       reader.offset());
    }
    document.blocks.reserve(blockCount);
    std::unordered_set<BlockId> ids;
    for (std::uint32_t i = 0; i < blockCount; ++i) {
        SnapshotBlockRecord block;
        if (!reader.readU32(&block.id) || !reader.readU64(&block.contentHash)) {
            return corrupt("payload.blocks", "truncated snapshot block",
                           reader.offset());
        }
        if (!ids.insert(block.id).second) {
            return corrupt("payload.blocks", "duplicate snapshot block id",
                           reader.offset());
        }
        document.blocks.push_back(block);
    }
    std::uint32_t layerCount = 0;
    if (!reader.readU32(&layerCount) || layerCount > kMaxSnapshotLayers) {
        return corrupt("payload.layers", "invalid snapshot layer count",
                       reader.offset());
    }
    document.layers.reserve(layerCount);
    for (std::uint32_t i = 0; i < layerCount; ++i) {
        SnapshotLayerRecord layer;
        if (!reader.readI32(&layer.level) || !reader.readI32(&layer.width) ||
            !reader.readI32(&layer.height) || !reader.readDouble(&layer.quantStep) ||
            !reader.readDouble(&layer.depthMin) ||
            !reader.readDouble(&layer.depthMax)) {
            return corrupt("payload.layers", "truncated snapshot layer",
                           reader.offset());
        }
        if (layer.level < 0 || layer.width <= 0 || layer.height <= 0 ||
            !std::isfinite(layer.quantStep) || layer.quantStep <= 0.0 ||
            !std::isfinite(layer.depthMin) || !std::isfinite(layer.depthMax) ||
            layer.depthMin >= layer.depthMax) {
            return corrupt("payload.layers", "invalid snapshot layer data",
                           reader.offset());
        }
        document.layers.push_back(layer);
    }
    if (!reader.readU64(&document.pyramidContentHash) ||
        !reader.readU64(&document.maskHash) ||
        !reader.readI32(&document.maskWidth) ||
        !reader.readI32(&document.maskHeight)) {
        return corrupt("payload.mask", "truncated mask metadata", reader.offset());
    }
    if (!readSummary(&reader, &document.summary)) {
        return corrupt("payload.summary", "truncated summary", reader.offset());
    }
    if (reader.remaining() != 0) {
        return corrupt("payload", "trailing bytes in snapshot payload",
                       reader.offset());
    }

    if (document.loadContentHash !=
        loadedContentHash(document.sceneContentHash, document.blocks)) {
        return corrupt("payload.load_content_hash",
                       "loaded block content hash mismatch");
    }
    if (document.summary.summaryVersion.empty()) {
        return corrupt("payload.summary.version", "empty error summary version");
    }
    if (document.summary.snapshotVersion != document.snapshotVersion ||
        document.summary.pyramidVersion != document.pyramidVersion ||
        document.summary.loadContentHash != document.loadContentHash ||
        document.summary.maskHash != document.maskHash) {
        return corrupt("payload.summary",
                       "summary version fields do not match snapshot");
    }
    const Mat4 storedView = document.camera.viewMatrix();
    for (std::size_t i = 0; i < 16; ++i) {
        if (storedView.m[i] != document.viewMatrix.m[i]) {
            return corrupt("payload.view_matrix",
                           "view matrix does not match camera fields");
        }
    }

    SnapshotLoadResult result;
    result.document = std::move(document);
    result.code = StatusCode::E_OK;
    result.diagnostic = makeDiagnostic(StatusCode::E_OK, "", "");
    return result;
}

}  // namespace preview
