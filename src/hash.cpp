#include "chunkpipe/hash.hpp"

#include <cstring>
#include <iomanip>
#include <sstream>

namespace chunkpipe {

namespace {

constexpr std::uint64_t kFnvOffset = 1469598103934665603ull;
constexpr std::uint64_t kFnvPrime = 1099511628211ull;

void hashAppend(std::uint64_t& hash, const void* data, std::size_t size) {
    const auto* bytes = static_cast<const std::uint8_t*>(data);
    for (std::size_t index = 0; index < size; ++index) {
        hash ^= bytes[index];
        hash *= kFnvPrime;
    }
}

template <typename T>
void hashAppendScalar(std::uint64_t& hash, const T& value) {
    hashAppend(hash, &value, sizeof(T));
}

} // namespace

std::uint64_t hashBytes(const void* data, std::size_t size) {
    std::uint64_t hash = kFnvOffset;
    hashAppend(hash, data, size);
    return hash;
}

std::uint64_t hashBytes(const std::vector<std::uint8_t>& bytes) {
    return hashBytes(bytes.data(), bytes.size());
}

std::uint64_t hashString(std::string_view text) {
    return hashBytes(text.data(), text.size());
}

std::uint64_t hashCombine(std::uint64_t seed, std::uint64_t value) {
    seed ^= value + 0x9e3779b97f4a7c15ull + (seed << 6) + (seed >> 2);
    return seed;
}

std::string hashHex(std::uint64_t value) {
    std::ostringstream stream;
    stream << std::hex << std::setfill('0') << std::setw(16) << value;
    return stream.str();
}

std::uint64_t chunkContentHash(const Chunk& chunk) {
    std::uint64_t hash = hashString("chunk-content-v1");
    hashAppendScalar(hash, chunk.id);
    hashAppendScalar(hash, chunk.bounds.min);
    hashAppendScalar(hash, chunk.bounds.max);
    hashAppendScalar(hash, static_cast<std::uint64_t>(chunk.vertices.size()));
    if (!chunk.vertices.empty()) {
        hashAppend(hash, chunk.vertices.data(), chunk.vertices.size() * sizeof(Vec3));
    }
    hashAppendScalar(hash, static_cast<std::uint64_t>(chunk.indices.size()));
    if (!chunk.indices.empty()) {
        hashAppend(hash, chunk.indices.data(), chunk.indices.size() * sizeof(std::uint32_t));
    }
    hashAppendScalar(hash, static_cast<std::uint8_t>(chunk.grid.has_value()));
    if (chunk.grid) {
        hashAppendScalar(hash, chunk.grid->x);
        hashAppendScalar(hash, chunk.grid->y);
        hashAppendScalar(hash, chunk.grid->z);
        hashAppendScalar(hash, chunk.grid->cellSize);
    }
    hashAppendScalar(hash, static_cast<std::uint8_t>(chunk.occluder.has_value()));
    if (chunk.occluder) {
        hashAppendScalar(hash, static_cast<std::uint8_t>(*chunk.occluder ? 1 : 0));
    }
    hashAppendScalar(hash, static_cast<std::uint8_t>(chunk.compression.has_value()));
    if (chunk.compression) {
        hashAppendScalar(hash, chunk.compression->codec);
        hashAppendScalar(hash, chunk.compression->rawBytes);
        hashAppendScalar(hash, chunk.compression->storedBytes);
        hashAppendScalar(hash, chunk.compression->flags);
    }
    hashAppendScalar(hash, chunk.priority);
    hashAppendScalar(hash, static_cast<std::uint32_t>(chunk.prioritySource));
    hashAppendScalar(hash, static_cast<std::uint8_t>(chunk.hierarchy.has_value()));
    if (chunk.hierarchy) {
        hashAppendScalar(hash, chunk.hierarchy->parent);
        hashAppendScalar(hash,
                         static_cast<std::uint64_t>(chunk.hierarchy->children.size()));
        for (const std::uint32_t child : chunk.hierarchy->children) {
            hashAppendScalar(hash, child);
        }
    }
    return hash;
}

std::uint64_t sceneContentHash(const SceneArchive& scene) {
    std::uint64_t hash = hashString("scene-content-v1");
    hashAppendScalar(hash, scene.version);
    hashAppendScalar(hash, scene.sceneId);
    hashAppendScalar(hash, scene.boundaryTolerance);
    hashAppendScalar(hash, static_cast<std::uint64_t>(scene.chunks.size()));
    for (const Chunk& chunk : scene.chunks) {
        hashAppendScalar(hash, chunkContentHash(chunk));
    }
    return hash;
}

} // namespace chunkpipe

