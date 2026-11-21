#include "chunkpipe/archive.hpp"

#include <algorithm>
#include <array>
#include <cmath>
#include <cstring>
#include <fstream>
#include <functional>
#include <limits>
#include <set>
#include <sstream>
#include <type_traits>
#include <unordered_map>
#include <unordered_set>

#include "chunkpipe/hash.hpp"

namespace chunkpipe {

namespace {

constexpr std::array<std::uint8_t, 8> kMagic = {'C', 'H', 'N', 'K', 'A', 'R', 'C', '1'};
constexpr std::uint16_t kHeaderSize = 32;
constexpr std::uint32_t kMaxChunks = 1'000'000;
constexpr std::uint32_t kMaxVertices = 100'000'000;
constexpr std::uint32_t kMaxIndices = 300'000'000;

struct ParseFailure {
    BusinessError error;
};

[[noreturn]] void fail(std::uint32_t chunkId,
                       const std::string& field,
                       const std::string& detail) {
    BusinessError error;
    error.code = Code::ChunkInvalid;
    error.message = "archive parse or validation failed";
    ErrorLocation location;
    location.chunkId = chunkId;
    location.field = field;
    location.detail = detail;
    error.locations.push_back(std::move(location));
    throw ParseFailure{std::move(error)};
}

class Reader {
public:
    explicit Reader(const std::vector<std::uint8_t>& bytes) : bytes_(bytes) {}

    void setChunk(std::uint32_t chunkId) { chunkId_ = chunkId; }
    std::size_t position() const { return position_; }
    std::size_t remaining() const { return bytes_.size() - position_; }

    template <typename T>
    T read(const std::string& field) {
        static_assert(std::is_trivially_copyable<T>::value, "scalar required");
        if (remaining() < sizeof(T)) {
            fail(chunkId_, field, "field is missing or truncated");
        }
        T value{};
        std::memcpy(&value, bytes_.data() + position_, sizeof(T));
        position_ += sizeof(T);
        return value;
    }

    Vec3 readVec3(const std::string& field) {
        Vec3 value;
        value.x = read<float>(field + ".x");
        value.y = read<float>(field + ".y");
        value.z = read<float>(field + ".z");
        return value;
    }

    std::vector<std::uint8_t> readBytes(std::uint64_t count, const std::string& field) {
        if (count > remaining()) {
            fail(chunkId_, field, "declared byte count exceeds available data");
        }
        std::vector<std::uint8_t> result(static_cast<std::size_t>(count));
        if (count > 0) {
            std::memcpy(result.data(), bytes_.data() + position_,
                        static_cast<std::size_t>(count));
            position_ += static_cast<std::size_t>(count);
        }
        return result;
    }

private:
    const std::vector<std::uint8_t>& bytes_;
    std::size_t position_ = 0;
    std::uint32_t chunkId_ = 0;
};

class Writer {
public:
    template <typename T>
    void write(const T& value) {
        static_assert(std::is_trivially_copyable<T>::value, "scalar required");
        const auto* bytes = reinterpret_cast<const std::uint8_t*>(&value);
        output_.insert(output_.end(), bytes, bytes + sizeof(T));
    }

    void writeVec3(const Vec3& value) {
        write(value.x);
        write(value.y);
        write(value.z);
    }

    void writeBytes(const std::vector<std::uint8_t>& value) {
        output_.insert(output_.end(), value.begin(), value.end());
    }

