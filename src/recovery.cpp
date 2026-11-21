#include "chunkpipe/recovery.hpp"

#include <algorithm>
#include <array>
#include <cstring>
#include <fstream>
#include <iomanip>
#include <limits>
#include <sstream>
#include <type_traits>
#include <unordered_map>

#include "chunkpipe/hash.hpp"

namespace chunkpipe {

namespace {

constexpr std::array<std::uint8_t, 8> kSnapshotMagic = {'C', 'P', 'S', 'N',
                                                        'A', 'P', '0', '1'};
constexpr std::uint32_t kSnapshotFormat = 1;
constexpr std::uint64_t kGenesisChain = 0;

struct RecoveryFailure {
    std::string field;
    std::string detail;
};

[[noreturn]] void recoveryFail(std::string field, std::string detail) {
    throw RecoveryFailure{std::move(field), std::move(detail)};
}

class ByteCursor {
public:
    ByteCursor(const std::vector<std::uint8_t>& bytes, std::size_t limit)
        : bytes_(bytes), limit_(limit) {}

    template <typename T>
    T read(const std::string& field) {
        static_assert(std::is_trivially_copyable<T>::value, "scalar required");
        if (position_ + sizeof(T) > limit_) {
            recoveryFail(field, "field is missing or truncated");
        }
        T value{};
        std::memcpy(&value, bytes_.data() + position_, sizeof(T));
        position_ += sizeof(T);
        return value;
    }

    Vec3 readVec3(const std::string& field) {
        return {read<float>(field + ".x"), read<float>(field + ".y"),
                read<float>(field + ".z")};
    }

    std::vector<std::uint8_t> readBytes(std::uint64_t count,
                                        const std::string& field) {
        if (count > limit_ - position_) {
            recoveryFail(field, "declared byte count exceeds snapshot body");
        }
        std::vector<std::uint8_t> value(static_cast<std::size_t>(count));
        if (count > 0) {
            std::memcpy(value.data(), bytes_.data() + position_,
                        static_cast<std::size_t>(count));
            position_ += static_cast<std::size_t>(count);
        }
        return value;
    }

