#include "font_atlas/snapshot.hpp"

#include "font_atlas/io.hpp"
#include "font_atlas/sha256.hpp"

#include <algorithm>
#include <exception>
#include <iomanip>
#include <sstream>

namespace font_atlas {
namespace {

Diagnostic mismatch(const std::string& message) {
    Diagnostic diagnostic;
    diagnostic.code = ErrorCode::SnapshotMismatch;
    diagnostic.message = message;
    return diagnostic;
}

std::string chunkRecordText(const ChunkPlan& chunk) {
    std::ostringstream stream;
    stream << chunk.index << '|' << chunk.parentIndex << '|' << chunk.splitLevel << '|'
           << chunk.rect.x << ',' << chunk.rect.y << ',' << chunk.rect.width << ','
           << chunk.rect.height << '|' << chunk.samples << '|' << chunk.batchSize << '|'
           << (chunk.completed ? 1 : 0) << '|' << chunk.contentHash;
    return stream.str();
}

std::string buildChain(const std::string& configHash, const std::vector<ChunkPlan>& chunks,
                       int committedChunks) {
    std::string chain = Sha256::hex("font-atlas-snapshot-v1|" + configHash);
    for (int i = 0; i < committedChunks; ++i) {
        chain = Sha256::hex(chain + "|" + chunkRecordText(chunks[static_cast<std::size_t>(i)]));
    }
    return chain;
}

Json configJson(const SnapshotConfig& config) {
    Json value = Json::object();
    value["fontFingerprint"] = Json(config.fontFingerprint);
    value["glyphFingerprint"] = Json(config.glyphFingerprint);
    value["glyph"] = Json(config.glyph);
    value["targetPixelSize"] = Json(config.targetPixelSize);
    value["subpixelX"] = Json(config.subpixelX);
    value["subpixelY"] = Json(config.subpixelY);
    value["fillRule"] = Json(fillRuleName(config.fillRule));
    value["quantizationStep"] = Json(config.quantizationStep);
    value["samplesPerAxis"] = Json(config.samplesPerAxis);
    value["tileWidth"] = Json(config.tileWidth);
    value["tileHeight"] = Json(config.tileHeight);
    value["bitmapWidth"] = Json(config.bitmapWidth);
    value["bitmapHeight"] = Json(config.bitmapHeight);
    value["seamTolerance"] = Json(config.seamTolerance);
    value["parallelism"] = Json(config.parallelism);
    value["globalBudgetBytes"] = Json(config.globalBudgetBytes);
    value["perTaskBudgetBytes"] = Json(config.perTaskBudgetBytes);
    value["sampleBatchSize"] = Json(config.sampleBatchSize);
    Json degrade = Json::array();
    for (const auto& item : config.degradeSequence) {
        degrade.pushBack(Json(item));
    }
    value["degradeSequence"] = degrade;
    return value;
}

Result<SnapshotConfig> parseConfig(const Json& value) {
    Diagnostic diagnostic;
    if (!value.isObject()) {
        return Result<SnapshotConfig>::failure(mismatch("snapshot config is not an object"));
    }
    SnapshotConfig config;
    config.fontFingerprint = jsonString(value, "fontFingerprint", "", diagnostic);
    config.glyphFingerprint = jsonString(value, "glyphFingerprint", "", diagnostic);
    config.glyph = jsonString(value, "glyph", "", diagnostic);
    config.targetPixelSize = jsonInt(value, "targetPixelSize", 0, diagnostic);
    config.subpixelX = jsonDouble(value, "subpixelX", 0.0, diagnostic);
    config.subpixelY = jsonDouble(value, "subpixelY", 0.0, diagnostic);
    const std::string fill = jsonString(value, "fillRule", "nonzero", diagnostic);
    if (!parseFillRule(fill, config.fillRule)) {
        return Result<SnapshotConfig>::failure(mismatch("invalid fillRule in snapshot"));
    }
    config.quantizationStep = jsonInt(value, "quantizationStep", 0, diagnostic);
    config.samplesPerAxis = jsonInt(value, "samplesPerAxis", 0, diagnostic);
    config.tileWidth = jsonInt(value, "tileWidth", 0, diagnostic);
    config.tileHeight = jsonInt(value, "tileHeight", 0, diagnostic);
    config.bitmapWidth = jsonInt(value, "bitmapWidth", 0, diagnostic);
    config.bitmapHeight = jsonInt(value, "bitmapHeight", 0, diagnostic);
    config.seamTolerance = jsonInt(value, "seamTolerance", 0, diagnostic);
    config.parallelism = jsonInt(value, "parallelism", 0, diagnostic);
    config.globalBudgetBytes = jsonUInt64(value, "globalBudgetBytes", 0, diagnostic);
    config.perTaskBudgetBytes = jsonUInt64(value, "perTaskBudgetBytes", 0, diagnostic);
    config.sampleBatchSize = jsonUInt64(value, "sampleBatchSize", 0, diagnostic);
    const Json* degrade = value.find("degradeSequence");
    if (degrade != nullptr) {
        if (!degrade->isArray()) {
            return Result<SnapshotConfig>::failure(
                mismatch("snapshot degradeSequence is not an array"));
        }
        for (const auto& item : degrade->asArray()) {
            if (!item.isString()) {
                return Result<SnapshotConfig>::failure(
                    mismatch("snapshot degradeSequence contains a non-string"));
            }
            config.degradeSequence.push_back(item.asString());
        }
    }
    if (!diagnostic.ok()) {
        return Result<SnapshotConfig>::failure(
            mismatch("snapshot config is malformed: " + diagnostic.message));
    }
    if (config.fontFingerprint.empty() || config.glyphFingerprint.empty() ||
        config.glyph.empty() || config.targetPixelSize <= 0 || config.quantizationStep <= 0 ||
        config.samplesPerAxis <= 0 || config.tileWidth <= 0 || config.tileHeight <= 0 ||
        config.bitmapWidth <= 0 || config.bitmapHeight <= 0) {
        return Result<SnapshotConfig>::failure(
            mismatch("snapshot config has missing or non-positive fields"));
    }
    return Result<SnapshotConfig>::success(std::move(config));
}

Result<SnapshotState> parseSnapshot(const Json& root) {
    if (!root.isObject()) {
        return Result<SnapshotState>::failure(mismatch("snapshot root is not an object"));
    }
    const Json* format = root.find("format");
    if (format == nullptr || !format->isString() ||
        format->asString() != "font-atlas-snapshot") {
        return Result<SnapshotState>::failure(mismatch("snapshot format tag is invalid"));
    }
    const Json* version = root.find("version");
    bool supportedVersion = false;
    if (version != nullptr && version->isNumber()) {
        try {
            supportedVersion = version->asInt() == 1;
        } catch (const std::exception&) {
            supportedVersion = false;
        }
    }
    if (!supportedVersion) {
        return Result<SnapshotState>::failure(mismatch("snapshot version is not supported"));
    }
    Diagnostic diagnostic;
    SnapshotState state;
    state.version = 1;
    const Json* config = root.find("config");
    const Json* configHash = root.find("configHash");
    const Json* chunks = root.find("chunks");
    const Json* committed = root.find("committedChunks");
    const Json* chain = root.find("hashChain");
    if (config == nullptr || configHash == nullptr || !configHash->isString() ||
        chunks == nullptr || !chunks->isArray() || committed == nullptr ||
        !committed->isNumber() || chain == nullptr || !chain->isString()) {
        return Result<SnapshotState>::failure(mismatch("snapshot is missing required fields"));
    }
    auto parsedConfig = parseConfig(*config);
    if (!parsedConfig.ok()) {
        return Result<SnapshotState>::failure(std::move(parsedConfig.error));
    }
    state.config = std::move(*parsedConfig.value);
    state.configHash = configHash->asString();
    if (state.configHash != snapshotConfigHash(state.config)) {
        return Result<SnapshotState>::failure(mismatch("snapshot config hash mismatch"));
    }
    try {
        state.committedChunks = committed->asInt();
    } catch (const std::exception&) {
        return Result<SnapshotState>::failure(
            mismatch("snapshot committed chunk count is not an integer"));
    }
    if (state.committedChunks < 0 ||
        static_cast<std::size_t>(state.committedChunks) > chunks->size()) {
        return Result<SnapshotState>::failure(mismatch("snapshot committed chunk count invalid"));
    }
    state.hashChain = chain->asString();
    for (std::size_t i = 0; i < chunks->size(); ++i) {
        const Json& item = chunks->asArray()[i];
        if (!item.isObject()) {
            return Result<SnapshotState>::failure(mismatch("snapshot chunk is not an object"));
        }
        ChunkPlan chunk;
        chunk.index = jsonInt(item, "index", -1, diagnostic);
        chunk.parentIndex = jsonInt(item, "parentIndex", -1, diagnostic);
        chunk.splitLevel = jsonInt(item, "splitLevel", 0, diagnostic);
        chunk.samples = jsonUInt64(item, "samples", 0, diagnostic);
        chunk.batchSize = jsonUInt64(item, "batchSize", 1, diagnostic);
        chunk.estimatedBytes = jsonUInt64(item, "estimatedBytes", 0, diagnostic);
        chunk.contentHash = jsonString(item, "contentHash", "", diagnostic);
        chunk.completed = jsonBool(item, "completed", false, diagnostic);
        const Json* rect = item.find("rect");
        if (rect == nullptr || !rect->isObject()) {
            return Result<SnapshotState>::failure(mismatch("snapshot chunk rect is invalid"));
        }
        chunk.rect.x = jsonInt(*rect, "x", -1, diagnostic);
        chunk.rect.y = jsonInt(*rect, "y", -1, diagnostic);
        chunk.rect.width = jsonInt(*rect, "width", 0, diagnostic);
        chunk.rect.height = jsonInt(*rect, "height", 0, diagnostic);
        if (!diagnostic.ok() || chunk.index != static_cast<int>(i) ||
            chunk.rect.x < 0 || chunk.rect.y < 0 || chunk.rect.width <= 0 ||
            chunk.rect.height <= 0) {
            return Result<SnapshotState>::failure(mismatch("snapshot chunk is malformed"));
        }
        state.chunks.push_back(std::move(chunk));
    }
    int completedCount = 0;
    for (int i = 0; i < state.committedChunks; ++i) {
        const ChunkPlan& chunk = state.chunks[static_cast<std::size_t>(i)];
        if (!chunk.completed || chunk.contentHash.empty()) {
            return Result<SnapshotState>::failure(
                mismatch("snapshot committed prefix is incomplete"));
        }
        ++completedCount;
    }
    for (std::size_t i = static_cast<std::size_t>(state.committedChunks);
         i < state.chunks.size(); ++i) {
        if (state.chunks[i].completed) {
            return Result<SnapshotState>::failure(
                mismatch("snapshot has a non-contiguous completed chunk"));
        }
    }
    (void)completedCount;
    if (buildChain(state.configHash, state.chunks, state.committedChunks) != state.hashChain) {
        return Result<SnapshotState>::failure(mismatch("snapshot hash chain mismatch"));
    }
    return Result<SnapshotState>::success(std::move(state));
}

}  // namespace

std::string snapshotConfigHash(const SnapshotConfig& config) {
    return Sha256::hex(configJson(config).dump());
}

std::string snapshotHashChain(const std::string& configHash,
                              const std::vector<ChunkPlan>& chunks) {
    int committed = 0;
    for (const auto& chunk : chunks) {
        if (!chunk.completed) {
            break;
        }
        ++committed;
    }
    return buildChain(configHash, chunks, committed);
}

Json snapshotToJson(const SnapshotState& state) {
    Json root = Json::object();
    root["format"] = Json("font-atlas-snapshot");
    root["version"] = Json(1);
    root["config"] = configJson(state.config);
    root["configHash"] = Json(state.configHash);
    root["committedChunks"] = Json(state.committedChunks);
    root["hashChain"] = Json(state.hashChain);
    Json chunks = Json::array();
    for (const auto& chunk : state.chunks) {
        Json item = Json::object();
        item["index"] = Json(chunk.index);
        item["parentIndex"] = Json(chunk.parentIndex);
        item["splitLevel"] = Json(chunk.splitLevel);
        item["samples"] = Json(chunk.samples);
        item["batchSize"] = Json(chunk.batchSize);
        item["estimatedBytes"] = Json(chunk.estimatedBytes);
        item["contentHash"] = Json(chunk.contentHash);
        item["completed"] = Json(chunk.completed);
        Json rect = Json::object();
        rect["x"] = Json(chunk.rect.x);
        rect["y"] = Json(chunk.rect.y);
        rect["width"] = Json(chunk.rect.width);
        rect["height"] = Json(chunk.rect.height);
        item["rect"] = rect;
        chunks.pushBack(item);
    }
    root["chunks"] = chunks;
    return root;
}

Result<void> saveSnapshot(const std::filesystem::path& directory, SnapshotState state) {
    auto directoryResult = ensureDirectory(directory);
    if (!directoryResult.ok()) {
        return directoryResult;
    }
    state.configHash = snapshotConfigHash(state.config);
    state.committedChunks = 0;
    for (const auto& chunk : state.chunks) {
        if (!chunk.completed) {
            break;
        }
        ++state.committedChunks;
    }
    state.hashChain = buildChain(state.configHash, state.chunks, state.committedChunks);
    return writeFileAtomic(directory / "snapshot.json", snapshotToJson(state).dump(2));
}

Result<std::optional<SnapshotState>> loadSnapshot(const std::filesystem::path& directory,
                                                  const std::string& expectedConfigHash) {
    const auto path = directory / "snapshot.json";
    std::error_code existsError;
    if (!std::filesystem::exists(path, existsError)) {
        if (existsError) {
            Diagnostic diagnostic;
            diagnostic.code = ErrorCode::IoError;
            diagnostic.message = "cannot inspect snapshot path: " + pathString(path);
            return Result<std::optional<SnapshotState>>::failure(std::move(diagnostic));
        }
        return Result<std::optional<SnapshotState>>::success(std::nullopt);
    }
    auto text = readTextFile(path);
    if (!text.ok()) {
        return Result<std::optional<SnapshotState>>::failure(std::move(text.error));
    }
    auto parsedJson = Json::parse(*text.value);
    if (!parsedJson.ok()) {
        return Result<std::optional<SnapshotState>>::failure(
            mismatch("snapshot JSON cannot be parsed: " + parsedJson.error.message));
    }
    auto parsed = parseSnapshot(*parsedJson.value);
    if (!parsed.ok()) {
        return Result<std::optional<SnapshotState>>::failure(std::move(parsed.error));
    }
    if (parsed.value->configHash != expectedConfigHash) {
        return Result<std::optional<SnapshotState>>::failure(
            mismatch("snapshot configuration does not match this raster request"));
    }
    for (int i = 0; i < parsed.value->committedChunks; ++i) {
        const ChunkPlan& chunk = parsed.value->chunks[static_cast<std::size_t>(i)];
        auto bytes = loadChunk(directory, i);
        if (!bytes.ok()) {
            return Result<std::optional<SnapshotState>>::failure(
                mismatch("committed chunk " + std::to_string(i) +
                         " cannot be loaded: " + bytes.error.message));
        }
        if (Sha256::hex(bytes.value->data(), bytes.value->size()) != chunk.contentHash) {
            return Result<std::optional<SnapshotState>>::failure(
                mismatch("committed chunk " + std::to_string(i) + " content hash mismatch"));
        }
        if (bytes.value->size() !=
            static_cast<std::size_t>(chunk.rect.area())) {
            return Result<std::optional<SnapshotState>>::failure(
                mismatch("committed chunk " + std::to_string(i) + " has invalid size"));
        }
    }
    return Result<std::optional<SnapshotState>>::success(std::move(parsed.value));
}

Result<void> saveChunk(const std::filesystem::path& directory, int index,
                       const std::vector<std::uint8_t>& coverage) {
    auto result = ensureDirectory(directory / "chunks");
    if (!result.ok()) {
        return result;
    }
    return writeFileAtomic(directory / "chunks" /
                               (std::to_string(index) + ".cov"),
                           coverage);
}

Result<std::vector<std::uint8_t>> loadChunk(const std::filesystem::path& directory, int index) {
    return readBinaryFile(directory / "chunks" / (std::to_string(index) + ".cov"));
}

}  // namespace font_atlas