    std::vector<std::uint8_t> take() { return std::move(output_); }

private:
    std::vector<std::uint8_t> output_;
};

std::uint64_t expectedMeshBytes(const Chunk& chunk) {
    return static_cast<std::uint64_t>(chunk.vertices.size()) * sizeof(Vec3) +
           static_cast<std::uint64_t>(chunk.indices.size()) * sizeof(std::uint32_t);
}

std::vector<std::uint8_t> rawMeshBytes(const Chunk& chunk) {
    std::vector<std::uint8_t> bytes;
    bytes.resize(static_cast<std::size_t>(expectedMeshBytes(chunk)));
    std::size_t offset = 0;
    if (!chunk.vertices.empty()) {
        const std::size_t size = chunk.vertices.size() * sizeof(Vec3);
        std::memcpy(bytes.data(), chunk.vertices.data(), size);
        offset += size;
    }
    if (!chunk.indices.empty()) {
        std::memcpy(bytes.data() + offset, chunk.indices.data(),
                    chunk.indices.size() * sizeof(std::uint32_t));
    }
    return bytes;
}

std::vector<std::uint8_t> rleEncode(const std::vector<std::uint8_t>& raw) {
    std::vector<std::uint8_t> encoded;
    std::size_t index = 0;
    while (index < raw.size()) {
        const std::uint8_t value = raw[index];
        std::uint32_t count = 1;
        while (index + count < raw.size() && raw[index + count] == value &&
               count < std::numeric_limits<std::uint32_t>::max()) {
            ++count;
        }
        encoded.push_back(value);
        const auto* countBytes = reinterpret_cast<const std::uint8_t*>(&count);
        encoded.insert(encoded.end(), countBytes, countBytes + sizeof(count));
        index += count;
    }
    return encoded;
}

std::vector<std::uint8_t> rleDecode(const std::vector<std::uint8_t>& encoded,
                                    std::uint64_t expectedBytes,
                                    std::uint32_t chunkId) {
    std::vector<std::uint8_t> decoded;
    if (expectedBytes > std::numeric_limits<std::size_t>::max()) {
        fail(chunkId, "compression.raw_bytes", "declared raw size is too large");
    }
    decoded.reserve(static_cast<std::size_t>(expectedBytes));
    std::size_t index = 0;
    while (index < encoded.size()) {
        if (encoded.size() - index < 5) {
            fail(chunkId, "compression.payload", "truncated RLE run header");
        }
        const std::uint8_t value = encoded[index++];
        std::uint32_t count = 0;
        std::memcpy(&count, encoded.data() + index, sizeof(count));
        index += sizeof(count);
        if (count == 0 || decoded.size() + count > expectedBytes) {
            fail(chunkId, "compression.payload", "RLE run exceeds declared raw size");
        }
        decoded.insert(decoded.end(), count, value);
    }
    if (decoded.size() != expectedBytes) {
        fail(chunkId, "compression.payload", "decoded size does not match descriptor");
    }
    return decoded;
}

void parseMeshBytes(const std::vector<std::uint8_t>& mesh,
                    std::uint32_t vertexCount,
                    std::uint32_t indexCount,
                    std::uint32_t chunkId,
                    Chunk& chunk) {
    const std::uint64_t vertexBytes =
        static_cast<std::uint64_t>(vertexCount) * sizeof(Vec3);
    const std::uint64_t indexBytes =
        static_cast<std::uint64_t>(indexCount) * sizeof(std::uint32_t);
    if (mesh.size() != vertexBytes + indexBytes) {
        fail(chunkId, "mesh.payload", "decoded mesh size does not match counts");
    }
    chunk.vertices.resize(vertexCount);
    chunk.indices.resize(indexCount);
    if (vertexBytes > 0) {
        std::memcpy(chunk.vertices.data(), mesh.data(), static_cast<std::size_t>(vertexBytes));
    }
    if (indexBytes > 0) {
        std::memcpy(chunk.indices.data(), mesh.data() + vertexBytes,
                    static_cast<std::size_t>(indexBytes));
    }
}

void validateChunk(const SceneArchive& scene,
                   const Chunk& chunk,
                   const std::unordered_set<std::uint32_t>& ids) {
    const std::uint32_t chunkId = chunk.id;
    if (!chunk.bounds.finiteAndOrdered()) {
        fail(chunkId, "bounds", "AABB is not finite and ordered");
    }
    if (chunk.indices.size() % 3 != 0) {
        fail(chunkId, "indices", "index count is not a multiple of three");
    }
    for (const Vec3& vertex : chunk.vertices) {
        if (!std::isfinite(vertex.x) || !std::isfinite(vertex.y) ||
            !std::isfinite(vertex.z)) {
            fail(chunkId, "vertices", "vertex contains a non-finite component");
        }
    }
    const float geometryTolerance =
        std::max(1e-3f, std::max(chunk.bounds.extent().x,
                                std::max(chunk.bounds.extent().y,
                                         chunk.bounds.extent().z)) *
                            1e-5f);
    for (std::size_t index = 0; index < chunk.vertices.size(); ++index) {
        if (!chunk.bounds.contains(chunk.vertices[index], geometryTolerance)) {
            std::ostringstream detail;
            detail << "vertex " << index << " lies outside the chunk AABB";
            fail(chunkId, "bounds", detail.str());
        }
    }
    for (std::size_t index = 0; index < chunk.indices.size(); ++index) {
        if (chunk.indices[index] >= chunk.vertices.size()) {
            std::ostringstream detail;
            detail << "index " << index << " references vertex " << chunk.indices[index]
                   << " with vertex count " << chunk.vertices.size();
            fail(chunkId, "indices", detail.str());
        }
    }
    if (chunk.grid) {
        if (chunk.grid->x == 0 || chunk.grid->y == 0 || chunk.grid->z == 0 ||
            !std::isfinite(chunk.grid->cellSize) || chunk.grid->cellSize <= 0.0f) {
            fail(chunkId, "grid", "grid dimensions and cell size must be positive");
        }
    }
    if (chunk.occluder && *chunk.occluder) {
        const Vec3 extent = chunk.bounds.extent();
        if (extent.x <= 0.0f || extent.y <= 0.0f || extent.z <= 0.0f ||
            chunk.vertices.empty() || chunk.indices.empty()) {
            fail(chunkId, "occluder",
                 "occluder marker requires a non-empty AABB and mesh");
        }
    }
    if (chunk.compression) {
        if (chunk.compression->codec > 1) {
            fail(chunkId, "compression.codec", "unknown compression codec");
        }
        if (chunk.compression->rawBytes != expectedMeshBytes(chunk)) {
            fail(chunkId, "compression.raw_bytes",
                 "raw byte count does not match vertex and index counts");
        }
        if (chunk.compression->codec == 0 &&
            chunk.compression->storedBytes != chunk.compression->rawBytes) {
            fail(chunkId, "compression.stored_bytes",
                 "raw codec stored size must equal raw size");
        }
    }
    if (chunk.prioritySource == PrioritySource::AuthoredV3 ||
        chunk.prioritySource == PrioritySource::DerivedFromBounds) {
        // Valid enumerator.
    } else {
        fail(chunkId, "priority.source", "unknown priority provenance");
    }
    if (chunk.hierarchy) {
        if (chunk.hierarchy->parent != kNoChunk &&
            ids.find(chunk.hierarchy->parent) == ids.end()) {
            fail(chunkId, "hierarchy.parent", "parent reference is dangling");
        }
        if (chunk.hierarchy->parent == chunkId) {
            fail(chunkId, "hierarchy.parent", "chunk cannot reference itself as parent");
        }
        std::unordered_set<std::uint32_t> children;
        for (const std::uint32_t child : chunk.hierarchy->children) {
            if (ids.find(child) == ids.end()) {
                fail(chunkId, "hierarchy.children", "child reference is dangling");
            }
            if (child == chunkId) {
                fail(chunkId, "hierarchy.children", "chunk cannot reference itself as child");
            }
            if (!children.insert(child).second) {
                fail(chunkId, "hierarchy.children", "duplicate child reference");
            }
        }
    }
    (void)scene;
}

void validateHierarchy(const SceneArchive& scene) {
    std::unordered_map<std::uint32_t, const Chunk*> chunks;
    for (const Chunk& chunk : scene.chunks) {
        chunks.emplace(chunk.id, &chunk);
    }
    for (const Chunk& chunk : scene.chunks) {
        if (!chunk.hierarchy) {
            continue;
        }
        for (const std::uint32_t child : chunk.hierarchy->children) {
            const auto childIt = chunks.find(child);
            if (childIt == chunks.end() || !childIt->second->hierarchy ||
                childIt->second->hierarchy->parent != chunk.id) {
                fail(chunk.id, "hierarchy.children",
                     "child hierarchy does not point back to this parent");
            }
        }
        if (chunk.hierarchy->parent == kNoChunk) {
            continue;
        }
        const auto parentIt = chunks.find(chunk.hierarchy->parent);
        if (parentIt == chunks.end() || !parentIt->second->hierarchy) {
            fail(chunk.id, "hierarchy.parent",
                 "parent chunk has no hierarchy table entry");
        }
        const auto& children = parentIt->second->hierarchy->children;
        if (std::find(children.begin(), children.end(), chunk.id) == children.end()) {
            fail(chunk.id, "hierarchy.parent",
                 "parent hierarchy table does not list this chunk as a child");
        }
    }

    std::unordered_map<std::uint32_t, std::uint8_t> state;
    std::function<void(std::uint32_t)> visit = [&](std::uint32_t chunkId) {
        const auto chunkIt = chunks.find(chunkId);
        if (chunkIt == chunks.end() || !chunkIt->second->hierarchy) {
            return;
        }
        std::uint8_t& current = state[chunkId];
        if (current == 1) {
            fail(chunkId, "hierarchy", "hierarchy contains a cycle");
        }
        if (current == 2) {
            return;
        }
        current = 1;
        for (const std::uint32_t child : chunkIt->second->hierarchy->children) {
            visit(child);
        }
        current = 2;
    };
    for (const Chunk& chunk : scene.chunks) {
        visit(chunk.id);
    }
}

void parseChunk(Reader& reader,
                std::uint16_t version,
                std::uint32_t ordinal,
                Chunk& chunk) {
    reader.setChunk(ordinal);
    chunk.id = reader.read<std::uint32_t>("id");
    reader.setChunk(chunk.id);
    chunk.bounds.min = reader.readVec3("bounds.min");
    chunk.bounds.max = reader.readVec3("bounds.max");
    const std::uint32_t vertexCount = reader.read<std::uint32_t>("vertex_count");
    const std::uint32_t indexCount = reader.read<std::uint32_t>("index_count");
    if (vertexCount > kMaxVertices) {
        fail(chunk.id, "vertex_count", "vertex count exceeds safety limit");
    }
    if (indexCount > kMaxIndices) {
        fail(chunk.id, "index_count", "index count exceeds safety limit");
    }

    std::vector<std::uint8_t> mesh;
    if (version == 3) {
        const std::uint16_t fieldFlags = reader.read<std::uint16_t>("field_flags");
        constexpr std::uint16_t knownFlags = 0x001f;
        if ((fieldFlags & ~knownFlags) != 0) {
            fail(chunk.id, "field_flags", "unknown field flag bit");
        }
        const bool hasGrid = (fieldFlags & 0x0001) != 0;
        const bool hasOccluder = (fieldFlags & 0x0002) != 0;
        const bool hasCompression = (fieldFlags & 0x0004) != 0;
        const bool hasHierarchy = (fieldFlags & 0x0008) != 0;
        const bool hasPriority = (fieldFlags & 0x0010) != 0;
        if (!hasPriority) {
            fail(chunk.id, "priority", "v3 priority field is explicitly missing");
        }
        if (hasCompression) {
            CompressionDesc compression;
            compression.codec = reader.read<std::uint32_t>("compression.codec");
            compression.flags = reader.read<std::uint32_t>("compression.flags");
            compression.rawBytes = reader.read<std::uint64_t>("compression.raw_bytes");
            compression.storedBytes = reader.read<std::uint64_t>("compression.stored_bytes");
            const std::uint64_t payloadBytes =
                reader.read<std::uint64_t>("compression.payload_bytes");
            if (payloadBytes != compression.storedBytes) {
                fail(chunk.id, "compression.payload_bytes",
                     "payload length does not match stored byte count");
            }
            const std::vector<std::uint8_t> payload =
                reader.readBytes(payloadBytes, "compression.payload");
            if (compression.codec == 0) {
                if (compression.rawBytes != compression.storedBytes) {
                    fail(chunk.id, "compression.stored_bytes",
                         "raw codec stored size must equal raw size");
                }
                mesh = payload;
            } else if (compression.codec == 1) {
                mesh = rleDecode(payload, compression.rawBytes, chunk.id);
            } else {
                fail(chunk.id, "compression.codec", "unknown compression codec");
            }
            chunk.compression = compression;
        } else {
            const std::uint64_t byteCount =
                static_cast<std::uint64_t>(vertexCount) * sizeof(Vec3) +
                static_cast<std::uint64_t>(indexCount) * sizeof(std::uint32_t);
            mesh = reader.readBytes(byteCount, "mesh.payload");
        }
        if (hasGrid) {
            GridDivision grid;
            grid.x = reader.read<std::uint32_t>("grid.x");
            grid.y = reader.read<std::uint32_t>("grid.y");
            grid.z = reader.read<std::uint32_t>("grid.z");
            grid.cellSize = reader.read<float>("grid.cell_size");
            chunk.grid = grid;
        }
        if (hasOccluder) {
            const std::uint8_t marker = reader.read<std::uint8_t>("occluder");
            if (marker > 1) {
                fail(chunk.id, "occluder", "occluder marker must be zero or one");
            }
            chunk.occluder = marker != 0;
        }
        chunk.priority = reader.read<std::uint32_t>("priority");
        const std::uint32_t prioritySource =
            reader.read<std::uint32_t>("priority.source");
        if (prioritySource == static_cast<std::uint32_t>(PrioritySource::AuthoredV3)) {
            chunk.prioritySource = PrioritySource::AuthoredV3;
        } else if (prioritySource ==
                   static_cast<std::uint32_t>(PrioritySource::DerivedFromBounds)) {
            chunk.prioritySource = PrioritySource::DerivedFromBounds;
        } else {
            fail(chunk.id, "priority.source", "unknown priority provenance");
        }
        if (hasHierarchy) {
            HierarchyTable hierarchy;
            hierarchy.parent = reader.read<std::uint32_t>("hierarchy.parent");
            const std::uint32_t childCount =
                reader.read<std::uint32_t>("hierarchy.child_count");
            if (childCount > kMaxChunks) {
                fail(chunk.id, "hierarchy.child_count", "child count exceeds safety limit");
            }
            hierarchy.children.reserve(childCount);
            for (std::uint32_t index = 0; index < childCount; ++index) {
                hierarchy.children.push_back(
                    reader.read<std::uint32_t>("hierarchy.children"));
            }
            chunk.hierarchy = std::move(hierarchy);
        }
    } else {
        const std::uint64_t byteCount =
            static_cast<std::uint64_t>(vertexCount) * sizeof(Vec3) +
            static_cast<std::uint64_t>(indexCount) * sizeof(std::uint32_t);
        mesh = reader.readBytes(byteCount, "mesh.payload");
        if (version == 2) {
            const std::uint16_t fieldFlags = reader.read<std::uint16_t>("field_flags");
            constexpr std::uint16_t knownFlags = 0x0003;
            if ((fieldFlags & ~knownFlags) != 0) {
                fail(chunk.id, "field_flags", "unknown field flag bit");
            }
            if ((fieldFlags & 0x0001) != 0) {
                GridDivision grid;
                grid.x = reader.read<std::uint32_t>("grid.x");
                grid.y = reader.read<std::uint32_t>("grid.y");
                grid.z = reader.read<std::uint32_t>("grid.z");
                grid.cellSize = reader.read<float>("grid.cell_size");
                chunk.grid = grid;
            }
            if ((fieldFlags & 0x0002) != 0) {
                const std::uint8_t marker = reader.read<std::uint8_t>("occluder");
                if (marker > 1) {
                    fail(chunk.id, "occluder", "occluder marker must be zero or one");
                }
                chunk.occluder = marker != 0;
            }
        }
    }
    parseMeshBytes(mesh, vertexCount, indexCount, chunk.id, chunk);
    if (version < 3) {
        chunk.priority = derivePriority(chunk.bounds);
        chunk.prioritySource = PrioritySource::DerivedFromBounds;
    }
}

} // namespace

Result<SceneArchive> readArchiveFromFile(const std::filesystem::path& path,
                                         const ArchiveReadOptions& options) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        return Result<SceneArchive>::failure(Code::IoError,
                                             "cannot open archive: " + path.string());
    }
    input.seekg(0, std::ios::end);
    const std::streamoff size = input.tellg();
    if (size < 0) {
        return Result<SceneArchive>::failure(Code::IoError,
                                             "cannot determine archive size: " +
                                                 path.string());
    }
    input.seekg(0, std::ios::beg);
    std::vector<std::uint8_t> bytes(static_cast<std::size_t>(size));
    if (size > 0) {
        input.read(reinterpret_cast<char*>(bytes.data()), size);
    }
    if (!input && !input.eof()) {
        return Result<SceneArchive>::failure(Code::IoError,
                                             "cannot read archive: " + path.string());
    }
    Result<SceneArchive> result = readArchiveFromBytes(bytes, options);
    if (result.ok()) {
        result.value().sourcePath = path.string();
    }
    return result;
}

