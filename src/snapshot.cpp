#include "snapshot.hpp"

#include <algorithm>
#include <cerrno>
#include <cmath>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <sstream>
#include <stdexcept>

#if defined(_WIN32)
#include <fcntl.h>
#include <io.h>
#include <windows.h>
#else
#include <fcntl.h>
#include <unistd.h>
#endif

namespace sl {
namespace {

const char* kSnapshotFormat = "silklight.snapshot.v1";

sj::Value metricsToJson(const std::vector<LayerMetric>& metrics) {
    sj::Value array = sj::Value::array();
    for (const LayerMetric& metric : metrics) {
        sj::Value item = sj::Value::object();
        item["layer"] = sj::Value(metric.layerIndex);
        item["separationId"] = sj::Value(metric.separationId);
        item["transmitted"] = sj::Value(metric.transmitted);
        item["reflected"] = sj::Value(metric.reflected);
        item["coverage"] = sj::Value(metric.coverage);
        item["samples"] = sj::Value(metric.samples);
        array.push(std::move(item));
    }
    return array;
}

bool metricsFromJson(const sj::Value* value, std::vector<LayerMetric>& metrics) {
    if (value == nullptr || !value->isArray()) return false;
    metrics.clear();
    for (const sj::Value& item : value->asArray()) {
        if (!item.isObject()) return false;
        const sj::Value* layer = item.find("layer");
        const sj::Value* separation = item.find("separationId");
        const sj::Value* transmitted = item.find("transmitted");
        const sj::Value* reflected = item.find("reflected");
        const sj::Value* coverage = item.find("coverage");
        const sj::Value* samples = item.find("samples");
        if (layer == nullptr || separation == nullptr || transmitted == nullptr ||
            reflected == nullptr || coverage == nullptr || samples == nullptr ||
            !layer->isNumber() || !separation->isString() || !transmitted->isNumber() ||
            !reflected->isNumber() || !coverage->isNumber() || !samples->isNumber()) {
            return false;
        }
        LayerMetric metric;
        metric.layerIndex = static_cast<int>(layer->asNumber());
        metric.separationId = separation->asString();
        metric.transmitted = transmitted->asNumber();
        metric.reflected = reflected->asNumber();
        metric.coverage = coverage->asNumber();
        metric.samples = samples->asNumber();
        metrics.push_back(std::move(metric));
    }
    return true;
}

bool writeAndSync(const std::string& path, const std::string& contents) {
    {
        std::ofstream output(path, std::ios::binary | std::ios::trunc);
        if (!output) return false;
        output.write(contents.data(), static_cast<std::streamsize>(contents.size()));
        output.flush();
        if (!output) return false;
    }
#if defined(_WIN32)
    const int fd = _open(path.c_str(), _O_RDONLY | _O_BINARY);
    if (fd >= 0) {
        _commit(fd);
        _close(fd);
    }
#else
    const int fd = ::open(path.c_str(), O_RDONLY);
    if (fd >= 0) {
        ::fsync(fd);
        ::close(fd);
    }
#endif
    return true;
}

void syncParentDirectory(const std::filesystem::path& path) {
#if !defined(_WIN32)
    const std::filesystem::path parent =
        path.parent_path().empty() ? std::filesystem::path(".") : path.parent_path();
#if defined(O_DIRECTORY)
    const int fd = ::open(parent.c_str(), O_RDONLY | O_DIRECTORY);
#else
    const int fd = ::open(parent.c_str(), O_RDONLY);
#endif
    if (fd >= 0) {
        ::fsync(fd);
        ::close(fd);
    }
#else
    (void)path;
#endif
}

} // namespace

std::string base64Encode(const std::vector<std::uint8_t>& bytes) {
    static const char table[] =
        "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    std::string out;
    out.reserve((bytes.size() + 2) / 3 * 4);
    std::size_t i = 0;
    while (i + 2 < bytes.size()) {
        const std::uint32_t value =
            (static_cast<std::uint32_t>(bytes[i]) << 16u) |
            (static_cast<std::uint32_t>(bytes[i + 1]) << 8u) |
            static_cast<std::uint32_t>(bytes[i + 2]);
        out.push_back(table[(value >> 18u) & 63u]);
        out.push_back(table[(value >> 12u) & 63u]);
        out.push_back(table[(value >> 6u) & 63u]);
        out.push_back(table[value & 63u]);
        i += 3;
    }
    if (i + 1 == bytes.size()) {
        const std::uint32_t value = static_cast<std::uint32_t>(bytes[i]) << 16u;
        out.push_back(table[(value >> 18u) & 63u]);
        out.push_back(table[(value >> 12u) & 63u]);
        out += "==";
    } else if (i + 2 == bytes.size()) {
        const std::uint32_t value =
            (static_cast<std::uint32_t>(bytes[i]) << 16u) |
            (static_cast<std::uint32_t>(bytes[i + 1]) << 8u);
        out.push_back(table[(value >> 18u) & 63u]);
        out.push_back(table[(value >> 12u) & 63u]);
        out.push_back(table[(value >> 6u) & 63u]);
        out.push_back('=');
    }
    return out;
}

bool base64Decode(const std::string& text, std::vector<std::uint8_t>& bytes) {
    auto inverse = [](char c) -> int {
        if (c >= 'A' && c <= 'Z') return c - 'A';
        if (c >= 'a' && c <= 'z') return c - 'a' + 26;
        if (c >= '0' && c <= '9') return c - '0' + 52;
        if (c == '+') return 62;
        if (c == '/') return 63;
        return -1;
    };
    if (text.size() % 4 != 0) return false;
    bytes.clear();
    bytes.reserve(text.size() / 4 * 3);
    for (std::size_t i = 0; i < text.size(); i += 4) {
        int values[4] = {0, 0, 0, 0};
        int padding = 0;
        for (int j = 0; j < 4; ++j) {
            char c = text[i + static_cast<std::size_t>(j)];
            if (c == '=') {
                if (j < 2 || i + 4 != text.size()) return false;
                ++padding;
                values[j] = 0;
            } else {
                if (padding != 0) return false;
                values[j] = inverse(c);
                if (values[j] < 0) return false;
            }
        }
        const std::uint32_t value =
            (static_cast<std::uint32_t>(values[0]) << 18u) |
            (static_cast<std::uint32_t>(values[1]) << 12u) |
            (static_cast<std::uint32_t>(values[2]) << 6u) |
            static_cast<std::uint32_t>(values[3]);
        bytes.push_back(static_cast<std::uint8_t>((value >> 16u) & 0xFFu));
        if (padding < 2) bytes.push_back(static_cast<std::uint8_t>((value >> 8u) & 0xFFu));
        if (padding < 1) bytes.push_back(static_cast<std::uint8_t>(value & 0xFFu));
    }
    return true;
}

void writeFileAtomic(const std::string& path, const std::string& contents) {
    const std::filesystem::path target(path);
    if (!target.parent_path().empty()) {
        std::filesystem::create_directories(target.parent_path());
    }
    const std::filesystem::path temporary = target.string() + ".tmp";
    if (!writeAndSync(temporary.string(), contents)) {
        throw std::runtime_error("cannot write temporary file: " + temporary.string());
    }
#if defined(_WIN32)
    const std::wstring from = temporary.wstring();
    const std::wstring to = target.wstring();
    if (!MoveFileExW(from.c_str(), to.c_str(),
                     MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)) {
        std::filesystem::remove(temporary);
        throw std::runtime_error("atomic snapshot replacement failed");
    }
#else
    if (std::rename(temporary.c_str(), target.c_str()) != 0) {
        std::filesystem::remove(temporary);
        throw std::runtime_error("atomic snapshot replacement failed: " +
                                 std::string(std::strerror(errno)));
    }
#endif
    syncParentDirectory(target);
}

bool readFile(const std::string& path, std::string& contents) {
    std::ifstream input(path, std::ios::binary);
    if (!input) return false;
    std::ostringstream buffer;
    buffer << input.rdbuf();
    contents = buffer.str();
    return true;
}

SnapshotLoadResult SnapshotStore::load(const Renderer& renderer) const {
    SnapshotLoadResult result;
    if (!std::filesystem::exists(path_)) {
        result.reason = "no-snapshot";
        return result;
    }
    result.exists = true;
    std::string contents;
    if (!readFile(path_, contents)) {
        result.reason = "snapshot-unreadable";
        return result;
    }
    std::string parseError;
    sj::Value document = sj::Value::parse(contents, parseError);
    if (!parseError.empty() || !document.isObject()) {
        result.reason = "snapshot-corrupt-json";
        return result;
    }
    const sj::Value* format = document.find("format");
    const sj::Value* checksum = document.find("checksum");
    const sj::Value* payload = document.find("payload");
    if (format == nullptr || !format->isString() || format->asString() != kSnapshotFormat ||
        checksum == nullptr || !checksum->isString() || payload == nullptr ||
        !payload->isObject()) {
        result.reason = "snapshot-corrupt-envelope";
        return result;
    }
    const std::string payloadText = payload->dump();
    if (checksum->asString() != hex64(hashString(payloadText))) {
        result.reason = "snapshot-checksum-mismatch";
        return result;
    }
    result.documentDigest = hex64(hashString(contents));

    const Scene& scene = renderer.scene();
    const sj::Value* sceneDigest = payload->find("sceneDigest");
    const sj::Value* sceneSummary = payload->find("sceneSummary");
    const sj::Value* image = payload->find("image");
    const sj::Value* completed = payload->find("completedBlocks");
    if (sceneDigest == nullptr || !sceneDigest->isString() || sceneSummary == nullptr ||
        !sceneSummary->isObject() || image == nullptr || !image->isObject() ||
        completed == nullptr || !completed->isArray()) {
        result.reason = "snapshot-missing-fields";
        return result;
    }
    if (sceneDigest->asString() != scene.sceneDigest) {
        result.reason = "snapshot-scene-mismatch";
        return result;
    }
    if (sceneSummary->dump() != scene.sceneSummaryJson) {
        result.reason = "snapshot-summary-mismatch";
        return result;
    }
    const auto imageInt = [&](const char* name, int& out) {
        const sj::Value* value = image->find(name);
        if (value == nullptr || !value->isNumber() || std::floor(value->asNumber()) !=
                                                       value->asNumber()) {
            return false;
        }
        out = static_cast<int>(value->asNumber());
        return true;
    };
    int width = 0;
    int height = 0;
    int blockWidth = 0;
    int blockHeight = 0;
    if (!imageInt("width", width) || !imageInt("height", height) ||
        !imageInt("blockWidth", blockWidth) || !imageInt("blockHeight", blockHeight) ||
        width != scene.image.width || height != scene.image.height ||
        blockWidth != scene.image.blockWidth || blockHeight != scene.image.blockHeight) {
        result.reason = "snapshot-image-config-mismatch";
        return result;
    }

    SnapshotData data;
    data.sceneDigest = sceneDigest->asString();
    data.sceneSummaryJson = sceneSummary->dump();
    data.imageWidth = width;
    data.imageHeight = height;
    data.blockWidth = blockWidth;
    data.blockHeight = blockHeight;
    data.documentDigest = result.documentDigest;

    for (const sj::Value& item : completed->asArray()) {
        if (!item.isObject()) {
            result.reason = "snapshot-block-corrupt";
            return result;
        }
        const sj::Value* id = item.find("id");
        const sj::Value* blockW = item.find("width");
        const sj::Value* blockH = item.find("height");
        const sj::Value* rgbaDigest = item.find("rgbaDigest");
        const sj::Value* ownerDigest = item.find("ownerDigest");
        const sj::Value* rgbaText = item.find("rgbaBase64");
        const sj::Value* ownerText = item.find("ownerBase64");
        if (id == nullptr || !id->isNumber() || blockW == nullptr || !blockW->isNumber() ||
            blockH == nullptr || !blockH->isNumber() || rgbaDigest == nullptr ||
            !rgbaDigest->isString() || ownerDigest == nullptr || !ownerDigest->isString() ||
            rgbaText == nullptr || !rgbaText->isString() || ownerText == nullptr ||
            !ownerText->isString()) {
            result.reason = "snapshot-block-missing-fields";
            return result;
        }
        const int blockId = static_cast<int>(id->asNumber());
        if (blockId < 0 || blockId >= renderer.blockCount()) {
            result.reason = "snapshot-block-id-out-of-range";
            return result;
        }
        const BlockRect expected = renderer.blockRect(blockId);
        if (static_cast<int>(blockW->asNumber()) != expected.width ||
            static_cast<int>(blockH->asNumber()) != expected.height) {
            result.reason = "snapshot-block-geometry-mismatch";
            return result;
        }
        SnapshotBlockRecord block;
        block.id = blockId;
        block.width = expected.width;
        block.height = expected.height;
        if (!base64Decode(rgbaText->asString(), block.rgba) ||
            !base64Decode(ownerText->asString(), block.owner)) {
            result.reason = "snapshot-block-base64-invalid";
            return result;
        }
        if (block.rgba.size() !=
                static_cast<std::size_t>(block.width * block.height * 4) ||
            block.owner.size() != static_cast<std::size_t>(block.width * block.height)) {
            result.reason = "snapshot-block-length-mismatch";
            return result;
        }
        block.rgbaDigest = rgbaDigest->asString();
        block.ownerDigest = ownerDigest->asString();
        if (Renderer::rgbaDigest(block.rgba) != block.rgbaDigest) {
            result.reason = "snapshot-block-rgba-digest-mismatch";
            return result;
        }
        std::uint64_t ownerHash = 1469598103934665603ULL;
        for (std::uint8_t value : block.owner) {
            ownerHash ^= value;
            ownerHash *= 1099511628211ULL;
        }
        if (hex64(hashMix(ownerHash)) != block.ownerDigest) {
            result.reason = "snapshot-block-owner-digest-mismatch";
            return result;
        }
        if (!metricsFromJson(item.find("metrics"), block.metrics) ||
            block.metrics.size() != scene.layers.size()) {
            result.reason = "snapshot-block-metrics-invalid";
            return result;
        }
        if (data.completed.find(blockId) != data.completed.end()) {
            result.reason = "snapshot-duplicate-block";
            return result;
        }
        data.completed.emplace(blockId, std::move(block));
    }

    result.valid = true;
    result.reason = "ok";
    result.data = std::move(data);
    return result;
}

std::string SnapshotStore::commit(
    const Renderer& renderer,
    const std::map<int, SnapshotBlockRecord>& completed) const {
    const Scene& scene = renderer.scene();
    sj::Value payload = sj::Value::object();
    payload["format"] = sj::Value(kSnapshotFormat);
    payload["sceneDigest"] = sj::Value(scene.sceneDigest);
    std::string summaryError;
    payload["sceneSummary"] = sj::Value::parse(scene.sceneSummaryJson, summaryError);
    if (!summaryError.empty()) {
        throw std::runtime_error("invalid scene summary while committing snapshot");
    }
    payload["image"] = sj::Value::object();
    payload["image"]["width"] = sj::Value(scene.image.width);
    payload["image"]["height"] = sj::Value(scene.image.height);
    payload["image"]["blockWidth"] = sj::Value(scene.image.blockWidth);
    payload["image"]["blockHeight"] = sj::Value(scene.image.blockHeight);
    payload["committedBlockCount"] = sj::Value(static_cast<double>(completed.size()));
    payload["completedBlocks"] = sj::Value::array();
    for (const auto& entry : completed) {
        const SnapshotBlockRecord& block = entry.second;
        sj::Value item = sj::Value::object();
        item["id"] = sj::Value(block.id);
        item["width"] = sj::Value(block.width);
        item["height"] = sj::Value(block.height);
        item["rgbaDigest"] = sj::Value(block.rgbaDigest);
        item["ownerDigest"] = sj::Value(block.ownerDigest);
        item["rgbaBase64"] = sj::Value(base64Encode(block.rgba));
        item["ownerBase64"] = sj::Value(base64Encode(block.owner));
        item["metrics"] = metricsToJson(block.metrics);
        payload["completedBlocks"].push(std::move(item));
    }

    sj::Value document = sj::Value::object();
    document["format"] = sj::Value(kSnapshotFormat);
    document["checksum"] = sj::Value(hex64(hashString(payload.dump())));
    document["payload"] = std::move(payload);
    const std::string text = document.dump();
    writeFileAtomic(path_, text);
    return hex64(hashString(text));
}

void SnapshotStore::discard() const {
    std::error_code error;
    std::filesystem::remove(path_, error);
}

} // namespace sl

