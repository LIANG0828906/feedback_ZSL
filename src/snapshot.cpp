#include "snapshot.hpp"

#include "sha256.hpp"

#include <algorithm>
#include <array>
#include <cstdint>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <limits>
#include <map>
#include <sstream>
#include <span>
#include <string>
#include <system_error>
#include <utility>
#include <vector>

#if defined(_WIN32)
#include <windows.h>
#else
#include <fcntl.h>
#include <unistd.h>
#endif

namespace mosaic {

namespace {

constexpr std::uint32_t kChunkFileVersion = 1;
constexpr std::uint32_t kSnapshotVersion = 1;

class BinaryWriter {
public:
    void u16(std::uint16_t value) {
        bytes_.push_back(static_cast<std::uint8_t>(value & 0xFFU));
        bytes_.push_back(static_cast<std::uint8_t>((value >> 8U) & 0xFFU));
    }
    void u32(std::uint32_t value) {
        for (std::uint32_t shift = 0; shift < 32U; shift += 8U) {
            bytes_.push_back(static_cast<std::uint8_t>((value >> shift) & 0xFFU));
        }
    }
    void u64(std::uint64_t value) {
        for (std::uint32_t shift = 0; shift < 64U; shift += 8U) {
            bytes_.push_back(static_cast<std::uint8_t>((value >> shift) & 0xFFU));
        }
    }
    void raw(std::span<const std::uint8_t> bytes) {
        bytes_.insert(bytes_.end(), bytes.begin(), bytes.end());
    }
    std::vector<std::uint8_t> take() { return std::move(bytes_); }

private:
    std::vector<std::uint8_t> bytes_;
};

class BinaryReader {
public:
    explicit BinaryReader(std::span<const std::uint8_t> bytes) : bytes_(bytes) {}