Result<SceneArchive> readArchiveFromBytes(const std::vector<std::uint8_t>& bytes,
                                          const ArchiveReadOptions& options) {
    try {
        Reader reader(bytes);
        reader.setChunk(0);
        const std::vector<std::uint8_t> magic =
            reader.readBytes(8, "magic");
        for (std::size_t index = 0; index < magic.size(); ++index) {
            if (magic[index] != kMagic[index]) {
                fail(0, "magic", "archive magic does not match CHUNKARC1");
            }
        }
        const std::uint16_t version = reader.read<std::uint16_t>("version");
        if (version < 1 || version > 3) {
            fail(0, "version", "unknown archive version");
        }
        const std::uint16_t headerSize = reader.read<std::uint16_t>("header_size");
        if (headerSize != kHeaderSize) {
            fail(0, "header_size", "unsupported archive header size");
        }
        SceneArchive scene;
        scene.version = version;
        scene.sceneId = reader.read<std::uint32_t>("scene_id");
        scene.boundaryTolerance = reader.read<float>("boundary_tolerance");
        if (!std::isfinite(scene.boundaryTolerance) ||
            scene.boundaryTolerance < 0.0f || scene.boundaryTolerance > 1.0f) {
            fail(0, "boundary_tolerance",
                 "boundary tolerance must be finite and in [0, 1]");
        }
        const std::uint32_t chunkCount = reader.read<std::uint32_t>("chunk_count");
        if (chunkCount > kMaxChunks) {
            fail(0, "chunk_count", "chunk count exceeds safety limit");
        }
        const std::uint32_t reserved = reader.read<std::uint32_t>("reserved");
        if (reserved != 0) {
            fail(0, "reserved", "reserved header field must be zero");
        }
        scene.chunks.resize(chunkCount);
        for (std::uint32_t index = 0; index < chunkCount; ++index) {
            parseChunk(reader, version, index, scene.chunks[index]);
        }
        if (options.rejectTrailingBytes && reader.remaining() != 0) {
            fail(scene.chunks.empty() ? 0 : scene.chunks.back().id,
                 "trailing_bytes", "archive contains trailing data");
        }
        Result<void> validation = validateScene(scene);
        if (!validation.ok()) {
            return Result<SceneArchive>::failure(validation.error());
        }
        return Result<SceneArchive>::success(std::move(scene));
    } catch (const ParseFailure& failure) {
        return Result<SceneArchive>::failure(failure.error);
    } catch (const std::exception& exception) {
        return Result<SceneArchive>::failure(
            Code::Internal, std::string("archive parser exception: ") + exception.what());
    }
}