    std::size_t position() const { return position_; }

private:
    const std::vector<std::uint8_t>& bytes_;
    std::size_t limit_;
    std::size_t position_ = 0;
};

template <typename T>
void appendScalar(std::vector<std::uint8_t>& bytes, const T& value) {
    static_assert(std::is_trivially_copyable<T>::value, "scalar required");
    const auto* data = reinterpret_cast<const std::uint8_t*>(&value);
    bytes.insert(bytes.end(), data, data + sizeof(T));
}

void appendVec3(std::vector<std::uint8_t>& bytes, const Vec3& value) {
    appendScalar(bytes, value.x);
    appendScalar(bytes, value.y);
    appendScalar(bytes, value.z);
}

Result<RecoveryState> recoveryError(std::uint32_t chunkId,
                                    const std::string& field,
                                    const std::string& detail) {
    BusinessError error;
    error.code = Code::RecoveryInvalid;
    error.message = "recovery snapshot validation failed";
    ErrorLocation location;
    location.chunkId = chunkId;
    location.field = field;
    location.detail = detail;
    error.locations.push_back(std::move(location));
    return Result<RecoveryState>::failure(std::move(error));
}

Result<CommitResult> commitError(std::uint32_t chunkId,
                                 Code code,
                                 const std::string& field,
                                 const std::string& detail) {
    BusinessError error;
    error.code = code;
    error.message = "recovery snapshot commit failed";
    ErrorLocation location;
    location.chunkId = chunkId;
    location.field = field;
    location.detail = detail;
    error.locations.push_back(std::move(location));
    return Result<CommitResult>::failure(std::move(error));
}

struct FileIdentity {
    std::uint32_t sequence = 0;
    std::uint32_t chunkId = 0;
};

bool parseFileName(const std::filesystem::path& path, FileIdentity& identity) {
    const std::string name = path.filename().string();
    if (name.size() < 23 || name.substr(name.size() - 5) != ".snap") {
        return false;
    }
    const std::size_t dash = name.find("-c");
    if (dash != 8) {
        return false;
    }
    try {
        identity.sequence = static_cast<std::uint32_t>(std::stoul(name.substr(0, 8)));
        identity.chunkId =
            static_cast<std::uint32_t>(std::stoul(name.substr(10, 8)));
    } catch (const std::exception&) {
        return false;
    }
    return true;
}

std::string snapshotFileName(std::uint32_t sequence, std::uint32_t chunkId) {
    std::ostringstream name;
    name << std::setfill('0') << std::setw(8) << sequence << "-c" << std::setw(8)
         << chunkId << ".snap";
    return name.str();
}

std::vector<std::uint8_t> buildSnapshot(const SceneArchive& scene,
                                        const Chunk& chunk,
                                        std::uint32_t sequence,
                                        std::uint64_t previousChain,
                                        std::uint64_t contentHash,
                                        std::uint8_t schedulerState) {
    std::vector<std::uint8_t> body;
    body.insert(body.end(), kSnapshotMagic.begin(), kSnapshotMagic.end());
    appendScalar(body, kSnapshotFormat);
    appendScalar(body, sequence);
    appendScalar(body, scene.sceneId);
    appendScalar(body, scene.version);
    appendScalar(body, static_cast<std::uint16_t>(0));
    appendScalar(body, chunk.id);
    appendScalar(body, schedulerState);
    appendScalar(body, static_cast<std::uint8_t>(0));
    appendScalar(body, static_cast<std::uint16_t>(0));
    appendScalar(body, previousChain);
    appendScalar(body, contentHash);
    appendVec3(body, chunk.bounds.min);
    appendVec3(body, chunk.bounds.max);
    appendScalar(body, static_cast<std::uint32_t>(chunk.vertices.size()));
    appendScalar(body, static_cast<std::uint32_t>(chunk.indices.size()));
    appendScalar(body, chunk.priority);
    appendScalar(body, static_cast<std::uint32_t>(chunk.prioritySource));

    std::uint16_t fieldFlags = 0;
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
    appendScalar(body, fieldFlags);
    if (chunk.grid) {
        appendScalar(body, chunk.grid->x);
        appendScalar(body, chunk.grid->y);
        appendScalar(body, chunk.grid->z);
        appendScalar(body, chunk.grid->cellSize);
    }
    if (chunk.occluder) {
        appendScalar(body, static_cast<std::uint8_t>(*chunk.occluder ? 1 : 0));
    }
    if (chunk.compression) {
        appendScalar(body, chunk.compression->codec);
        appendScalar(body, chunk.compression->flags);
        appendScalar(body, chunk.compression->rawBytes);
        appendScalar(body, chunk.compression->storedBytes);
    }
    if (chunk.hierarchy) {
        appendScalar(body, chunk.hierarchy->parent);
        appendScalar(body,
                     static_cast<std::uint32_t>(chunk.hierarchy->children.size()));
        for (const std::uint32_t child : chunk.hierarchy->children) {
            appendScalar(body, child);
        }
    }
    if (!chunk.vertices.empty()) {
        const auto* data = reinterpret_cast<const std::uint8_t*>(chunk.vertices.data());
        body.insert(body.end(), data,
                    data + chunk.vertices.size() * sizeof(Vec3));
    }
    if (!chunk.indices.empty()) {
        const auto* data = reinterpret_cast<const std::uint8_t*>(chunk.indices.data());
        body.insert(body.end(), data,
                    data + chunk.indices.size() * sizeof(std::uint32_t));
    }
    const std::uint64_t chain = hashBytes(body);
    appendScalar(body, chain);
    return body;
}

RecoveredChunk parseSnapshot(const std::vector<std::uint8_t>& bytes,
                             const FileIdentity& identity) {
    if (bytes.size() < 8 + 4 + 8) {
        recoveryFail("snapshot", "snapshot file is truncated");
    }
    const std::uint64_t storedChain = [&]() {
        std::uint64_t value = 0;
        std::memcpy(&value, bytes.data() + bytes.size() - sizeof(value), sizeof(value));
        return value;
    }();
    const std::uint64_t computedChain =
        hashBytes(bytes.data(), bytes.size() - sizeof(std::uint64_t));
    if (storedChain != computedChain) {
        recoveryFail("snapshot.chain", "snapshot hash chain is broken");
    }

    ByteCursor cursor(bytes, bytes.size() - sizeof(std::uint64_t));
    const std::vector<std::uint8_t> magic =
        cursor.readBytes(8, "snapshot.magic");
    if (magic != std::vector<std::uint8_t>(kSnapshotMagic.begin(), kSnapshotMagic.end())) {
        recoveryFail("snapshot.magic", "snapshot magic does not match");
    }
    const std::uint32_t format = cursor.read<std::uint32_t>("snapshot.format");
    if (format != kSnapshotFormat) {
        recoveryFail("snapshot.format", "unsupported snapshot format version");
    }
    RecoveredChunk recovered;
    recovered.sequence = cursor.read<std::uint32_t>("snapshot.sequence");
    recovered.sceneId = cursor.read<std::uint32_t>("snapshot.scene_id");
    recovered.archiveVersion =
        cursor.read<std::uint16_t>("snapshot.scene_version");
    (void)cursor.read<std::uint16_t>("snapshot.reserved");
    recovered.chunk.id = cursor.read<std::uint32_t>("chunk.id");
    recovered.schedulerState =
        cursor.read<std::uint8_t>("snapshot.scheduler_state");
    (void)cursor.read<std::uint8_t>("snapshot.reserved2");
    (void)cursor.read<std::uint16_t>("snapshot.reserved3");
    recovered.previousChainHash =
        cursor.read<std::uint64_t>("snapshot.previous_chain");
    recovered.contentHash = cursor.read<std::uint64_t>("snapshot.content_hash");
    recovered.chunk.bounds.min = cursor.readVec3("chunk.bounds.min");
    recovered.chunk.bounds.max = cursor.readVec3("chunk.bounds.max");
    const std::uint32_t vertexCount =
        cursor.read<std::uint32_t>("chunk.vertex_count");
    const std::uint32_t indexCount =
        cursor.read<std::uint32_t>("chunk.index_count");
    recovered.chunk.priority = cursor.read<std::uint32_t>("chunk.priority");
    const std::uint32_t prioritySource =
        cursor.read<std::uint32_t>("chunk.priority_source");
    const auto gridFlags = cursor.read<std::uint16_t>("chunk.field_flags");
    if ((gridFlags & ~static_cast<std::uint16_t>(0x000f)) != 0) {
        recoveryFail("chunk.field_flags", "unknown snapshot field flag bit");
    }
    if (prioritySource > static_cast<std::uint32_t>(PrioritySource::DerivedFromBounds)) {
        recoveryFail("chunk.priority_source", "unknown priority provenance");
    }
    recovered.chunk.prioritySource =
        static_cast<PrioritySource>(prioritySource);
    if (gridFlags & 0x0001) {
        GridDivision grid;
        grid.x = cursor.read<std::uint32_t>("chunk.grid.x");
        grid.y = cursor.read<std::uint32_t>("chunk.grid.y");
        grid.z = cursor.read<std::uint32_t>("chunk.grid.z");
        grid.cellSize = cursor.read<float>("chunk.grid.cell_size");
        recovered.chunk.grid = grid;
    }
    if (gridFlags & 0x0002) {
        const std::uint8_t marker = cursor.read<std::uint8_t>("chunk.occluder");
        if (marker > 1) {
            recoveryFail("chunk.occluder", "occluder marker must be zero or one");
        }
        recovered.chunk.occluder = marker != 0;
    }
    if (gridFlags & 0x0004) {
        CompressionDesc compression;
        compression.codec = cursor.read<std::uint32_t>("chunk.compression.codec");
        compression.flags = cursor.read<std::uint32_t>("chunk.compression.flags");
        compression.rawBytes =
            cursor.read<std::uint64_t>("chunk.compression.raw_bytes");
        compression.storedBytes =
            cursor.read<std::uint64_t>("chunk.compression.stored_bytes");
        recovered.chunk.compression = compression;
    }
    if (gridFlags & 0x0008) {
        HierarchyTable hierarchy;
        hierarchy.parent = cursor.read<std::uint32_t>("chunk.hierarchy.parent");
        const std::uint32_t childCount =
            cursor.read<std::uint32_t>("chunk.hierarchy.child_count");
        if (childCount > 1'000'000) {
            recoveryFail("chunk.hierarchy.child_count",
                         "child count exceeds safety limit");
        }
        for (std::uint32_t index = 0; index < childCount; ++index) {
            hierarchy.children.push_back(
                cursor.read<std::uint32_t>("chunk.hierarchy.children"));
        }
        recovered.chunk.hierarchy = std::move(hierarchy);
    }
    const std::uint64_t vertexBytes =
        static_cast<std::uint64_t>(vertexCount) * sizeof(Vec3);
    const std::uint64_t indexBytes =
        static_cast<std::uint64_t>(indexCount) * sizeof(std::uint32_t);
    const std::vector<std::uint8_t> vertices =
        cursor.readBytes(vertexBytes, "chunk.vertices");
    const std::vector<std::uint8_t> indices =
        cursor.readBytes(indexBytes, "chunk.indices");
    if (cursor.position() != bytes.size() - sizeof(std::uint64_t)) {
        recoveryFail("snapshot.trailing", "snapshot contains unparsed data");
    }
    recovered.chunk.vertices.resize(vertexCount);
    recovered.chunk.indices.resize(indexCount);
    if (!vertices.empty()) {
        std::memcpy(recovered.chunk.vertices.data(), vertices.data(),
                    vertices.size());
    }
    if (!indices.empty()) {
        std::memcpy(recovered.chunk.indices.data(), indices.data(), indices.size());
    }
    if (recovered.sequence != identity.sequence ||
        recovered.chunk.id != identity.chunkId) {
        recoveryFail("snapshot.file_identity",
                     "file name does not match snapshot sequence or chunk id");
    }
    if (recovered.archiveVersion == 0) {
        recoveryFail("snapshot.scene_version", "invalid scene version");
    }
    const std::uint64_t computedContent = chunkContentHash(recovered.chunk);
    if (computedContent != recovered.contentHash) {
        recoveryFail("snapshot.content_hash", "chunk content hash does not match");
    }
    recovered.chainHash = storedChain;
    return recovered;
}

} // namespace

RecoveryStore::RecoveryStore(std::filesystem::path directory)
    : directory_(std::move(directory)) {}

Result<RecoveryState> RecoveryStore::recover(const SceneArchive& scene) {
    recovered_ = true;
    sceneId_ = scene.sceneId;
    sceneVersion_ = scene.version;
    nextSequence_ = 1;
    chainHash_ = kGenesisChain;
    committed_.clear();
    writeCount_ = 0;

    std::error_code filesystemError;
    std::filesystem::create_directories(directory_, filesystemError);
    if (filesystemError) {
        return recoveryError(0, "recovery.directory",
                             "cannot create recovery directory");
    }

    std::vector<std::filesystem::path> files;
    std::filesystem::directory_iterator iterator(directory_, filesystemError);
    if (filesystemError) {
        return recoveryError(0, "recovery.directory",
                             "cannot enumerate recovery directory");
    }
    const std::filesystem::directory_iterator end;
    while (iterator != end) {
        const auto& entry = *iterator;
        if (entry.is_regular_file(filesystemError)) {
            if (filesystemError) {
                return recoveryError(0, "recovery.directory",
                                     "cannot inspect recovery directory entry");
            }
            FileIdentity identity;
            if (parseFileName(entry.path(), identity)) {
                files.push_back(entry.path());
            }
        }
        iterator.increment(filesystemError);
        if (filesystemError) {
            return recoveryError(0, "recovery.directory",
                                 "cannot enumerate recovery directory");
        }
    }
    std::sort(files.begin(), files.end());

    std::unordered_map<std::uint32_t, const Chunk*> archiveChunks;
    for (const Chunk& chunk : scene.chunks) {
        archiveChunks[chunk.id] = &chunk;
    }

    RecoveryState state;
    std::uint32_t expectedSequence = 1;
    for (const std::filesystem::path& path : files) {
        FileIdentity identity;
        if (!parseFileName(path, identity)) {
            return recoveryError(0, "snapshot.file_name",
                                 "cannot parse recovery file name");
        }
        try {
            std::ifstream input(path, std::ios::binary);
            if (!input) {
                return recoveryError(identity.chunkId, "snapshot.file",
                                     "cannot open snapshot file");
            }
            input.seekg(0, std::ios::end);
            const std::streamoff size = input.tellg();
            input.seekg(0, std::ios::beg);
            if (size < 0) {
                return recoveryError(identity.chunkId, "snapshot.file",
                                     "cannot determine snapshot size");
            }
            std::vector<std::uint8_t> bytes(static_cast<std::size_t>(size));
            if (size > 0) {
                input.read(reinterpret_cast<char*>(bytes.data()), size);
            }
            if (input.bad()) {
                return recoveryError(identity.chunkId, "snapshot.file",
                                     "cannot read snapshot file");
            }

            if (identity.sequence != expectedSequence) {
                std::ostringstream detail;
                detail << "expected sequence " << expectedSequence << " but found "
                       << identity.sequence;
                return recoveryError(identity.chunkId, "snapshot.sequence",
                                     detail.str());
            }
            RecoveredChunk recovered = parseSnapshot(bytes, identity);
            if (recovered.chunk.id != identity.chunkId) {
                return recoveryError(identity.chunkId, "chunk.id",
                                     "snapshot chunk id does not match file name");
            }
            const auto archiveIt = archiveChunks.find(recovered.chunk.id);
            if (archiveIt == archiveChunks.end()) {
                return recoveryError(recovered.chunk.id, "chunk.id",
                                     "snapshot chunk is absent from archive");
            }
            if (recovered.sceneId != scene.sceneId ||
                recovered.archiveVersion != scene.version) {
                return recoveryError(recovered.chunk.id, "snapshot.scene_identity",
                                     "snapshot scene id or archive version differs");
            }
            if (recovered.previousChainHash != chainHash_) {
                return recoveryError(recovered.chunk.id, "snapshot.previous_chain",
                                     "snapshot hash chain is broken");
            }
            if (recovered.contentHash != chunkContentHash(*archiveIt->second)) {
                return recoveryError(recovered.chunk.id, "snapshot.content_hash",
                                     "snapshot chunk differs from archive chunk");
            }
            if (committed_.find(recovered.chunk.id) != committed_.end()) {
                return recoveryError(recovered.chunk.id, "chunk.id",
                                     "duplicate committed chunk snapshot");
            }
            state.chunks.push_back(recovered);
            committed_[recovered.chunk.id] = recovered;
            chainHash_ = recovered.chainHash;
            nextSequence_ = identity.sequence + 1;
            expectedSequence = identity.sequence + 1;
        } catch (const RecoveryFailure& failure) {
            return recoveryError(identity.chunkId, failure.field, failure.detail);
        } catch (const std::exception& exception) {
            return recoveryError(identity.chunkId, "snapshot",
                                 std::string("snapshot parse exception: ") +
                                     exception.what());
        }
    }
    state.nextSequence = nextSequence_;
    state.chainHash = chainHash_;
    return Result<RecoveryState>::success(std::move(state));
}

Result<CommitResult> RecoveryStore::commitChunk(const SceneArchive& scene,
                                                const Chunk& chunk,
                                                std::uint64_t contentHash,
                                                std::uint8_t schedulerState) {
    if (!recovered_) {
        return commitError(chunk.id, Code::Internal, "recovery.state",
                           "recover must be called before commit");
    }
    if (scene.sceneId != sceneId_ || scene.version != sceneVersion_) {
        return commitError(chunk.id, Code::RecoveryInvalid,
                           "snapshot.scene_identity",
                           "scene identity changed after recovery initialization");
    }
    if (contentHash != chunkContentHash(chunk)) {
        return commitError(chunk.id, Code::RecoveryInvalid,
                           "snapshot.content_hash",
                           "provided content hash does not match chunk");
    }
    const auto existing = committed_.find(chunk.id);
    if (existing != committed_.end()) {
        return Result<CommitResult>::success(
            CommitResult{false, existing->second.sequence, chainHash_});
    }

    std::error_code filesystemError;
    std::filesystem::create_directories(directory_, filesystemError);
    if (filesystemError) {
        return commitError(chunk.id, Code::IoError, "recovery.directory",
                           "cannot create recovery directory");
    }

    const std::uint32_t sequence = nextSequence_;
    const auto finalPath = directory_ / snapshotFileName(sequence, chunk.id);
    auto tempPath = finalPath;
    tempPath += ".tmp";
    std::error_code removeError;
    std::filesystem::remove(tempPath, removeError);

    const std::vector<std::uint8_t> bytes =
        buildSnapshot(scene, chunk, sequence, chainHash_, contentHash, schedulerState);
    {
        std::ofstream output(tempPath, std::ios::binary | std::ios::trunc);
        if (!output) {
            return commitError(chunk.id, Code::IoError, "recovery.temp_file",
                               "cannot create temporary snapshot");
        }
        output.write(reinterpret_cast<const char*>(bytes.data()),
                     static_cast<std::streamsize>(bytes.size()));
        output.flush();
        if (!output) {
            return commitError(chunk.id, Code::IoError, "recovery.temp_file",
                               "cannot write temporary snapshot");
        }
    }
    std::filesystem::rename(tempPath, finalPath, filesystemError);
    if (filesystemError) {
        std::filesystem::remove(tempPath, removeError);
        return commitError(chunk.id, Code::IoError, "recovery.rename",
                           "atomic snapshot rename failed");
    }

    RecoveredChunk recovered;
    recovered.sequence = sequence;
    recovered.sceneId = scene.sceneId;
    recovered.archiveVersion = scene.version;
    recovered.chunk = chunk;
    recovered.contentHash = contentHash;
    recovered.previousChainHash = chainHash_;
    recovered.chainHash = hashBytes(bytes.data(), bytes.size() - sizeof(std::uint64_t));
    recovered.schedulerState = schedulerState;
    committed_[chunk.id] = recovered;
    chainHash_ = recovered.chainHash;
    nextSequence_ = sequence + 1;
    ++writeCount_;
    return Result<CommitResult>::success(
        CommitResult{true, sequence, chainHash_});
}

bool RecoveryStore::hasCommitted(std::uint32_t chunkId) const {
    return committed_.find(chunkId) != committed_.end();
}

const std::filesystem::path& RecoveryStore::directory() const {
    return directory_;
}

std::size_t RecoveryStore::writeCount() const {
    return writeCount_;
}

} // namespace chunkpipe

