#include "chunkpipe/compare.hpp"

#include <algorithm>
#include <cmath>
#include <iomanip>
#include <map>
#include <set>
#include <sstream>
#include <unordered_map>

#include "chunkpipe/archive.hpp"
#include "chunkpipe/hash.hpp"
#include "chunkpipe/trace.hpp"

namespace chunkpipe {

namespace {

struct Variant {
    SceneArchive scene;
    FrameSnapshot snapshot;
    std::vector<std::uint32_t> selectedOrder;
    std::set<std::uint32_t> loadedSet;
};

std::vector<std::uint32_t> selectedPriorityOrder(const SceneArchive& scene,
                                                 const Camera& camera,
                                                 std::uint32_t maxChunks) {
    std::vector<const Chunk*> chunks;
    chunks.reserve(scene.chunks.size());
    for (const Chunk& chunk : scene.chunks) {
        chunks.push_back(&chunk);
    }
    std::sort(chunks.begin(), chunks.end(), [&](const Chunk* a, const Chunk* b) {
        if (a->priority != b->priority) {
            return a->priority > b->priority;
        }
        const float distanceA = length(a->bounds.center() - camera.position);
        const float distanceB = length(b->bounds.center() - camera.position);
        if (distanceA != distanceB) {
            return distanceA < distanceB;
        }
        return a->id < b->id;
    });
    if (maxChunks > 0 && chunks.size() > maxChunks) {
        chunks.resize(maxChunks);
    }
    std::vector<std::uint32_t> ids;
    ids.reserve(chunks.size());
    for (const Chunk* chunk : chunks) {
        ids.push_back(chunk->id);
    }
    return ids;
}

std::uint8_t maskFor(const FrameSnapshot& snapshot, std::uint32_t chunkId) {
    for (std::size_t index = 0; index < snapshot.chunkIds.size(); ++index) {
        if (snapshot.chunkIds[index] == chunkId) {
            return snapshot.visibilityMask[index];
        }
    }
    return 0;
}

bool sameOptionalUint32(const std::optional<std::uint32_t>& a,
                        const std::optional<std::uint32_t>& b) {
    return a == b;
}

std::string cameraJson(const Camera& camera) {
    std::ostringstream output;
    output << std::fixed << std::setprecision(6);
    output << "{\"position\":[" << camera.position.x << "," << camera.position.y
           << "," << camera.position.z << "],\"target\":[" << camera.target.x << ","
           << camera.target.y << "," << camera.target.z << "],\"up\":["
           << camera.up.x << "," << camera.up.y << "," << camera.up.z
           << "],\"fov_y_degrees\":" << camera.fovYDegrees
           << ",\"aspect\":" << camera.aspect << ",\"near\":" << camera.nearPlane
           << ",\"far\":" << camera.farPlane << ",\"width\":" << camera.width
           << ",\"height\":" << camera.height << "}";
    return output.str();
}

std::string statsJson(const CullStats& stats) {
    std::ostringstream output;
    output << "{\"scene_chunks\":" << stats.sceneChunks
           << ",\"loaded_chunks\":" << stats.loadedChunks
           << ",\"frustum_culled\":" << stats.frustumCulled
           << ",\"occlusion_culled\":" << stats.occlusionCulled
           << ",\"visible_chunks\":" << stats.visibleChunks
           << ",\"critical_chunks\":" << stats.criticalChunks
           << ",\"boundary_pairs\":" << stats.boundaryPairs << "}";
    return output.str();
}

std::string diffJson(const VisibilityDiff& diff) {
    std::ostringstream output;
    output << "{\"chunk_id\":" << diff.chunkId << ",\"from_version\":"
           << diff.fromVersion << ",\"to_version\":" << diff.toVersion
           << ",\"from_mask\":" << static_cast<unsigned>(diff.fromMask)
           << ",\"to_mask\":" << static_cast<unsigned>(diff.toMask)
           << ",\"cause\":\"" << jsonEscape(diff.cause) << "\",\"fields\":[";
    for (std::size_t index = 0; index < diff.fields.size(); ++index) {
        if (index != 0) {
            output << ",";
        }
        output << "\"" << jsonEscape(diff.fields[index]) << "\"";
    }
    output << "]}";
    return output.str();
}

} // namespace

Result<CompareReport> compareArchives(
    const std::vector<std::filesystem::path>& paths,
    const Camera& camera,
    std::uint32_t maxLoadedChunks) {
    if (paths.size() < 2) {
        return Result<CompareReport>::failure(
            Code::ChunkInvalid,
            "at least two archive paths are required for comparison");
    }
    std::map<std::uint16_t, Variant> variants;
    for (const auto& path : paths) {
        Result<SceneArchive> scene = readArchiveFromFile(path);
        if (!scene.ok()) {
            return Result<CompareReport>::failure(scene.error());
        }
        const std::uint16_t version = scene.value().version;
        if (variants.find(version) != variants.end()) {
            return Result<CompareReport>::failure(
                Code::ChunkInvalid,
                "duplicate archive version in comparison: " +
                    archiveVersionName(version));
        }
        Variant variant;
        const std::vector<std::uint32_t> selectedOrder =
            selectedPriorityOrder(scene.value(), camera, maxLoadedChunks);
        std::vector<Chunk> loaded;
        for (const std::uint32_t id : selectedOrder) {
            for (const Chunk& chunk : scene.value().chunks) {
                if (chunk.id == id) {
                    loaded.push_back(chunk);
                    break;
                }
            }
        }
        Result<FrameSnapshot> snapshot =
            renderVisibility(scene.value(), loaded, camera,
                             scene.value().boundaryTolerance);
        if (!snapshot.ok()) {
            return Result<CompareReport>::failure(snapshot.error());
        }
        variant.selectedOrder = selectedOrder;
        variant.loadedSet.insert(selectedOrder.begin(), selectedOrder.end());
        variant.scene = std::move(scene.value());
        variant.snapshot = std::move(snapshot.value());
        variants.emplace(version, std::move(variant));
    }

    const std::uint32_t sceneId = variants.begin()->second.scene.sceneId;
    for (const auto& entry : variants) {
        if (entry.second.scene.sceneId != sceneId) {
            BusinessError error;
            error.code = Code::ChunkInvalid;
            error.message =
                "archives do not share the same scene coordinate baseline";
            ErrorLocation location;
            location.field = "scene_id";
            location.detail = "version " + std::to_string(entry.first) +
                              " scene id differs";
            error.locations.push_back(std::move(location));
            return Result<CompareReport>::failure(std::move(error));
        }
    }

    CompareReport report;
    report.sceneId = sceneId;
    report.camera = camera;
    for (const auto& entry : variants) {
        report.versions.push_back(entry.first);
        report.maskHashes[entry.first] =
            hashString(maskHex(entry.second.snapshot));
        report.snapshotHashes[entry.first] = entry.second.snapshot.contentHash;
        report.stats[entry.first] = entry.second.snapshot.stats;
    }

    for (std::size_t i = 0; i < report.versions.size(); ++i) {
        for (std::size_t j = i + 1; j < report.versions.size(); ++j) {
            const std::uint16_t fromVersion = report.versions[i];
            const std::uint16_t toVersion = report.versions[j];
            const Variant& from = variants.at(fromVersion);
            const Variant& to = variants.at(toVersion);
            std::set<std::uint32_t> chunkIds;
            for (const Chunk& chunk : from.scene.chunks) {
                chunkIds.insert(chunk.id);
            }
            for (const Chunk& chunk : to.scene.chunks) {
                chunkIds.insert(chunk.id);
            }

            for (const std::uint32_t chunkId : chunkIds) {
                const auto fromChunkIt =
                    std::find_if(from.scene.chunks.begin(), from.scene.chunks.end(),
                                 [&](const Chunk& chunk) {
                                     return chunk.id == chunkId;
                                 });
                const auto toChunkIt =
                    std::find_if(to.scene.chunks.begin(), to.scene.chunks.end(),
                                 [&](const Chunk& chunk) {
                                     return chunk.id == chunkId;
                                 });
                if (fromChunkIt == from.scene.chunks.end() ||
                    toChunkIt == to.scene.chunks.end()) {
                    continue;
                }
                const Chunk& a = *fromChunkIt;
                const Chunk& b = *toChunkIt;

                std::vector<std::string> fields;
                bool formatDifference = false;
                bool priorityDifference = false;
                bool loadedDifference =
                    from.loadedSet.count(chunkId) != to.loadedSet.count(chunkId);
                if (!sameOptionalUint32(
                        a.occluder ? std::optional<std::uint32_t>(
                                         static_cast<std::uint32_t>(*a.occluder))
                                   : std::nullopt,
                        b.occluder ? std::optional<std::uint32_t>(
                                         static_cast<std::uint32_t>(*b.occluder))
                                   : std::nullopt)) {
                    fields.push_back("occluder");
                    formatDifference = true;
                }
                if (static_cast<bool>(a.grid) != static_cast<bool>(b.grid) ||
                    (a.grid && b.grid &&
                     (a.grid->x != b.grid->x || a.grid->y != b.grid->y ||
                      a.grid->z != b.grid->z ||
                      std::abs(a.grid->cellSize - b.grid->cellSize) > 1e-5f))) {
                    fields.push_back("grid");
                    formatDifference = true;
                }
                if (static_cast<bool>(a.compression) !=
                        static_cast<bool>(b.compression) ||
                    (a.compression && b.compression &&
                     (a.compression->codec != b.compression->codec ||
                      a.compression->rawBytes != b.compression->rawBytes))) {
                    fields.push_back("compression");
                    formatDifference = true;
                }
                if (static_cast<bool>(a.hierarchy) !=
                        static_cast<bool>(b.hierarchy)) {
                    fields.push_back("hierarchy");
                    formatDifference = true;
                }
                if (a.priority != b.priority ||
                    a.prioritySource != b.prioritySource) {
                    fields.push_back("priority");
                    fields.push_back("priority.source");
                    priorityDifference = true;
                }
                if (!(a.bounds.min.x == b.bounds.min.x &&
                      a.bounds.min.y == b.bounds.min.y &&
                      a.bounds.min.z == b.bounds.min.z &&
                      a.bounds.max.x == b.bounds.max.x &&
                      a.bounds.max.y == b.bounds.max.y &&
                      a.bounds.max.z == b.bounds.max.z)) {
                    fields.push_back("bounds");
                    formatDifference = true;
                }
                if (loadedDifference) {
                    fields.push_back("loaded_set");
                }

                const std::uint8_t maskA = maskFor(from.snapshot, chunkId);
                const std::uint8_t maskB = maskFor(to.snapshot, chunkId);
                if (maskA != maskB) {
                    VisibilityDiff diff;
                    diff.chunkId = chunkId;
                    diff.fromVersion = fromVersion;
                    diff.toVersion = toVersion;
                    diff.fromMask = maskA;
                    diff.toMask = maskB;
                    diff.fields = fields;
                    if (loadedDifference) {
                        diff.cause = "load_order";
                    } else if (formatDifference) {
                        diff.cause = "format_degradation";
                    } else if (priorityDifference) {
                        diff.cause = "priority_derivation";
                    } else {
                        diff.cause = "format_degradation";
                    }
                    report.differences.push_back(std::move(diff));
                } else if (!fields.empty()) {
                    VisibilityDiff diff;
                    diff.chunkId = chunkId;
                    diff.fromVersion = fromVersion;
                    diff.toVersion = toVersion;
                    diff.fromMask = maskA;
                    diff.toMask = maskB;
                    diff.fields = fields;
                    if (loadedDifference) {
                        diff.cause = "load_order";
                    } else if (priorityDifference) {
                        diff.cause = "priority_derivation";
                    } else if (formatDifference) {
                        diff.cause = "format_degradation";
                    } else {
                        diff.cause = "priority_derivation";
                    }
                    report.metadataDifferences.push_back(std::move(diff));
                }
            }
        }
    }
    return Result<CompareReport>::success(std::move(report));
}

std::string compareReportJson(const CompareReport& report) {
    std::ostringstream output;
    output << std::fixed << std::setprecision(6);
    output << "{\"scene_id\":" << report.sceneId << ",\"camera\":"
           << cameraJson(report.camera) << ",\"versions\":[";
    for (std::size_t index = 0; index < report.versions.size(); ++index) {
        if (index != 0) {
            output << ",";
        }
        output << report.versions[index];
    }
    output << "],\"mask_hashes\":{";
    bool first = true;
    for (const auto& entry : report.maskHashes) {
        if (!first) {
            output << ",";
        }
        first = false;
        output << "\"" << entry.first << "\":\"" << hashHex(entry.second) << "\"";
    }
    output << "},\"snapshot_hashes\":{";
    first = true;
    for (const auto& entry : report.snapshotHashes) {
        if (!first) {
            output << ",";
        }
        first = false;
        output << "\"" << entry.first << "\":\"" << hashHex(entry.second) << "\"";
    }
    output << "},\"stats\":{";
    first = true;
    for (const auto& entry : report.stats) {
        if (!first) {
            output << ",";
        }
        first = false;
        output << "\"" << entry.first << "\":" << statsJson(entry.second);
    }
    output << "},\"differences\":[";
    for (std::size_t index = 0; index < report.differences.size(); ++index) {
        if (index != 0) {
            output << ",";
        }
        output << diffJson(report.differences[index]);
    }
    output << "],\"metadata_differences\":[";
    for (std::size_t index = 0; index < report.metadataDifferences.size(); ++index) {
        if (index != 0) {
            output << ",";
        }
        output << diffJson(report.metadataDifferences[index]);
    }
    output << "]}\n";
    return output.str();
}

std::string visibilityDiffJsonLines(const CompareReport& report) {
    std::ostringstream output;
    for (const VisibilityDiff& diff : report.differences) {
        output << diffJson(diff) << "\n";
    }
    return output.str();
}

} // namespace chunkpipe