Result<void> validateScene(const SceneArchive& scene) {
    try {
        if (scene.version < 1 || scene.version > 3) {
            fail(0, "version", "unknown archive version");
        }
        if (!std::isfinite(scene.boundaryTolerance) || scene.boundaryTolerance < 0.0f ||
            scene.boundaryTolerance > 1.0f) {
            fail(0, "boundary_tolerance",
                 "boundary tolerance must be finite and in [0, 1]");
        }
        std::unordered_set<std::uint32_t> ids;
        for (const Chunk& chunk : scene.chunks) {
            if (!ids.insert(chunk.id).second) {
                fail(chunk.id, "id", "duplicate chunk id");
            }
        }
        for (const Chunk& chunk : scene.chunks) {
            validateChunk(scene, chunk, ids);
        }
        validateHierarchy(scene);
        return Result<void>::success();
    } catch (const ParseFailure& failure) {
        return Result<void>::failure(failure.error);
    }
}

std::uint32_t derivePriority(const Aabb& bounds) {
    const float distance = length(bounds.center());
    const float quantized = std::floor(distance * 16.0f);
    if (!std::isfinite(quantized)) {
        return 0;
    }
    constexpr float maximum = 999999.0f;
    const float clamped = std::max(0.0f, std::min(maximum, 999999.0f - quantized));
    return static_cast<std::uint32_t>(clamped);
}