    bool u16(std::uint16_t& value) {
        if (position_ + 2 > bytes_.size()) {
            return false;
        }
        value = static_cast<std::uint16_t>(bytes_[position_]) |
                static_cast<std::uint16_t>(bytes_[position_ + 1] << 8U);
        position_ += 2;
        return true;
    }
    bool u32(std::uint32_t& value) {
        if (position_ + 4 > bytes_.size()) {
            return false;
        }
        value = 0;
        for (std::uint32_t i = 0; i < 4; ++i) {
            value |= static_cast<std::uint32_t>(bytes_[position_ + i]) << (8U * i);
        }
        position_ += 4;
        return true;
    }
    bool u64(std::uint64_t& value) {
        if (position_ + 8 > bytes_.size()) {
            return false;
        }
        value = 0;
        for (std::uint32_t i = 0; i < 8; ++i) {
            value |= static_cast<std::uint64_t>(bytes_[position_ + i]) << (8U * i);
        }
        position_ += 8;
        return true;
    }
    bool raw(std::uint8_t* destination, std::size_t size) {
        if (position_ + size > bytes_.size()) {
            return false;
        }
        std::memcpy(destination, bytes_.data() + position_, size);
        position_ += size;
        return true;
    }
    std::span<const std::uint8_t> rest() const {
        return bytes_.subspan(position_);
    }
    bool atEnd() const { return position_ == bytes_.size(); }

private:
    std::span<const std::uint8_t> bytes_;
    std::size_t position_ = 0;
};

void appendHex64(BinaryWriter& writer, const std::string& digest) {
    std::array<std::uint8_t, 64> bytes{};
    const std::size_t count = std::min<std::size_t>(digest.size(), bytes.size());
    std::memcpy(bytes.data(), digest.data(), count);
    writer.raw(bytes);
}

std::string readHex64(BinaryReader& reader, bool& ok) {
    std::array<std::uint8_t, 64> bytes{};
    ok = reader.raw(bytes.data(), bytes.size());
    if (!ok) {
        return {};
    }
    std::string text(bytes.begin(), bytes.end());
    if (text.find_first_not_of("0123456789abcdef") != std::string::npos) {
        ok = false;
        return {};
    }
    return text;
}

std::vector<std::uint8_t> encodeChunk(
    const std::string& sceneDigest,
    const ChunkResult& chunk) {
    BinaryWriter writer;
    writer.raw(std::span<const std::uint8_t>(
        reinterpret_cast<const std::uint8_t*>("MCHK"),
        4));
    writer.u32(kChunkFileVersion);
    writer.u32(chunk.index);
    appendHex64(writer, sceneDigest);
    writer.u32(chunk.x);
    writer.u32(chunk.y);
    writer.u32(chunk.width);
    writer.u32(chunk.height);
    writer.u32(static_cast<std::uint32_t>(chunk.layerStats.size()));
    for (const LayerStats& layer : chunk.layerStats) {
        writer.u32(static_cast<std::uint32_t>(layer.layerIndex));
        writer.u64(layer.visibleFragments);
        writer.u64(layer.transmissionEnergyMicro);
        writer.u64(layer.reflectionEnergyMicro);
        writer.u64(layer.transmissionLuminanceMicro);
    }
    writer.u64(chunk.rgba.size());
    writer.u64(chunk.ownership.size());
    writer.raw(chunk.rgba);
    for (const std::uint64_t owner : chunk.ownership) {
        writer.u64(owner);
    }
    return writer.take();
}

std::optional<ChunkResult> decodeChunk(
    std::span<const std::uint8_t> bytes,
    const Scene& scene,
    const std::string& sceneDigest,
    std::uint32_t expectedIndex,
    std::string& reason) {
    BinaryReader reader(bytes);
    std::array<std::uint8_t, 4> magic{};
    if (!reader.raw(magic.data(), magic.size()) ||
        std::memcmp(magic.data(), "MCHK", 4) != 0) {
        reason = "chunk-magic";
        return std::nullopt;
    }
    std::uint32_t version = 0;
    if (!reader.u32(version) || version != kChunkFileVersion) {
        reason = "chunk-version";
        return std::nullopt;
    }
    ChunkResult chunk;
    if (!reader.u32(chunk.index) || chunk.index != expectedIndex) {
        reason = "chunk-index";
        return std::nullopt;
    }
    bool digestOk = false;
    const std::string storedDigest = readHex64(reader, digestOk);
    if (!digestOk || storedDigest != sceneDigest) {
        reason = "chunk-scene-digest";
        return std::nullopt;
    }
    std::uint32_t layerCount = 0;
    if (!reader.u32(chunk.x) || !reader.u32(chunk.y) ||
        !reader.u32(chunk.width) || !reader.u32(chunk.height) ||
        !reader.u32(layerCount)) {
        reason = "chunk-header";
        return std::nullopt;
    }
    std::uint32_t expectedX = 0;
    std::uint32_t expectedY = 0;
    std::uint32_t expectedWidth = 0;
    std::uint32_t expectedHeight = 0;
    chunkBounds(scene, expectedIndex, expectedX, expectedY, expectedWidth, expectedHeight);
    if (chunk.x != expectedX || chunk.y != expectedY ||
        chunk.width != expectedWidth || chunk.height != expectedHeight) {
        reason = "chunk-bounds";
        return std::nullopt;
    }
    if (layerCount > 4096U) {
        reason = "chunk-layer-count";
        return std::nullopt;
    }
    chunk.layerStats.resize(layerCount);
    for (std::uint32_t i = 0; i < layerCount; ++i) {
        std::uint32_t layerIndex = 0;
        LayerStats& layer = chunk.layerStats[i];
        if (!reader.u32(layerIndex) || !reader.u64(layer.visibleFragments) ||
            !reader.u64(layer.transmissionEnergyMicro) ||
            !reader.u64(layer.reflectionEnergyMicro) ||
            !reader.u64(layer.transmissionLuminanceMicro)) {
            reason = "chunk-stats";
            return std::nullopt;
        }
        layer.layerIndex = static_cast<std::int32_t>(layerIndex);
    }
    std::uint64_t rgbaSize = 0;
    std::uint64_t ownerCount = 0;
    if (!reader.u64(rgbaSize) || !reader.u64(ownerCount)) {
        reason = "chunk-payload-size";
        return std::nullopt;
    }
    const std::uint64_t expectedPixels =
        static_cast<std::uint64_t>(chunk.width) * chunk.height;
    if (rgbaSize != expectedPixels * 4U || ownerCount != expectedPixels ||
        rgbaSize > std::numeric_limits<std::size_t>::max() ||
        ownerCount > std::numeric_limits<std::size_t>::max()) {
        reason = "chunk-payload-length";
        return std::nullopt;
    }
    chunk.rgba.resize(static_cast<std::size_t>(rgbaSize));
    if (!reader.raw(chunk.rgba.data(), chunk.rgba.size())) {
        reason = "chunk-rgba";
        return std::nullopt;
    }
    chunk.ownership.resize(static_cast<std::size_t>(ownerCount));
    for (std::uint64_t& owner : chunk.ownership) {
        if (!reader.u64(owner)) {
            reason = "chunk-ownership";
            return std::nullopt;
        }
    }
    if (!reader.atEnd()) {
        reason = "chunk-trailing-bytes";
        return std::nullopt;
    }
    return chunk;
}

const JsonValue* requireObjectField(
    const JsonValue& object,
    std::string_view key,
    std::string& reason) {
    if (!object.isObject()) {
        reason = "snapshot-object-expected";
        return nullptr;
    }
    const JsonValue* value = object.find(key);
    if (value == nullptr) {
        reason = "snapshot-field-missing";
    }
    return value;
}

bool readString(
    const JsonValue& object,
    std::string_view key,
    std::string& output,
    std::string& reason) {
    const JsonValue* value = requireObjectField(object, key, reason);
    if (value == nullptr || !value->isString()) {
        if (value != nullptr) {
            reason = "snapshot-field-type";
        }
        return false;
    }
    output = value->asString();
    return true;
}

bool readU32(
    const JsonValue& object,
    std::string_view key,
    std::uint32_t& output,
    std::string& reason) {
    const JsonValue* value = requireObjectField(object, key, reason);
    if (value == nullptr || !value->isNumber() ||
        value->asNumber() < 0.0 ||
        std::trunc(value->asNumber()) != value->asNumber() ||
        value->asNumber() > static_cast<double>(std::numeric_limits<std::uint32_t>::max())) {
        if (value != nullptr) {
            reason = "snapshot-field-type";
        }
        return false;
    }
    output = static_cast<std::uint32_t>(value->asNumber());
    return true;
}

bool pathIsSafeRelative(const std::string& text) {
    if (text.empty()) {
        return false;
    }
    const std::filesystem::path path(text);
    if (path.is_absolute() || path.has_root_name() || path.has_root_directory()) {
        return false;
    }
    for (const auto& part : path) {
        if (part == ".." || part == ".") {
            return false;
        }
    }
    return true;
}

struct ChunkColorSummary {
    std::uint64_t meanR = 0;
    std::uint64_t meanG = 0;
    std::uint64_t meanB = 0;
    std::uint64_t meanA = 0;
};

ChunkColorSummary computeChunkColorSummary(const ChunkResult& result) {
    std::uint64_t red = 0;
    std::uint64_t green = 0;
    std::uint64_t blue = 0;
    std::uint64_t alpha = 0;
    const std::size_t pixels = result.rgba.size() / 4U;
    for (std::size_t i = 0; i < pixels; ++i) {
        red += result.rgba[i * 4];
        green += result.rgba[i * 4 + 1];
        blue += result.rgba[i * 4 + 2];
        alpha += result.rgba[i * 4 + 3];
    }
    const auto mean = [pixels](std::uint64_t total) {
        return pixels == 0 ? 0 : static_cast<std::uint64_t>(
            (total + pixels / 2U) / pixels);
    };
    return {mean(red), mean(green), mean(blue), mean(alpha)};
}

std::string chunkColorSummaryJson(const StoredChunk& chunk) {
    const ChunkColorSummary summary = computeChunkColorSummary(chunk.result);
    std::ostringstream out;
    out << "{\"meanR\": " << summary.meanR << ", \"meanG\": " << summary.meanG
        << ", \"meanB\": " << summary.meanB << ", \"meanA\": " << summary.meanA
        << ", \"rgbaSha256\": \"" << chunk.rgbaDigest
        << "\", \"ownershipSha256\": \"" << chunk.ownershipDigest << "\"}";
    return out.str();
}

bool writeManifestAtomic(
    const std::filesystem::path& cacheDirectory,
    const std::string& manifest,
    std::string& error) {
    return atomicWriteTextFile(cacheDirectory / "snapshot.json", manifest, error);
}

bool ensureDirectory(const std::filesystem::path& path, std::string& error) {
    std::error_code code;
    std::filesystem::create_directories(path, code);
    if (code) {
        error = "cannot create directory: " + code.message();
        return false;
    }
    return true;
}

}  // namespace

std::string chunkFileRelativePath(std::uint32_t chunkIndex) {
    std::ostringstream out;
    out << "chunks/chunk_" << std::setw(6) << std::setfill('0') << chunkIndex << ".bin";
    return out.str();
}

std::optional<std::vector<std::uint8_t>> readBinaryFile(
    const std::filesystem::path& path,
    std::string& error) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        error = "cannot open file";
        return std::nullopt;
    }
    input.seekg(0, std::ios::end);
    const std::streamoff size = input.tellg();
    if (size < 0) {
        error = "cannot determine file size";
        return std::nullopt;
    }
    input.seekg(0, std::ios::beg);
    std::vector<std::uint8_t> bytes(static_cast<std::size_t>(size));
    if (size > 0 && !input.read(reinterpret_cast<char*>(bytes.data()), size)) {
        error = "cannot read file";
        return std::nullopt;
    }
    return bytes;
}

