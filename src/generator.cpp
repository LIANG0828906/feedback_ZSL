#include "chunkpipe/generator.hpp"

#include <array>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <map>
#include <sstream>

#include "chunkpipe/archive.hpp"
#include "chunkpipe/hash.hpp"

namespace chunkpipe {

namespace {

Chunk makeChunk(std::uint32_t id,
                std::uint32_t xIndex,
                std::uint32_t zIndex,
                bool frontOccluder) {
    Chunk chunk;
    chunk.id = id;
    const float centerX = static_cast<float>(xIndex) * 6.0f - 6.0f;
    const float centerZ = static_cast<float>(zIndex) * 6.0f + 2.0f;
    chunk.bounds.min = {centerX - 3.0f, 0.0f, centerZ - 3.0f};
    chunk.bounds.max = {centerX + 3.0f, 4.0f, centerZ + 3.0f};

    const float halfX = frontOccluder ? 2.85f : 1.6f;
    const float height = frontOccluder ? 4.0f : 1.5f;
    const float halfZ = frontOccluder ? 2.85f : 1.6f;
    const float minX = centerX - halfX;
    const float maxX = centerX + halfX;
    const float minZ = centerZ - halfZ;
    const float maxZ = centerZ + halfZ;
    chunk.vertices = {
        {minX, 0.0f, minZ}, {maxX, 0.0f, minZ}, {maxX, height, minZ},
        {minX, height, minZ}, {minX, 0.0f, maxZ}, {maxX, 0.0f, maxZ},
        {maxX, height, maxZ}, {minX, height, maxZ}};
    chunk.indices = {
        0, 2, 1, 0, 3, 2, 4, 5, 6, 4, 6, 7, 0, 1, 5, 0, 5, 4,
        1, 2, 6, 1, 6, 5, 2, 3, 7, 2, 7, 6, 3, 0, 4, 3, 4, 7};
    return chunk;
}

SceneArchive buildScene(std::uint16_t version) {
    SceneArchive scene;
    scene.version = version;
    scene.sceneId = 0x53314d50u;
    scene.boundaryTolerance = 0.05f;
    for (std::uint32_t zIndex = 0; zIndex < 3; ++zIndex) {
        for (std::uint32_t xIndex = 0; xIndex < 3; ++xIndex) {
            const std::uint32_t id = zIndex * 3 + xIndex;
            const bool frontOccluder = zIndex == 0;
            Chunk chunk = makeChunk(id, xIndex, zIndex, frontOccluder);
            chunk.priority = derivePriority(chunk.bounds);
            chunk.prioritySource = PrioritySource::DerivedFromBounds;

            if (version >= 2) {
                chunk.grid = GridDivision{2, 1, 2, 3.0f};
                chunk.occluder = frontOccluder;
            }
            if (version == 3) {
                chunk.compression = CompressionDesc{
                    1,
                    static_cast<std::uint64_t>(chunk.vertices.size() * sizeof(Vec3) +
                                               chunk.indices.size() *
                                                   sizeof(std::uint32_t)),
                    0,
                    0};
                const int distanceClass =
                    std::abs(static_cast<int>(id) - 4);
                chunk.priority =
                    static_cast<std::uint32_t>(2000 - distanceClass * 300);
                chunk.prioritySource = PrioritySource::AuthoredV3;
                HierarchyTable hierarchy;
                hierarchy.parent = kNoChunk;
                if (id == 4) {
                    hierarchy.children = {1, 3, 5, 7};
                } else if (id == 1 || id == 3 || id == 5 || id == 7) {
                    hierarchy.parent = 4;
                }
                chunk.hierarchy = std::move(hierarchy);
            }
            scene.chunks.push_back(std::move(chunk));
        }
    }
    return scene;
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

} // namespace

Result<GeneratedSamples> generateDeterministicSamples(
    const std::filesystem::path& directory) {
    std::error_code filesystemError;
    std::filesystem::create_directories(directory, filesystemError);
    if (filesystemError) {
        return Result<GeneratedSamples>::failure(
            Code::IoError, "cannot create sample directory");
    }

    GeneratedSamples generated;
    generated.camera = Camera{};
    generated.camera.position = {0.0f, 2.5f, -12.0f};
    generated.camera.target = {0.0f, 1.5f, 7.0f};
    generated.camera.up = {0.0f, 1.0f, 0.0f};
    generated.camera.fovYDegrees = 60.0f;
    generated.camera.aspect = 16.0f / 9.0f;
    generated.camera.nearPlane = 0.1f;
    generated.camera.farPlane = 100.0f;
    generated.camera.width = 256;
    generated.camera.height = 144;

    std::ostringstream versionsJson;
    versionsJson << std::fixed << std::setprecision(6);
    for (std::uint16_t version = 1; version <= 3; ++version) {
        SceneArchive scene = buildScene(version);
        const auto archivePath =
            directory / ("scene_v" + std::to_string(version) + ".arc");
        Result<void> written = writeArchiveToFile(archivePath, scene, version);
        if (!written.ok()) {
            return Result<GeneratedSamples>::failure(written.error());
        }
        generated.archives.push_back(archivePath);

        Result<SceneArchive> loaded = readArchiveFromFile(archivePath);
        if (!loaded.ok()) {
            return Result<GeneratedSamples>::failure(loaded.error());
        }
        Result<FrameSnapshot> snapshot =
            renderVisibility(loaded.value(), loaded.value().chunks,
                             generated.camera, loaded.value().boundaryTolerance);
        if (!snapshot.ok()) {
            return Result<GeneratedSamples>::failure(snapshot.error());
        }
        if (version > 1) {
            versionsJson << ",";
        }
        versionsJson << "\"" << version << "\":{\"archive\":\""
                     << archivePath.filename().generic_string()
                     << "\",\"archive_content_hash\":\""
                     << hashHex(sceneContentHash(loaded.value()))
                     << "\",\"mask\":\"" << maskHex(snapshot.value())
                     << "\",\"mask_hash\":\""
                     << hashHex(hashString(maskHex(snapshot.value())))
                     << "\",\"snapshot_hash\":\""
                     << hashHex(snapshot.value().contentHash)
                     << "\",\"stats\":" << statsJson(snapshot.value().stats)
                     << "}";
    }

    generated.baselinePath = directory / "baseline.json";
    std::ofstream baseline(generated.baselinePath, std::ios::trunc);
    if (!baseline) {
        return Result<GeneratedSamples>::failure(
            Code::IoError, "cannot create baseline.json");
    }
    baseline << std::fixed << std::setprecision(6);
    baseline << "{\"scene_id\":" << 0x53314d50u
             << ",\"boundary_tolerance\":0.05,\"camera\":{"
             << "\"position\":[" << generated.camera.position.x << ","
             << generated.camera.position.y << "," << generated.camera.position.z
             << "],\"target\":[" << generated.camera.target.x << ","
             << generated.camera.target.y << "," << generated.camera.target.z
             << "],\"up\":[" << generated.camera.up.x << "," << generated.camera.up.y
             << "," << generated.camera.up.z << "],\"fov_y_degrees\":"
             << generated.camera.fovYDegrees << ",\"aspect\":"
             << generated.camera.aspect << ",\"near\":" << generated.camera.nearPlane
             << ",\"far\":" << generated.camera.farPlane << ",\"width\":"
             << generated.camera.width << ",\"height\":"
             << generated.camera.height << "},\"versions\":{"
             << versionsJson.str() << "}}\n";
    baseline.flush();
    if (!baseline) {
        return Result<GeneratedSamples>::failure(
            Code::IoError, "cannot write baseline.json");
    }
    return Result<GeneratedSamples>::success(std::move(generated));
}

} // namespace chunkpipe