Result<void> checkVersionLoss(const SceneArchive& scene, std::uint16_t targetVersion) {
    if (targetVersion < 1 || targetVersion > 3) {
        BusinessError error;
        error.code = Code::ChunkInvalid;
        error.message = "unknown target archive version";
        ErrorLocation location;
        location.chunkId = 0;
        location.field = "version";
        error.locations.push_back(std::move(location));
        return Result<void>::failure(std::move(error));
    }
    if (targetVersion == 3) {
        return Result<void>::success();
    }

    BusinessError error;
    error.code = Code::VersionLoss;
    error.message = "target version cannot represent source chunk attributes";
    for (const Chunk& chunk : scene.chunks) {
        auto addLoss = [&](const std::string& field) {
            ErrorLocation location;
            location.chunkId = chunk.id;
            location.field = field;
            location.detail = "field cannot be expressed by archive v" +
                              std::to_string(targetVersion);
            error.locations.push_back(std::move(location));
        };
        if (targetVersion == 1) {
            if (chunk.grid) {
                addLoss("grid");
            }
            if (chunk.occluder) {
                addLoss("occluder");
            }
        }
        if (chunk.compression) {
            addLoss("compression");
        }
        if (chunk.prioritySource == PrioritySource::AuthoredV3) {
            addLoss("priority");
        }
        if (chunk.hierarchy) {
            addLoss("hierarchy");
        }
    }
    if (error.locations.empty()) {
        return Result<void>::success();
    }
    return Result<void>::failure(std::move(error));
}

