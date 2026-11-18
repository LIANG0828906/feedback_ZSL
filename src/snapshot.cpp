#include "snapshot.h"

#include <algorithm>
#include <cmath>
#include <filesystem>
#include <iomanip>
#include <limits>
#include <sstream>

#include "json.h"
#include "sha256.h"
#include "util.h"

namespace caisson {
namespace {

std::string blockFileName(int blockIndex) {
    std::ostringstream out;
    out << "block_" << std::setw(6) << std::setfill('0') << blockIndex << ".bin";
    return out.str();
}

std::uint64_t u64Value(const json::Value& value, const std::string& field) {
    if (!value.isNumber() || value.number < 0.0 ||
        std::floor(value.number) != value.number ||
        value.number > 9007199254740991.0) {
        throw std::runtime_error("invalid snapshot integer field: " + field);
    }
    return static_cast<std::uint64_t>(value.number);
}

int int32Value(const json::Value& value, const std::string& field) {
    const std::uint64_t raw = u64Value(value, field);
    if (raw > static_cast<std::uint64_t>(std::numeric_limits<int>::max())) {
        throw std::runtime_error("snapshot integer field is too large: " + field);
    }
    return static_cast<int>(raw);
}

const json::Value& required(const json::Value& parent, const char* key) {
    const json::Value* value = parent.find(key);
    if (value == nullptr) {
        throw std::runtime_error(std::string("missing snapshot field: ") + key);
    }
    return *value;
}

std::string stringValue(const json::Value& value, const std::string& field) {
    if (!value.isString()) {
        throw std::runtime_error("invalid snapshot string field: " + field);
    }
    return value.string;
}

std::vector<std::uint64_t> uintArray(
    const json::Value& value,
    std::size_t expected,
    const std::string& field) {
    if (!value.isArray() || value.array.size() != expected) {
        throw std::runtime_error("invalid snapshot array field: " + field);
    }
    std::vector<std::uint64_t> output;
    output.reserve(expected);
    for (const json::Value& item : value.array) {
        output.push_back(u64Value(item, field));
    }
    return output;
}

void appendU64(std::ostringstream& out, std::uint64_t value) {
    out << value;
}

std::string snapshotDocument(
    const Scene& scene,
    const std::vector<SnapshotBlock>& blocks) {
    std::ostringstream out;
    out << "{\n";
    out << "  \"snapshot_version\": 1,\n";
    out << "  \"scene_digest\": " << quote(scene.digest) << ",\n";
    out << "  \"image_width\": " << scene.image.width << ",\n";
    out << "  \"image_height\": " << scene.image.height << ",\n";
    out << "  \"tile_size\": " << scene.image.tileSize << ",\n";
    out << "  \"aa_samples\": " << scene.image.aaSamples << ",\n";
    out << "  \"block_columns\": " << scene.blockColumns() << ",\n";
    out << "  \"block_rows\": " << scene.blockRows() << ",\n";
    out << "  \"layer_count\": " << scene.layers.size() << ",\n";
    out << "  \"completed_blocks\": [\n";
    for (std::size_t i = 0; i < blocks.size(); ++i) {
        const SnapshotBlock& block = blocks[i];
        if (i != 0) out << ",\n";
        out << "    {\"block_index\":" << block.rect.index
            << ",\"block_x\":" << block.rect.x
            << ",\"block_y\":" << block.rect.y
            << ",\"width\":" << block.rect.width
            << ",\"height\":" << block.rect.height
            << ",\"file\":" << quote(block.relativeFile)
            << ",\"byte_size\":" << block.bytes.size()
            << ",\"sha256\":" << quote(block.sha256)
            << ",\"color_sums\":[";
        for (int channel = 0; channel < 4; ++channel) {
            if (channel != 0) out << ',';
            out << block.colorSums[static_cast<std::size_t>(channel)];
        }
        out << "],\"owner_sums\":[";
        for (int channel = 0; channel < 4; ++channel) {
            if (channel != 0) out << ',';
            out << block.ownerSums[static_cast<std::size_t>(channel)];
        }
        out << "],\"transmit_energy\":[";
        for (std::size_t layer = 0; layer < block.stats.transmitEnergy.size(); ++layer) {
            if (layer != 0) out << ',';
            out << block.stats.transmitEnergy[layer];
        }
        out << "],\"reflect_energy\":[";
        for (std::size_t layer = 0; layer < block.stats.reflectEnergy.size(); ++layer) {
            if (layer != 0) out << ',';
            out << block.stats.reflectEnergy[layer];
        }
        out << "],\"diffuse_energy\":[";
        for (std::size_t layer = 0; layer < block.stats.diffuseEnergy.size(); ++layer) {
            if (layer != 0) out << ',';
            out << block.stats.diffuseEnergy[layer];
        }
        out << "],\"layer_samples\":[";
        for (std::size_t layer = 0; layer < block.stats.layerSamples.size(); ++layer) {
            if (layer != 0) out << ',';
            out << block.stats.layerSamples[layer];
        }
        out << "],\"covered_samples\":" << block.stats.coveredSamples
            << ",\"wood_samples\":" << block.stats.woodSamples
            << ",\"gauze_samples\":" << block.stats.gauzeSamples
            << ",\"occluder_samples\":" << block.stats.occluderSamples
            << '}';
    }
    out << "\n  ],\n";
    out << "  \"snapshot_checksum\": " << quote(snapshotSignature(scene, blocks)) << "\n";
    out << "}\n";
    return out.str();
}

void discard(
    const std::filesystem::path& outputDirectory,
    std::string& error) {
    std::error_code ec;
    std::filesystem::remove(outputDirectory / "snapshot.json", ec);
    ec.clear();
    std::filesystem::remove_all(outputDirectory / "blocks", ec);
    error.clear();
}

SnapshotBlock parseBlock(
    const Scene& scene,
    const json::Value& value,
    int expectedIndex) {
    const BlockRect expected = blockRect(scene, expectedIndex);
    SnapshotBlock block;
    block.rect.index = int32Value(required(value, "block_index"), "block_index");
    block.rect.x = int32Value(required(value, "block_x"), "block_x");
    block.rect.y = int32Value(required(value, "block_y"), "block_y");
    block.rect.width = int32Value(required(value, "width"), "width");
    block.rect.height = int32Value(required(value, "height"), "height");
    if (block.rect.index != expectedIndex ||
        block.rect.x != expected.x || block.rect.y != expected.y ||
        block.rect.width != expected.width || block.rect.height != expected.height) {
        throw std::runtime_error("snapshot block rectangle mismatch");
    }
    block.relativeFile = stringValue(required(value, "file"), "file");
    if (block.relativeFile != blockRelativePath(expectedIndex)) {
        throw std::runtime_error("snapshot block path mismatch");
    }
    const std::uint64_t byteSize = u64Value(required(value, "byte_size"), "byte_size");
    const std::uint64_t expectedBytes =
        static_cast<std::uint64_t>(expected.width) *
        static_cast<std::uint64_t>(expected.height) * 8U;
    if (byteSize != expectedBytes) {
        throw std::runtime_error("snapshot block byte size mismatch");
    }
    block.sha256 = stringValue(required(value, "sha256"), "sha256");
    const std::vector<std::uint64_t> colorSums =
        uintArray(required(value, "color_sums"), 4, "color_sums");
    const std::vector<std::uint64_t> ownerSums =
        uintArray(required(value, "owner_sums"), 4, "owner_sums");
    for (int channel = 0; channel < 4; ++channel) {
        block.colorSums[static_cast<std::size_t>(channel)] =
            colorSums[static_cast<std::size_t>(channel)];
        block.ownerSums[static_cast<std::size_t>(channel)] =
            ownerSums[static_cast<std::size_t>(channel)];
    }
    const std::size_t layerCount = scene.layers.size();
    const std::vector<std::uint64_t> transmit =
        uintArray(required(value, "transmit_energy"), layerCount, "transmit_energy");
    const std::vector<std::uint64_t> reflect =
        uintArray(required(value, "reflect_energy"), layerCount, "reflect_energy");
    const std::vector<std::uint64_t> diffuse =
        uintArray(required(value, "diffuse_energy"), layerCount, "diffuse_energy");
    const std::vector<std::uint64_t> layerSamples =
        uintArray(required(value, "layer_samples"), layerCount, "layer_samples");
    block.stats.resize(layerCount);
    block.stats.transmitEnergy = transmit;
    block.stats.reflectEnergy = reflect;
    block.stats.diffuseEnergy = diffuse;
    block.stats.layerSamples = layerSamples;
    block.stats.coveredSamples =
        u64Value(required(value, "covered_samples"), "covered_samples");
    block.stats.woodSamples =
        u64Value(required(value, "wood_samples"), "wood_samples");
    block.stats.gauzeSamples =
        u64Value(required(value, "gauze_samples"), "gauze_samples");
    block.stats.occluderSamples =
        u64Value(required(value, "occluder_samples"), "occluder_samples");
    return block;
}

}  // namespace

std::string blockRelativePath(int blockIndex) {
    return "blocks/" + blockFileName(blockIndex);
}

std::uint64_t sumChannels(
    const std::vector<std::uint8_t>& bytes,
    int offset,
    int stride) {
    std::uint64_t sum = 0;
    for (std::size_t i = static_cast<std::size_t>(offset);
         i < bytes.size();
         i += static_cast<std::size_t>(stride)) {
        sum += bytes[i];
    }
    return sum;
}

std::string snapshotSignature(
    const Scene& scene,
    const std::vector<SnapshotBlock>& blocks) {
    std::ostringstream out;
    out << "caisson-snapshot-v1|" << scene.digest << '|'
        << scene.image.width << '|' << scene.image.height << '|'
        << scene.image.tileSize << '|' << scene.image.aaSamples << '|'
        << scene.blockColumns() << '|' << scene.blockRows() << '|'
        << scene.layers.size() << '|';
    for (const SnapshotBlock& block : blocks) {
        out << block.rect.index << ',' << block.rect.x << ','
            << block.rect.y << ',' << block.rect.width << ','
            << block.rect.height << ',' << block.relativeFile << ','
            << block.bytes.size() << ',' << block.sha256 << ',';
        for (const std::uint64_t value : block.colorSums) appendU64(out, value), out << ',';
        for (const std::uint64_t value : block.ownerSums) appendU64(out, value), out << ',';
        for (const std::uint64_t value : block.stats.transmitEnergy) appendU64(out, value), out << ',';
        for (const std::uint64_t value : block.stats.reflectEnergy) appendU64(out, value), out << ',';
        for (const std::uint64_t value : block.stats.diffuseEnergy) appendU64(out, value), out << ',';
        for (const std::uint64_t value : block.stats.layerSamples) appendU64(out, value), out << ',';
        out << block.stats.coveredSamples << ',' << block.stats.woodSamples << ','
            << block.stats.gauzeSamples << ',' << block.stats.occluderSamples << '|';
    }
    return sha256Hex(out.str());
}

bool commitBlockSnapshot(
    const Scene& scene,
    const std::filesystem::path& outputDirectory,
    std::vector<SnapshotBlock>& blocks,
    BlockResult result,
    std::string& error) {
    SnapshotBlock block;
    block.rect = result.rect;
    block.relativeFile = blockRelativePath(result.rect.index);
    block.bytes = std::move(result.pixels);
    block.stats = std::move(result.stats);
    block.sha256 = sha256Hex(block.bytes);
    for (int channel = 0; channel < 4; ++channel) {
        block.colorSums[static_cast<std::size_t>(channel)] =
            sumChannels(block.bytes, channel, 8);
        block.ownerSums[static_cast<std::size_t>(channel)] =
            sumChannels(block.bytes, channel + 4, 8);
    }
    if (!atomicWriteBinary(outputDirectory / block.relativeFile, block.bytes, error)) {
        return false;
    }
    blocks.push_back(std::move(block));
    const std::string document = snapshotDocument(scene, blocks);
    return atomicWriteText(outputDirectory / "snapshot.json", document, error);
}

SnapshotLoadStatus loadSnapshot(
    const Scene& scene,
    const std::filesystem::path& outputDirectory,
    SnapshotState& state,
    std::string& error) {
    state = SnapshotState{};
    const std::filesystem::path snapshotPath = outputDirectory / "snapshot.json";
    std::error_code ec;
    if (!std::filesystem::exists(snapshotPath, ec)) {
        return SnapshotLoadStatus::None;
    }

    try {
        std::string text;
        std::string ioError;
        if (!readTextFile(snapshotPath, text, ioError)) {
            throw std::runtime_error(ioError);
        }
        const json::Value root = json::parse(text);
        if (!root.isObject()) throw std::runtime_error("snapshot root is not an object");
        if (int32Value(required(root, "snapshot_version"), "snapshot_version") != 1) {
            throw std::runtime_error("unsupported snapshot version");
        }
        if (stringValue(required(root, "scene_digest"), "scene_digest") != scene.digest) {
            throw std::runtime_error("scene digest changed");
        }
        if (int32Value(required(root, "image_width"), "image_width") !=
                scene.image.width ||
            int32Value(required(root, "image_height"), "image_height") !=
                scene.image.height ||
            int32Value(required(root, "tile_size"), "tile_size") !=
                scene.image.tileSize ||
            int32Value(required(root, "aa_samples"), "aa_samples") !=
                scene.image.aaSamples ||
            int32Value(required(root, "block_columns"), "block_columns") !=
                scene.blockColumns() ||
            int32Value(required(root, "block_rows"), "block_rows") !=
                scene.blockRows() ||
            u64Value(required(root, "layer_count"), "layer_count") !=
                scene.layers.size()) {
            throw std::runtime_error("snapshot dimensions/settings mismatch");
        }

        const json::Value& completed = required(root, "completed_blocks");
        if (!completed.isArray()) {
            throw std::runtime_error("completed_blocks is not an array");
        }
        if (completed.array.size() > static_cast<std::size_t>(scene.blockCount())) {
            throw std::runtime_error("too many completed blocks");
        }
        std::vector<SnapshotBlock> blocks;
        blocks.reserve(completed.array.size());
        for (std::size_t i = 0; i < completed.array.size(); ++i) {
            SnapshotBlock block = parseBlock(
                scene, completed.array[i], static_cast<int>(i));
            std::string readError;
            if (!readBinaryFile(
                    outputDirectory / block.relativeFile, block.bytes, readError)) {
                throw std::runtime_error(readError);
            }
            if (block.bytes.size() !=
                static_cast<std::size_t>(block.rect.width) *
                    static_cast<std::size_t>(block.rect.height) * 8U) {
                throw std::runtime_error("block file has wrong size");
            }
            if (sha256Hex(block.bytes) != block.sha256) {
                throw std::runtime_error("block SHA-256 mismatch");
            }
            for (int channel = 0; channel < 4; ++channel) {
                if (sumChannels(block.bytes, channel, 8) !=
                        block.colorSums[static_cast<std::size_t>(channel)] ||
                    sumChannels(block.bytes, channel + 4, 8) !=
                        block.ownerSums[static_cast<std::size_t>(channel)]) {
                    throw std::runtime_error("block color summary mismatch");
                }
            }
            blocks.push_back(std::move(block));
        }
        const std::string checksum =
            stringValue(required(root, "snapshot_checksum"), "snapshot_checksum");
        if (checksum != snapshotSignature(scene, blocks)) {
            throw std::runtime_error("snapshot checksum mismatch");
        }
        state.blocks = std::move(blocks);
        state.status = SnapshotLoadStatus::Resumed;
        return SnapshotLoadStatus::Resumed;
    } catch (const std::exception& exception) {
        discard(outputDirectory, error);
        state = SnapshotState{};
        state.status = SnapshotLoadStatus::DiscardedCorrupt;
        error.clear();
        (void)exception;
        return SnapshotLoadStatus::DiscardedCorrupt;
    }
}

bool clearSnapshot(
    const std::filesystem::path& outputDirectory,
    std::string& error) {
    error.clear();
    std::error_code ec;
    std::filesystem::remove(outputDirectory / "snapshot.json", ec);
    if (ec) {
        error = "cannot remove snapshot: " + ec.message();
        return false;
    }
    ec.clear();
    std::filesystem::remove_all(outputDirectory / "blocks", ec);
    if (ec) {
        error = "cannot remove block directory: " + ec.message();
        return false;
    }
    return true;
}

}  // namespace caisson

