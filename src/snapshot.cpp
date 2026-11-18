#include "snapshot.h"

#include "hash.h"
#include "output.h"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <filesystem>

namespace cr {
namespace {

std::int32_t readLittleInt32(const std::vector<std::uint8_t>& bytes, std::size_t offset) {
    const std::uint32_t value =
        static_cast<std::uint32_t>(bytes[offset]) |
        (static_cast<std::uint32_t>(bytes[offset + 1]) << 8u) |
        (static_cast<std::uint32_t>(bytes[offset + 2]) << 16u) |
        (static_cast<std::uint32_t>(bytes[offset + 3]) << 24u);
    return static_cast<std::int32_t>(value);
}

bool getInt(const json::Value& object, const char* key, int& out) {
    const json::Value* value = object.find(key);
    if (value == nullptr || !value->isNumber()) return false;
    const double number = value->asNumber();
    if (std::floor(number) != number) return false;
    out = static_cast<int>(number);
    return true;
}

bool getDouble(const json::Value& object, const char* key, double& out) {
    const json::Value* value = object.find(key);
    if (value == nullptr || !value->isNumber()) return false;
    out = value->asNumber();
    return std::isfinite(out);
}

bool getString(const json::Value& object, const char* key, std::string& out) {
    const json::Value* value = object.find(key);
    if (value == nullptr || !value->isString()) return false;
    out = value->asString();
    return true;
}

json::Value sceneSummary(const Scene& scene) {
    json::Value summary = json::Value::object();
    summary.set("name", json::Value(scene.name));
    summary.set("schema", json::Value(scene.schema));
    summary.set("width", json::Value(scene.render.width));
    summary.set("height", json::Value(scene.render.height));
    summary.set("tileSize", json::Value(scene.render.tileSize));
    summary.set("textureResolution", json::Value(scene.render.textureResolution));
    json::Value layers = json::Value::array();
    for (const Layer& layer : scene.layers) {
        json::Value item = json::Value::object();
        item.set("layer", json::Value(layer.index));
        item.set("z", json::Value(layer.z));
        item.set("thickness", json::Value(layer.thickness));
        json::Value boards = json::Value::array();
        for (const Board& board : layer.boards) {
            json::Value boardJson = json::Value::object();
            boardJson.set("id", json::Value(board.id));
            boardJson.set("seed", json::Value(
                                     static_cast<double>(board.seed)));
            boardJson.set("transmittance", json::Value(board.transmittance));
            boards.push(std::move(boardJson));
        }
        item.set("boards", std::move(boards));
        json::Value brackets = json::Value::array();
        for (const Bracket& bracket : layer.brackets) {
            json::Value bracketJson = json::Value::object();
            bracketJson.set("id", json::Value(bracket.id));
            bracketJson.set("reflectance", json::Value(bracket.reflectance));
            bracketJson.set("thickness", json::Value(bracket.thickness));
            brackets.push(std::move(bracketJson));
        }
        item.set("brackets", std::move(brackets));
        layers.push(std::move(item));
    }
    summary.set("layers", std::move(layers));
    json::Value lights = json::Value::array();
    for (const Light& light : scene.lights) {
        json::Value item = json::Value::object();
        item.set("id", json::Value(light.id));
        json::Value position = json::Value::array();
        position.push(json::Value(light.position.x));
        position.push(json::Value(light.position.y));
        position.push(json::Value(light.position.z));
        item.set("position", std::move(position));
        lights.push(std::move(item));
    }
    summary.set("lights", std::move(lights));
    return summary;
}

json::Value tileToJson(const TileData& tile) {
    json::Value item = tile.summaryJson();
    item.set("rgbaBase64", json::Value(base64Encode(tile.rgba)));
    std::vector<std::uint8_t> ownerBytes;
    ownerBytes.reserve(tile.owners.size() * 4);
    for (const int owner : tile.owners) {
        const std::uint32_t value = static_cast<std::uint32_t>(owner);
        ownerBytes.push_back(static_cast<std::uint8_t>(value & 0xffu));
        ownerBytes.push_back(static_cast<std::uint8_t>((value >> 8u) & 0xffu));
        ownerBytes.push_back(static_cast<std::uint8_t>((value >> 16u) & 0xffu));
        ownerBytes.push_back(static_cast<std::uint8_t>((value >> 24u) & 0xffu));
    }
    item.set("ownerBase64", json::Value(base64Encode(ownerBytes)));
    return item;
}

bool parseLayerStats(const json::Value& statsValue, BlockStats& stats) {
    if (!statsValue.isObject()) return false;
    const json::Value* layersValue = statsValue.find("layers");
    if (layersValue == nullptr || !layersValue->isArray()) return false;
    int rays = 0;
    if (getInt(statsValue, "rays", rays)) stats.rays = rays;
    stats.layers.clear();
    for (const json::Value& item : layersValue->asArray()) {
        if (!item.isObject()) return false;
        LayerEnergyStats layer;
        if (!getInt(item, "layer", layer.layerIndex)) return false;
        if (!getDouble(item, "reflectedContribution", layer.reflected)) return false;
        if (!getDouble(item, "transmittedContribution", layer.transmitted)) return false;
        const json::Value* samplesValue = item.find("samples");
        if (samplesValue == nullptr || !samplesValue->isNumber()) return false;
        layer.samples = static_cast<long long>(samplesValue->asNumber());
        stats.layers.push_back(layer);
    }
    return true;
}

bool parseTile(const json::Value& item, const Scene& scene, int expectedIndex,
               TileData& tile, std::string& reason) {
    if (!item.isObject()) {
        reason = "block entry is not an object";
        return false;
    }
    if (!getInt(item, "index", tile.index) || tile.index != expectedIndex) {
        reason = "block index sequence mismatch at " + std::to_string(expectedIndex);
        return false;
    }
    if (!getInt(item, "x", tile.x) || !getInt(item, "y", tile.y) ||
        !getInt(item, "width", tile.width) || !getInt(item, "height", tile.height)) {
        reason = "block geometry fields are invalid";
        return false;
    }
    const int blocksX = scene.blocksX();
    const int expectedX = (expectedIndex % blocksX) * scene.render.tileSize;
    const int expectedY = (expectedIndex / blocksX) * scene.render.tileSize;
    const int expectedWidth =
        std::min(scene.render.tileSize, scene.render.width - expectedX);
    const int expectedHeight =
        std::min(scene.render.tileSize, scene.render.height - expectedY);
    if (tile.x != expectedX || tile.y != expectedY ||
        tile.width != expectedWidth || tile.height != expectedHeight) {
        reason = "block offset mismatch at " + std::to_string(expectedIndex);
        return false;
    }
    std::string rgbaText;
    std::string ownerText;
    if (!getString(item, "rgbaBase64", rgbaText) ||
        !getString(item, "ownerBase64", ownerText)) {
        reason = "missing block payload at " + std::to_string(expectedIndex);
        return false;
    }
    std::vector<std::uint8_t> ownerBytes;
    if (!base64Decode(rgbaText, tile.rgba) || !base64Decode(ownerText, ownerBytes)) {
        reason = "invalid base64 payload at " + std::to_string(expectedIndex);
        return false;
    }
    const std::size_t pixelCount = static_cast<std::size_t>(tile.width) *
                                   static_cast<std::size_t>(tile.height);
    if (tile.rgba.size() != pixelCount * 4u || ownerBytes.size() != pixelCount * 4u) {
        reason = "payload length mismatch at " + std::to_string(expectedIndex);
        return false;
    }
    tile.owners.resize(pixelCount);
    for (std::size_t i = 0; i < pixelCount; ++i) {
        tile.owners[i] = readLittleInt32(ownerBytes, i * 4);
    }
    std::string expectedDigest;
    if (!getString(item, "rgbaDigest", expectedDigest) ||
        expectedDigest != tile.rgbaDigest()) {
        reason = "RGBA digest mismatch at " + std::to_string(expectedIndex);
        return false;
    }
    if (!getString(item, "ownerDigest", expectedDigest) ||
        expectedDigest != tile.ownerDigest()) {
        reason = "owner digest mismatch at " + std::to_string(expectedIndex);
        return false;
    }
    const json::Value* statsValue = item.find("stats");
    if (statsValue == nullptr || !parseLayerStats(*statsValue, tile.stats)) {
        reason = "block stats are invalid at " + std::to_string(expectedIndex);
        return false;
    }
    if (tile.stats.layers.size() != scene.layers.size()) {
        reason = "block stats layer count mismatch at " + std::to_string(expectedIndex);
        return false;
    }
    return true;
}

json::Value snapshotBase(const Scene& scene, const std::vector<TileData>& tiles) {
    json::Value root = json::Value::object();
    root.set("format", json::Value("coffered-snapshot/v1"));
    root.set("sceneDigest", json::Value(scene.digest));
    root.set("width", json::Value(scene.render.width));
    root.set("height", json::Value(scene.render.height));
    root.set("tileSize", json::Value(scene.render.tileSize));
    root.set("totalBlocks", json::Value(scene.totalBlocks()));
    root.set("completedBlockCount", json::Value(static_cast<int>(tiles.size())));
    root.set("sceneSummary", sceneSummary(scene));
    json::Value blocks = json::Value::array();
    for (const TileData& tile : tiles) blocks.push(tileToJson(tile));
    root.set("blocks", std::move(blocks));
    return root;
}

}  // namespace

json::Value snapshotManifest(const Scene& scene, const std::vector<TileData>& tiles) {
    json::Value root = snapshotBase(scene, tiles);
    root.set("checksum", json::Value(sha256Hex(root.canonical())));
    return root;
}

bool saveSnapshot(const std::string& path, const Scene& scene,
                  const std::vector<TileData>& tiles, std::string& error) {
    const json::Value manifest = snapshotManifest(scene, tiles);
    return writeTextAtomic(path, manifest.pretty(2), error);
}

SnapshotLoadResult loadSnapshot(const std::string& path, const Scene& scene) {
    SnapshotLoadResult result;
    std::string text;
    std::string error;
    if (!readTextFile(path, text, error)) {
        if (std::filesystem::exists(path)) {
            result.present = true;
            result.discarded = true;
            result.reason = error;
            discardSnapshot(path);
        }
        return result;
    }
    result.present = true;
    json::Value root;
    try {
        root = json::parse(text);
    } catch (const std::exception& exception) {
        result.discarded = true;
        result.reason = std::string("snapshot JSON parse failed: ") + exception.what();
        discardSnapshot(path);
        return result;
    }
    if (!root.isObject()) {
        result.discarded = true;
        result.reason = "snapshot root is not an object";
        discardSnapshot(path);
        return result;
    }
    std::string format;
    std::string sceneDigest;
    std::string checksum;
    int width = 0;
    int height = 0;
    int tileSize = 0;
    int totalBlocks = 0;
    int completedCount = 0;
    if (!getString(root, "format", format) || format != "coffered-snapshot/v1" ||
        !getString(root, "sceneDigest", sceneDigest) ||
        !getString(root, "checksum", checksum) ||
        !getInt(root, "width", width) || !getInt(root, "height", height) ||
        !getInt(root, "tileSize", tileSize) ||
        !getInt(root, "totalBlocks", totalBlocks) ||
        !getInt(root, "completedBlockCount", completedCount)) {
        result.discarded = true;
        result.reason = "snapshot header is incomplete";
        discardSnapshot(path);
        return result;
    }
    if (sceneDigest != scene.digest || width != scene.render.width ||
        height != scene.render.height || tileSize != scene.render.tileSize ||
        totalBlocks != scene.totalBlocks()) {
        result.discarded = true;
        result.reason = "scene digest or render tiling changed";
        discardSnapshot(path);
        return result;
    }
    json::Value checksumBase = root;
    checksumBase.asObject().erase("checksum");
    const std::string expectedChecksum = sha256Hex(checksumBase.canonical());
    if (checksum != expectedChecksum) {
        result.discarded = true;
        result.reason = "snapshot checksum mismatch";
        discardSnapshot(path);
        return result;
    }
    const json::Value* blocksValue = root.find("blocks");
    if (blocksValue == nullptr || !blocksValue->isArray() ||
        static_cast<int>(blocksValue->asArray().size()) != completedCount ||
        completedCount < 0 || completedCount > scene.totalBlocks()) {
        result.discarded = true;
        result.reason = "snapshot block count is invalid";
        discardSnapshot(path);
        return result;
    }
    result.tiles.reserve(static_cast<std::size_t>(completedCount));
    for (int i = 0; i < completedCount; ++i) {
        TileData tile;
        std::string reason;
        if (!parseTile(blocksValue->asArray()[static_cast<std::size_t>(i)], scene, i, tile,
                       reason)) {
            result.discarded = true;
            result.reason = reason;
            result.tiles.clear();
            discardSnapshot(path);
            return result;
        }
        result.tiles.push_back(std::move(tile));
    }
    result.valid = true;
    return result;
}

void discardSnapshot(const std::string& path) {
    removeFileIfExists(path);
    removeFileIfExists(path + ".tmp");
}

}  // namespace cr