Result<void> writeArchiveToFile(const std::filesystem::path& path,
                                const SceneArchive& scene,
                                std::uint16_t targetVersion) {
    Result<std::vector<std::uint8_t>> serialized = serializeArchive(scene, targetVersion);
    if (!serialized.ok()) {
        return Result<void>::failure(serialized.error());
    }
    const auto parent = path.parent_path();
    if (!parent.empty()) {
        std::error_code errorCode;
        std::filesystem::create_directories(parent, errorCode);
        if (errorCode) {
            return Result<void>::failure(Code::IoError,
                                         "cannot create archive directory: " +
                                             parent.string());
        }
    }
    std::ofstream output(path, std::ios::binary | std::ios::trunc);
    if (!output) {
        return Result<void>::failure(Code::IoError,
                                     "cannot create archive: " + path.string());
    }
    output.write(reinterpret_cast<const char*>(serialized.value().data()),
                 static_cast<std::streamsize>(serialized.value().size()));
    output.flush();
    if (!output) {
        return Result<void>::failure(Code::IoError,
                                     "cannot write archive: " + path.string());
    }
    return Result<void>::success();
}

Result<std::vector<std::uint8_t>> serializeArchive(const SceneArchive& scene,
                                                   std::uint16_t targetVersion) {
    Result<void> loss = checkVersionLoss(scene, targetVersion);
    if (!loss.ok()) {
        return Result<std::vector<std::uint8_t>>::failure(loss.error());
    }
    Result<void> validation = validateScene(scene);
    if (!validation.ok()) {
        return Result<std::vector<std::uint8_t>>::failure(validation.error());
    }

    Writer writer;
    for (const std::uint8_t byte : kMagic) {
        writer.write(byte);
    }
    writer.write(targetVersion);
    writer.write(kHeaderSize);
    writer.write(scene.sceneId);
    writer.write(scene.boundaryTolerance);
    writer.write(static_cast<std::uint32_t>(scene.chunks.size()));
    writer.write<std::uint32_t>(0);

    for (const Chunk& chunk : scene.chunks) {
        writer.write(chunk.id);
        writer.writeVec3(chunk.bounds.min);
        writer.writeVec3(chunk.bounds.max);
        writer.write(static_cast<std::uint32_t>(chunk.vertices.size()));
        writer.write(static_cast<std::uint32_t>(chunk.indices.size()));

        if (targetVersion == 3) {
            std::uint16_t fieldFlags = 0x0010;
            if (chunk.grid) {
                fieldFlags |= 0x0001;
            }
            if (chunk.occluder) {
                fieldFlags |= 0x0002;
            }
            if (chunk.compression) {
                fieldFlags |= 0x0004;
            }
            if (chunk.hierarchy) {
                fieldFlags |= 0x0008;
            }
            writer.write(fieldFlags);
            if (chunk.compression) {
                CompressionDesc compression = *chunk.compression;
                const std::vector<std::uint8_t> raw = rawMeshBytes(chunk);
                std::vector<std::uint8_t> payload;
                if (compression.codec == 0) {
                    payload = raw;
                } else if (compression.codec == 1) {
                    payload = rleEncode(raw);
                } else {
                    BusinessError error;
                    error.code = Code::ChunkInvalid;
                    error.message = "unsupported compression codec during serialization";
                    ErrorLocation location;
                    location.chunkId = chunk.id;
                    location.field = "compression.codec";
                    error.locations.push_back(std::move(location));
                    return Result<std::vector<std::uint8_t>>::failure(std::move(error));
                }
                compression.rawBytes = raw.size();
                compression.storedBytes = payload.size();
                writer.write(compression.codec);
                writer.write(compression.flags);
                writer.write(compression.rawBytes);
                writer.write(compression.storedBytes);
                writer.write(static_cast<std::uint64_t>(payload.size()));
                writer.writeBytes(payload);
            } else {
                writer.writeBytes(rawMeshBytes(chunk));
            }
            if (chunk.grid) {
                writer.write(chunk.grid->x);
                writer.write(chunk.grid->y);
                writer.write(chunk.grid->z);
                writer.write(chunk.grid->cellSize);
            }
            if (chunk.occluder) {
                writer.write(static_cast<std::uint8_t>(*chunk.occluder ? 1 : 0));
            }
            writer.write(chunk.priority);
            writer.write(static_cast<std::uint32_t>(chunk.prioritySource));
            if (chunk.hierarchy) {
                writer.write(chunk.hierarchy->parent);
                writer.write(static_cast<std::uint32_t>(chunk.hierarchy->children.size()));
                for (const std::uint32_t child : chunk.hierarchy->children) {
                    writer.write(child);
                }
            }
        } else {
            writer.writeBytes(rawMeshBytes(chunk));
            if (targetVersion == 2) {
                std::uint16_t fieldFlags = 0;
                if (chunk.grid) {
                    fieldFlags |= 0x0001;
                }
                if (chunk.occluder) {
                    fieldFlags |= 0x0002;
                }
                writer.write(fieldFlags);
                if (chunk.grid) {
                    writer.write(chunk.grid->x);
                    writer.write(chunk.grid->y);
                    writer.write(chunk.grid->z);
                    writer.write(chunk.grid->cellSize);
                }
                if (chunk.occluder) {
                    writer.write(static_cast<std::uint8_t>(*chunk.occluder ? 1 : 0));
                }
            }
        }
    }
    return Result<std::vector<std::uint8_t>>::success(writer.take());
}

std::string archiveVersionName(std::uint16_t version) {
    return "v" + std::to_string(version);
}

} // namespace chunkpipe

