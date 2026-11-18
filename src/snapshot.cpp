#include "snapshot.hpp"

#include "sha256.hpp"
#include "util.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <limits>
#include <set>

namespace sr {
namespace {

constexpr char kMagic[8] = {'S', 'R', 'S', 'N', 'A', 'P', '0', '1'};
constexpr uint32_t kSnapshotVersion = 1;
constexpr size_t kDigestSize = 32;

void appendDigest(std::vector<uint8_t>& out, const std::array<uint8_t, 32>& digest) {
    out.insert(out.end(), digest.begin(), digest.end());
}

bool readDigest(const std::vector<uint8_t>& in, size_t& offset,
                std::array<uint8_t, 32>& digest) {
    if (offset + kDigestSize > in.size()) {
        return false;
    }
    std::memcpy(digest.data(), in.data() + offset, kDigestSize);
    offset += kDigestSize;
    return true;
}

std::array<uint8_t, 32> digestForText(const std::string& text) {
    return Sha256::hash(text);
}

bool validTilePayload(const TileResult& tile, int imageWidth, int imageHeight,
                      int tileWidth, int tileHeight, int tileCols, int totalTiles) {
    if (tile.rect.index < 0 || tile.rect.index >= totalTiles ||
        tile.rect.width <= 0 || tile.rect.height <= 0 ||
        tile.rect.x < 0 || tile.rect.y < 0 ||
        tile.rect.x + tile.rect.width > imageWidth ||
        tile.rect.y + tile.rect.height > imageHeight) {
        return false;
    }
    const int expectedX = (tile.rect.index % tileCols) * tileWidth;
    const int expectedY = (tile.rect.index / tileCols) * tileHeight;
    if (tile.rect.x != expectedX || tile.rect.y != expectedY) {
        return false;
    }
    const int expectedWidth = std::min(tileWidth, imageWidth - expectedX);
    const int expectedHeight = std::min(tileHeight, imageHeight - expectedY);
    if (tile.rect.width != expectedWidth || tile.rect.height != expectedHeight) {
        return false;
    }
    const size_t pixels =
        static_cast<size_t>(tile.rect.width) * static_cast<size_t>(tile.rect.height);
    if (tile.finalRgba.size() != pixels * 4U ||
        tile.smokeRgba.size() != pixels * 4U ||
        tile.ownership.size() != pixels) {
        return false;
    }
    if (!isFinite(tile.stats.transmittanceSum) ||
        tile.stats.transmittanceSum < 0.0) {
        return false;
    }
    return true;
}

}  // namespace

SnapshotLoadResult SnapshotStore::load(const Scene& scene, int frame,
                                       const Renderer& renderer) const {
    (void)frame;
    SnapshotLoadResult result;
    std::error_code ec;
    if (!std::filesystem::exists(path_, ec) || ec) {
        result.state = SnapshotLoadState::Missing;
        return result;
    }
    std::vector<uint8_t> bytes;
    std::string readError;
    if (!readFileBytes(path_, bytes, readError)) {
        result.state = SnapshotLoadState::DiscardedCorrupt;
        result.detail = readError;
        return result;
    }
    if (bytes.size() < 8U + kDigestSize || std::memcmp(bytes.data(), kMagic, 8U) != 0) {
        result.state = SnapshotLoadState::DiscardedCorrupt;
        result.detail = "snapshot magic or size is invalid";
        return result;
    }
    const size_t bodySize = bytes.size() - kDigestSize;
    const std::array<uint8_t, 32> expectedBodyDigest =
        Sha256::hash(bytes.data(), bodySize);
    if (std::memcmp(expectedBodyDigest.data(), bytes.data() + bodySize, kDigestSize) != 0) {
        result.state = SnapshotLoadState::DiscardedCorrupt;
        result.detail = "snapshot checksum mismatch";
        return result;
    }

    size_t offset = 8U;
    uint32_t version = 0;
    uint32_t width = 0;
    uint32_t height = 0;
    uint32_t tileWidth = 0;
    uint32_t tileHeight = 0;
    uint32_t tileCols = 0;
    uint32_t tileRows = 0;
    uint32_t totalTiles = 0;
    uint32_t completed = 0;
    std::array<uint8_t, 32> sceneDigest{};
    std::array<uint8_t, 32> renderDigest{};
    if (!readU32(bytes, offset, version) || version != kSnapshotVersion ||
        !readU32(bytes, offset, width) || !readU32(bytes, offset, height) ||
        !readU32(bytes, offset, tileWidth) || !readU32(bytes, offset, tileHeight) ||
        !readU32(bytes, offset, tileCols) || !readU32(bytes, offset, tileRows) ||
        !readU32(bytes, offset, totalTiles) || !readU32(bytes, offset, completed) ||
        !readDigest(bytes, offset, sceneDigest) ||
        !readDigest(bytes, offset, renderDigest)) {
        result.state = SnapshotLoadState::DiscardedCorrupt;
        result.detail = "snapshot header is truncated or has an unsupported version";
        return result;
    }
    const std::array<uint8_t, 32> expectedRenderDigest =
        digestForText(renderer.renderDigest());
    const bool dimensionsMatch =
        width == static_cast<uint32_t>(scene.image.width) &&
        height == static_cast<uint32_t>(scene.image.height) &&
        tileWidth == static_cast<uint32_t>(scene.image.tileWidth) &&
        tileHeight == static_cast<uint32_t>(scene.image.tileHeight) &&
        tileCols == static_cast<uint32_t>(renderer.tileColumns()) &&
        tileRows == static_cast<uint32_t>(renderer.tileRows()) &&
        totalTiles == static_cast<uint32_t>(renderer.tileCount());
    const bool contentMatches =
        std::memcmp(sceneDigest.data(), scene.digest.data(), kDigestSize) == 0 &&
        std::memcmp(renderDigest.data(), expectedRenderDigest.data(), kDigestSize) == 0;
    if (!dimensionsMatch || !contentMatches) {
        result.state = SnapshotLoadState::DiscardedMismatch;
        result.detail = !contentMatches ? "scene or render configuration changed"
                                        : "snapshot dimensions do not match the scene";
        return result;
    }
    if (completed > totalTiles) {
        result.state = SnapshotLoadState::DiscardedCorrupt;
        result.detail = "completed tile count exceeds total tile count";
        return result;
    }

    std::set<int> seen;
    for (uint32_t i = 0; i < completed; ++i) {
        TileResult tile;
        uint32_t index = 0;
        uint32_t x = 0;
        uint32_t y = 0;
        uint32_t w = 0;
        uint32_t h = 0;
        uint64_t visible = 0;
        uint64_t transmittanceSamples = 0;
        double transmittanceSum = 0.0;
        uint32_t lightCount = 0;
        if (!readU32(bytes, offset, index) || !readU32(bytes, offset, x) ||
            !readU32(bytes, offset, y) || !readU32(bytes, offset, w) ||
            !readU32(bytes, offset, h) || !readU64(bytes, offset, visible) ||
            !readU64(bytes, offset, transmittanceSamples) ||
            !readDouble(bytes, offset, transmittanceSum) ||
            !readU32(bytes, offset, lightCount) || lightCount > 64U) {
            result.state = SnapshotLoadState::DiscardedCorrupt;
            result.detail = "snapshot tile header is truncated";
            return result;
        }
        if (index > static_cast<uint32_t>(std::numeric_limits<int>::max()) ||
            !seen.insert(static_cast<int>(index)).second) {
            result.state = SnapshotLoadState::DiscardedCorrupt;
            result.detail = "snapshot contains a duplicate or invalid tile index";
            return result;
        }
        tile.rect.index = static_cast<int>(index);
        tile.rect.x = static_cast<int>(x);
        tile.rect.y = static_cast<int>(y);
        tile.rect.width = static_cast<int>(w);
        tile.rect.height = static_cast<int>(h);
        tile.stats.visibleFragments = visible;
        tile.stats.transmittanceSamples = transmittanceSamples;
        tile.stats.transmittanceSum = transmittanceSum;
        for (uint32_t lightIndex = 0; lightIndex < lightCount; ++lightIndex) {
            uint32_t lightId = 0;
            uint64_t count = 0;
            if (!readU32(bytes, offset, lightId) || !readU64(bytes, offset, count)) {
                result.state = SnapshotLoadState::DiscardedCorrupt;
                result.detail = "snapshot shadow statistics are truncated";
                return result;
            }
            tile.stats.shadowSamplePoints[lightId] = count;
        }
        uint32_t finalLength = 0;
        uint32_t smokeLength = 0;
        uint32_t ownerLength = 0;
        std::array<uint8_t, 32> finalDigest{};
        std::array<uint8_t, 32> smokeDigest{};
        std::array<uint8_t, 32> ownerDigest{};
        if (!readU32(bytes, offset, finalLength) ||
            !readU32(bytes, offset, smokeLength) ||
            !readU32(bytes, offset, ownerLength) ||
            !readDigest(bytes, offset, finalDigest) ||
            !readDigest(bytes, offset, smokeDigest) ||
            !readDigest(bytes, offset, ownerDigest)) {
            result.state = SnapshotLoadState::DiscardedCorrupt;
            result.detail = "snapshot tile payload header is truncated";
            return result;
        }
        const size_t pixels =
            static_cast<size_t>(tile.rect.width) * static_cast<size_t>(tile.rect.height);
        if (finalLength != pixels * 4U || smokeLength != pixels * 4U ||
            ownerLength != pixels * 4U ||
            offset + finalLength + smokeLength + ownerLength > bodySize) {
            result.state = SnapshotLoadState::DiscardedCorrupt;
            result.detail = "snapshot tile payload length is invalid";
            return result;
        }
        tile.finalRgba.assign(bytes.begin() + static_cast<std::ptrdiff_t>(offset),
                              bytes.begin() +
                                  static_cast<std::ptrdiff_t>(offset + finalLength));
        offset += finalLength;
        tile.smokeRgba.assign(bytes.begin() + static_cast<std::ptrdiff_t>(offset),
                              bytes.begin() +
                                  static_cast<std::ptrdiff_t>(offset + smokeLength));
        offset += smokeLength;
        tile.ownership.resize(pixels);
        for (size_t pixel = 0; pixel < pixels; ++pixel) {
            uint32_t owner = 0;
            if (!readU32(bytes, offset, owner)) {
                result.state = SnapshotLoadState::DiscardedCorrupt;
                result.detail = "snapshot ownership payload is truncated";
                return result;
            }
            tile.ownership[pixel] = owner;
        }
        if (!validTilePayload(tile, scene.image.width, scene.image.height,
                              scene.image.tileWidth, scene.image.tileHeight,
                              renderer.tileColumns(), renderer.tileCount())) {
            result.state = SnapshotLoadState::DiscardedCorrupt;
            result.detail = "snapshot tile geometry or statistics are invalid";
            return result;
        }
        if (std::memcmp(finalDigest.data(),
                        Sha256::hash(tile.finalRgba).data(), kDigestSize) != 0 ||
            std::memcmp(smokeDigest.data(),
                        Sha256::hash(tile.smokeRgba).data(), kDigestSize) != 0) {
            result.state = SnapshotLoadState::DiscardedCorrupt;
            result.detail = "snapshot color tile checksum mismatch";
            return result;
        }
        std::vector<uint8_t> ownerBytes;
        ownerBytes.reserve(tile.ownership.size() * 4U);
        for (const uint32_t owner : tile.ownership) {
            appendU32(ownerBytes, owner);
        }
        if (std::memcmp(ownerDigest.data(), Sha256::hash(ownerBytes).data(),
                        kDigestSize) != 0) {
            result.state = SnapshotLoadState::DiscardedCorrupt;
            result.detail = "snapshot ownership tile checksum mismatch";
            return result;
        }
        result.tiles.emplace(tile.rect.index, std::move(tile));
    }
    if (offset != bodySize) {
        result.state = SnapshotLoadState::DiscardedCorrupt;
        result.detail = "snapshot has trailing or missing records";
        return result;
    }
    result.state = SnapshotLoadState::Loaded;
    return result;
}

bool SnapshotStore::commit(const Scene& scene, int frame, const Renderer& renderer,
                           const std::map<int, TileResult>& tiles,
                           std::string& error) const {
    (void)frame;
    std::vector<uint8_t> body;
    body.insert(body.end(), kMagic, kMagic + 8);
    appendU32(body, kSnapshotVersion);
    appendU32(body, static_cast<uint32_t>(scene.image.width));
    appendU32(body, static_cast<uint32_t>(scene.image.height));
    appendU32(body, static_cast<uint32_t>(scene.image.tileWidth));
    appendU32(body, static_cast<uint32_t>(scene.image.tileHeight));
    appendU32(body, static_cast<uint32_t>(renderer.tileColumns()));
    appendU32(body, static_cast<uint32_t>(renderer.tileRows()));
    appendU32(body, static_cast<uint32_t>(renderer.tileCount()));
    appendU32(body, static_cast<uint32_t>(tiles.size()));
    appendDigest(body, scene.digest);
    appendDigest(body, digestForText(renderer.renderDigest()));

    for (const auto& entry : tiles) {
        const TileResult& tile = entry.second;
        appendU32(body, static_cast<uint32_t>(tile.rect.index));
        appendU32(body, static_cast<uint32_t>(tile.rect.x));
        appendU32(body, static_cast<uint32_t>(tile.rect.y));
        appendU32(body, static_cast<uint32_t>(tile.rect.width));
        appendU32(body, static_cast<uint32_t>(tile.rect.height));
        appendU64(body, tile.stats.visibleFragments);
        appendU64(body, tile.stats.transmittanceSamples);
        appendDouble(body, tile.stats.transmittanceSum);
        appendU32(body, static_cast<uint32_t>(tile.stats.shadowSamplePoints.size()));
        for (const auto& shadow : tile.stats.shadowSamplePoints) {
            appendU32(body, shadow.first);
            appendU64(body, shadow.second);
        }
        appendU32(body, static_cast<uint32_t>(tile.finalRgba.size()));
        appendU32(body, static_cast<uint32_t>(tile.smokeRgba.size()));
        appendU32(body, static_cast<uint32_t>(tile.ownership.size() * 4U));
        appendDigest(body, Sha256::hash(tile.finalRgba));
        appendDigest(body, Sha256::hash(tile.smokeRgba));
        std::vector<uint8_t> ownerBytes;
        ownerBytes.reserve(tile.ownership.size() * 4U);
        for (const uint32_t owner : tile.ownership) {
            appendU32(ownerBytes, owner);
        }
        appendDigest(body, Sha256::hash(ownerBytes));
        body.insert(body.end(), tile.finalRgba.begin(), tile.finalRgba.end());
        body.insert(body.end(), tile.smokeRgba.begin(), tile.smokeRgba.end());
        body.insert(body.end(), ownerBytes.begin(), ownerBytes.end());
    }
    const std::array<uint8_t, 32> bodyDigest = Sha256::hash(body);
    appendDigest(body, bodyDigest);
    return writeFileBytesAtomic(path_, body, error);
}

}  // namespace sr