bool atomicWriteFile(
    const std::filesystem::path& path,
    std::span<const std::uint8_t> bytes,
    std::string& error) {
    if (!ensureDirectory(path.parent_path(), error)) {
        return false;
    }
    const std::filesystem::path temporary = path.string() + ".tmp";
    {
        std::ofstream output(temporary, std::ios::binary | std::ios::trunc);
        if (!output) {
            error = "cannot open temporary output file";
            return false;
        }
        if (!bytes.empty()) {
            output.write(
                reinterpret_cast<const char*>(bytes.data()),
                static_cast<std::streamsize>(bytes.size()));
        }
        output.flush();
        if (!output) {
            error = "cannot write temporary output file";
            return false;
        }
    }
#if defined(_WIN32)
    if (!MoveFileExW(
            temporary.wstring().c_str(),
            path.wstring().c_str(),
            MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)) {
        error = "cannot replace destination file";
        return false;
    }
#else
    const int fd = ::open(temporary.c_str(), O_RDONLY);
    if (fd >= 0) {
        ::fsync(fd);
        ::close(fd);
    }
    std::error_code renameError;
    std::filesystem::rename(temporary, path, renameError);
    if (renameError) {
        error = "cannot atomically rename output file: " + renameError.message();
        return false;
    }
    const std::filesystem::path parent = path.parent_path();
    const int directoryFd = ::open(parent.c_str(), O_RDONLY | O_DIRECTORY);
    if (directoryFd >= 0) {
        ::fsync(directoryFd);
        ::close(directoryFd);
    }
#endif
    return true;
}

bool atomicWriteTextFile(
    const std::filesystem::path& path,
    const std::string& text,
    std::string& error) {
    return atomicWriteFile(
        path,
        std::span<const std::uint8_t>(
            reinterpret_cast<const std::uint8_t*>(text.data()),
            text.size()),
        error);
}

std::string bytesDigest(std::span<const std::uint8_t> bytes) {
    return Sha256::digest(bytes);
}

std::string bytesDigest(const std::vector<std::uint8_t>& bytes) {
    return Sha256::digest(bytes);
}

SnapshotLoadResult loadSnapshot(
    const std::filesystem::path& cacheDirectory,
    const Scene& scene,
    const std::string& sceneDigest) {
    SnapshotLoadResult result;
    const std::filesystem::path manifestPath = cacheDirectory / "snapshot.json";
    std::error_code existsError;
    if (!std::filesystem::exists(manifestPath, existsError)) {
        return result;
    }
    result.snapshotPresent = true;
    std::string error;
    const auto manifestBytes = readBinaryFile(manifestPath, error);
    if (!manifestBytes.has_value()) {
        result.discarded = true;
        result.discardReason = "snapshot-read-error";
        return result;
    }
    const std::string manifestText(
        reinterpret_cast<const char*>(manifestBytes->data()),
        manifestBytes->size());
    const JsonParseResult parsed = parseJson(manifestText);
    if (parsed.error.has_value() || !parsed.value.has_value()) {
        result.discarded = true;
        result.discardReason = "snapshot-parse-error";
        return result;
    }
    const JsonValue& root = *parsed.value;
    std::string format;
    std::string storedSceneDigest;
    std::uint32_t version = 0;
    std::uint32_t chunkWidth = 0;
    std::uint32_t chunkHeight = 0;
    std::uint32_t totalChunks = 0;
    std::uint32_t completedCount = 0;
    std::uint32_t storedResumeCount = 0;
    std::string state;
    std::string reason;
    if (!readString(root, "format", format, reason) ||
        !readU32(root, "version", version, reason) ||
        !readString(root, "state", state, reason) ||
        !readString(root, "sceneDigest", storedSceneDigest, reason) ||
        !readU32(root, "chunkWidth", chunkWidth, reason) ||
        !readU32(root, "chunkHeight", chunkHeight, reason) ||
        !readU32(root, "totalChunks", totalChunks, reason) ||
        !readU32(root, "completedCount", completedCount, reason) ||
        !readU32(root, "resumeCount", storedResumeCount, reason)) {
        result.discarded = true;
        result.discardReason = "snapshot-invalid:" + reason;
        return result;
    }
    if (format != "mosaic-snapshot" || version != kSnapshotVersion) {
        result.discarded = true;
        result.discardReason = "snapshot-version";
        return result;
    }
    if (state != "in-progress" && state != "interrupted" && state != "complete") {
        result.discarded = true;
        result.discardReason = "snapshot-state";
        return result;
    }
    if (storedSceneDigest != sceneDigest) {
        result.discarded = true;
        result.discardReason = "scene-digest-mismatch";
        return result;
    }
    if (chunkWidth != scene.settings.output.chunkWidth ||
        chunkHeight != scene.settings.output.chunkHeight ||
        totalChunks != chunkCount(scene)) {
        result.discarded = true;
        result.discardReason = "snapshot-layout-mismatch";
        return result;
    }
    const JsonValue* summaryValue = requireObjectField(root, "sceneSummary", reason);
    std::string summaryDigest;
    std::uint32_t summaryWidth = 0;
    std::uint32_t summaryHeight = 0;
    if (summaryValue == nullptr || !summaryValue->isObject() ||
        !readString(*summaryValue, "digest", summaryDigest, reason) ||
        !readU32(*summaryValue, "width", summaryWidth, reason) ||
        !readU32(*summaryValue, "height", summaryHeight, reason) ||
        summaryDigest != sceneDigest ||
        summaryWidth != scene.settings.output.width ||
        summaryHeight != scene.settings.output.height) {
        result.discarded = true;
        result.discardReason = "snapshot-scene-summary";
        return result;
    }
    const JsonValue* completedValue = requireObjectField(root, "completedChunks", reason);
    const JsonValue* chunksValue = requireObjectField(root, "chunks", reason);
    if (completedValue == nullptr || !completedValue->isArray() ||
        chunksValue == nullptr || !chunksValue->isArray() ||
        completedValue->size() != completedCount ||
        chunksValue->size() != completedCount) {
        result.discarded = true;
        result.discardReason = "snapshot-completed-list";
        return result;
    }
    if (completedCount > totalChunks) {
        result.discarded = true;
        result.discardReason = "snapshot-completed-count";
        return result;
    }

    std::vector<std::uint32_t> completed;
    completed.reserve(completedCount);
    for (std::size_t i = 0; i < completedValue->size(); ++i) {
        const JsonValue& entry = completedValue->at(i);
        if (!entry.isNumber() || entry.asNumber() < 0.0 ||
            std::trunc(entry.asNumber()) != entry.asNumber()) {
            result.discarded = true;
            result.discardReason = "snapshot-completed-index";
            return result;
        }
        const auto index = static_cast<std::uint32_t>(entry.asNumber());
        if (index >= totalChunks) {
            result.discarded = true;
            result.discardReason = "snapshot-completed-range";
            return result;
        }
        completed.push_back(index);
    }
    std::sort(completed.begin(), completed.end());
    if (std::adjacent_find(completed.begin(), completed.end()) != completed.end()) {
        result.discarded = true;
        result.discardReason = "snapshot-completed-duplicate";
        return result;
    }

    std::map<std::uint32_t, const JsonValue*> entriesByIndex;
    for (std::size_t i = 0; i < chunksValue->size(); ++i) {
        const JsonValue& entry = chunksValue->at(i);
        std::uint32_t index = 0;
        if (!readU32(entry, "index", index, reason) || index >= totalChunks ||
            entriesByIndex.find(index) != entriesByIndex.end()) {
            result.discarded = true;
            result.discardReason = "snapshot-chunk-entry";
            return result;
        }
        entriesByIndex[index] = &entry;
    }
    for (const std::uint32_t index : completed) {
        if (entriesByIndex.find(index) == entriesByIndex.end()) {
            result.discarded = true;
            result.discardReason = "snapshot-chunk-entry-missing";
            return result;
        }
    }
    if (entriesByIndex.size() != completed.size()) {
        result.discarded = true;
        result.discardReason = "snapshot-chunk-entry-extra";
        return result;
    }

    const std::filesystem::path canonicalCache = std::filesystem::weakly_canonical(cacheDirectory);
    for (const std::uint32_t index : completed) {
        const JsonValue& entry = *entriesByIndex.at(index);
        std::string relativePath;
        std::string fileDigest;
        std::string rgbaDigest;
        std::string ownershipDigest;
        std::uint32_t x = 0;
        std::uint32_t y = 0;
        std::uint32_t width = 0;
        std::uint32_t height = 0;
        if (!readString(entry, "file", relativePath, reason) ||
            !readString(entry, "fileSha256", fileDigest, reason) ||
            !readString(entry, "rgbaSha256", rgbaDigest, reason) ||
            !readString(entry, "ownershipSha256", ownershipDigest, reason) ||
            !readU32(entry, "x", x, reason) ||
            !readU32(entry, "y", y, reason) ||
            !readU32(entry, "width", width, reason) ||
            !readU32(entry, "height", height, reason) ||
            !pathIsSafeRelative(relativePath)) {
            result.discarded = true;
            result.discardReason = "snapshot-chunk-metadata";
            return result;
        }
        std::uint32_t expectedX = 0;
        std::uint32_t expectedY = 0;
        std::uint32_t expectedWidth = 0;
        std::uint32_t expectedHeight = 0;
        chunkBounds(scene, index, expectedX, expectedY, expectedWidth, expectedHeight);
        if (x != expectedX || y != expectedY ||
            width != expectedWidth || height != expectedHeight) {
            result.discarded = true;
            result.discardReason = "snapshot-chunk-bounds";
            return result;
        }
        const std::filesystem::path chunkPath = canonicalCache / relativePath;
        const std::filesystem::path canonicalChunk =
            std::filesystem::weakly_canonical(chunkPath);
        const std::string cachePrefix = canonicalCache.string();
        const std::string chunkText = canonicalChunk.string();
        if (chunkText.rfind(cachePrefix + std::filesystem::path::preferred_separator, 0) != 0) {
            result.discarded = true;
            result.discardReason = "snapshot-chunk-path-escape";
            return result;
        }
        const auto fileBytes = readBinaryFile(canonicalChunk, error);
        if (!fileBytes.has_value()) {
            result.discarded = true;
            result.discardReason = "snapshot-chunk-file-missing";
            return result;
        }
        if (bytesDigest(*fileBytes) != fileDigest) {
            result.discarded = true;
            result.discardReason = "snapshot-chunk-file-digest";
            return result;
        }
        const auto chunk = decodeChunk(*fileBytes, scene, sceneDigest, index, reason);
        if (!chunk.has_value()) {
            result.discarded = true;
            result.discardReason = "snapshot-" + reason;
            return result;
        }
        const std::string actualRgbaDigest = bytesDigest(chunk->rgba);
        const std::vector<std::uint8_t> ownershipBytes =
            [&chunk]() {
                std::vector<std::uint8_t> bytes;
                bytes.reserve(chunk->ownership.size() * 8U);
                for (const std::uint64_t value : chunk->ownership) {
                    for (std::uint32_t shift = 0; shift < 64U; shift += 8U) {
                        bytes.push_back(
                            static_cast<std::uint8_t>((value >> shift) & 0xFFU));
                    }
                }
                return bytes;
            }();
        if (actualRgbaDigest != rgbaDigest ||
            bytesDigest(ownershipBytes) != ownershipDigest) {
            result.discarded = true;
            result.discardReason = "snapshot-chunk-color-summary";
            return result;
        }
        const JsonValue* colorValue = entry.find("colorSummary");
        const ChunkColorSummary expectedSummary =
            computeChunkColorSummary(chunk.value());
        std::uint32_t meanR = 0;
        std::uint32_t meanG = 0;
        std::uint32_t meanB = 0;
        std::uint32_t meanA = 0;
        std::string summaryRgbaDigest;
        std::string summaryOwnershipDigest;
        if (colorValue == nullptr || !colorValue->isObject() ||
            !readU32(*colorValue, "meanR", meanR, reason) ||
            !readU32(*colorValue, "meanG", meanG, reason) ||
            !readU32(*colorValue, "meanB", meanB, reason) ||
            !readU32(*colorValue, "meanA", meanA, reason) ||
            !readString(*colorValue, "rgbaSha256", summaryRgbaDigest, reason) ||
            !readString(*colorValue, "ownershipSha256", summaryOwnershipDigest, reason) ||
            meanR != expectedSummary.meanR || meanG != expectedSummary.meanG ||
            meanB != expectedSummary.meanB || meanA != expectedSummary.meanA ||
            summaryRgbaDigest != rgbaDigest ||
            summaryOwnershipDigest != ownershipDigest) {
            result.discarded = true;
            result.discardReason = "snapshot-chunk-color-summary";
            return result;
        }
        result.chunks.emplace(
            index,
            StoredChunk{*chunk, fileDigest, rgbaDigest, ownershipDigest});
    }
    result.valid = true;
    result.reusedChunks = static_cast<std::uint32_t>(result.chunks.size());
    result.resumeCount = storedResumeCount + 1U;
    return result;
}

StoredChunk storeChunk(
    const std::filesystem::path& cacheDirectory,
    const std::string& sceneDigest,
    const ChunkResult& chunk,
    std::string& error) {
    const std::string relativePath = chunkFileRelativePath(chunk.index);
    const std::vector<std::uint8_t> bytes = encodeChunk(sceneDigest, chunk);
    if (!atomicWriteFile(cacheDirectory / relativePath, bytes, error)) {
        return {};
    }
    std::vector<std::uint8_t> ownershipBytes;
    ownershipBytes.reserve(chunk.ownership.size() * 8U);
    for (const std::uint64_t owner : chunk.ownership) {
        for (std::uint32_t shift = 0; shift < 64U; shift += 8U) {
            ownershipBytes.push_back(
                static_cast<std::uint8_t>((owner >> shift) & 0xFFU));
        }
    }
    return {
        chunk,
        bytesDigest(bytes),
        bytesDigest(chunk.rgba),
        bytesDigest(ownershipBytes),
    };
}

bool writeSnapshot(
    const std::filesystem::path& cacheDirectory,
    const Scene& scene,
    const std::string& sceneDigest,
    const std::map<std::uint32_t, StoredChunk>& chunks,
    const std::string& state,
    std::uint32_t resumeCount,
    std::string& error) {
    std::ostringstream out;
    out << "{\n  \"format\": \"mosaic-snapshot\",\n";
    out << "  \"version\": " << kSnapshotVersion << ",\n";
    out << "  \"state\": \"" << escapeJsonString(state) << "\",\n";
    out << "  \"sceneDigest\": \"" << sceneDigest << "\",\n";
    out << "  \"sceneSummary\": " << sceneSummaryJson(scene) << ",\n";
    out << "  \"chunkWidth\": " << scene.settings.output.chunkWidth << ",\n";
    out << "  \"chunkHeight\": " << scene.settings.output.chunkHeight << ",\n";
    out << "  \"totalChunks\": " << chunkCount(scene) << ",\n";
    out << "  \"resumeCount\": " << resumeCount << ",\n";
    out << "  \"completedCount\": " << chunks.size() << ",\n";
    out << "  \"completedChunks\": [";
    bool first = true;
    for (const auto& [index, chunk] : chunks) {
        (void)chunk;
        out << (first ? "" : ", ") << index;
        first = false;
    }
    out << "],\n  \"chunks\": [";
    first = true;
    for (const auto& [index, stored] : chunks) {
        const ChunkResult& chunk = stored.result;
        out << (first ? "\n" : ",\n");
        out << "    {\"index\": " << index
            << ", \"x\": " << chunk.x
            << ", \"y\": " << chunk.y
            << ", \"width\": " << chunk.width
            << ", \"height\": " << chunk.height
            << ", \"file\": \"" << chunkFileRelativePath(index).c_str() << "\""
            << ", \"fileSha256\": \"" << stored.fileDigest << "\""
            << ", \"rgbaSha256\": \"" << stored.rgbaDigest << "\""
            << ", \"ownershipSha256\": \"" << stored.ownershipDigest << "\""
            << ", \"colorSummary\": " << chunkColorSummaryJson(stored) << "}";
        first = false;
    }
    if (!chunks.empty()) {
        out << '\n';
    }
    out << "  ]\n}\n";
    return writeManifestAtomic(cacheDirectory, out.str(), error);
}

bool writeRecoveryReport(
    const std::filesystem::path& outputDirectory,
    const Scene& scene,
    const std::string& sceneDigest,
    const std::string& outcome,
    const SnapshotLoadResult& loadResult,
    std::uint32_t reusedChunks,
    std::uint32_t recomputedChunks,
    const std::map<std::uint32_t, StoredChunk>& chunks,
    std::string& error) {
    std::ostringstream out;
    out << "{\n  \"format\": \"mosaic-recovery-report\",\n";
    out << "  \"version\": 1,\n";
    out << "  \"outcome\": \"" << escapeJsonString(outcome) << "\",\n";
    out << "  \"sceneDigest\": \"" << sceneDigest << "\",\n";
    out << "  \"snapshotPresent\": " << (loadResult.snapshotPresent ? "true" : "false") << ",\n";
    out << "  \"snapshotLoaded\": " << (loadResult.valid ? "true" : "false") << ",\n";
    out << "  \"snapshotDiscarded\": " << (loadResult.discarded ? "true" : "false") << ",\n";
    out << "  \"discardReason\": \"" << escapeJsonString(loadResult.discardReason) << "\",\n";
    out << "  \"reusedChunks\": " << reusedChunks << ",\n";
    out << "  \"recomputedChunks\": " << recomputedChunks << ",\n";
    out << "  \"totalChunks\": " << chunkCount(scene) << ",\n";
    out << "  \"resumeCount\": " << loadResult.resumeCount << ",\n";
    out << "  \"completedChunks\": [";
    bool first = true;
    for (const auto& [index, chunk] : chunks) {
        (void)chunk;
        out << (first ? "" : ", ") << index;
        first = false;
    }
    out << "]\n}\n";
    return atomicWriteTextFile(outputDirectory / "recovery.json", out.str(), error);
}

void discardCache(const std::filesystem::path& cacheDirectory) {
    std::error_code error;
    std::filesystem::remove_all(cacheDirectory, error);
}

}  // namespace mosaic
