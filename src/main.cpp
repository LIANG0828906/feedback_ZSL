#include <cstdlib>
#include <fstream>
#include <iostream>
#include <string>
#include <vector>

#include "terrain/async_controller.h"
#include "terrain/sample_generator.h"
#include "terrain/scene.h"
#include "terrain/terrain_io.h"
#include "terrain/version_store.h"

namespace {

void usage() {
    std::cout
        << "terrain_app commands:\n"
        << "  gen <samples-dir>\n"
        << "  inspect <height.bin> <material.bin>\n"
        << "  pipeline <samples-dir> [store-dir]\n"
        << "  apply <samples-dir> <store-dir> <sequence>\n"
        << "  recover <store-dir> <patch-dir>\n"
        << "  ppm <samples-dir> <out.ppm>\n";
}

int printStatus(const terrain::Status& status) {
    if (!status.ok()) {
        std::cerr << status.toString() << "\n";
        return 1;
    }
    return 0;
}

int commandGen(const std::string& root) {
    terrain::SamplePaths paths;
    const terrain::Status status =
        terrain::generateDeterministicSamples(root, &paths);
    if (!status.ok()) return printStatus(status);
    std::cout << "height=" << paths.height_field << "\n"
              << "material=" << paths.material_image << "\n"
              << "patches=" << paths.patch_directory << "\n"
              << "store=" << paths.store_directory << "\n";
    return 0;
}

int commandInspect(const std::string& height_path,
                   const std::string& material_path) {
    terrain::TerrainScene scene;
    const terrain::Status status =
        scene.initializeFromFiles(height_path, material_path);
    if (!status.ok()) return printStatus(status);
    std::cout << "width=" << scene.height().width
              << " height=" << scene.height().height
              << " spacing=" << scene.height().cell_spacing
              << " version=" << scene.version()
              << " leaves=" << scene.quadtree().leaves().size()
              << " mesh_vertices=";
    std::size_t vertices = 0;
    for (const auto& entry : scene.meshes()) {
        vertices += entry.second.vertices.size();
    }
    std::cout << vertices << " render="
              << scene.renderer().lastSummary().stableString() << "\n";
    return 0;
}

int commandPipeline(const std::string& samples, std::string store_root) {
    terrain::SamplePaths paths;
    terrain::Status status =
        terrain::generateDeterministicSamples(samples, &paths);
    if (!status.ok()) return printStatus(status);
    if (store_root.empty()) store_root = paths.store_directory;
    terrain::VersionStore store(store_root);
    status = store.initializeFromFiles(paths.height_field,
                                       paths.material_image);
    if (!status.ok()) {
        const terrain::Status loaded = store.loadCurrent(nullptr, nullptr, nullptr);
        if (!loaded.ok()) return printStatus(status);
    }
    terrain::AsyncTerrainController controller;
    terrain::SceneOptions options;
    status = controller.initializeFromStore(&store, options);
    if (!status.ok()) return printStatus(status);
    std::vector<terrain::PatchBatch> batches;
    status = terrain::groupPatchDirectory(paths.patch_directory, &batches);
    if (!status.ok()) return printStatus(status);
    std::cout << "batches=" << batches.size()
              << " current_version=" << controller.scene().version() << "\n";
    for (const terrain::PatchBatch& batch : batches) {
        if (batch.sequence <= controller.scene().version()) continue;
        status = controller.submit(batch, &store);
        if (!status.ok()) return printStatus(status);
        terrain::JobResult result;
        if (!controller.waitForCompletion(60000, &result)) {
            std::cerr << "timeout\n";
            return 1;
        }
        std::cout << result.status.toString() << "\n";
        std::cout << result.report.stableString() << "\n";
        if (!result.status.ok()) return 1;
    }
    return 0;
}

int commandRecover(const std::string& store_root,
                   const std::string& patch_dir) {
    terrain::VersionStore store(store_root);
    terrain::AsyncTerrainController controller;
    terrain::RecoveryReport recovery;
    const terrain::Status status =
        controller.recoverPending(&store, patch_dir, &recovery);
    std::cout << "start_version=" << recovery.started_at_version
              << " end_version=" << recovery.finished_at_version
              << " committed=" << recovery.committed_batches
              << " skipped=" << recovery.skipped_committed_patches << "\n";
    for (const std::string& note : recovery.notes) {
        std::cout << note << "\n";
    }
    return printStatus(status);
}

int commandApplyOne(const std::string& samples, const std::string& store_root,
                    std::uint64_t sequence) {
    terrain::SamplePaths paths;
    terrain::Status status =
        terrain::generateDeterministicSamples(samples, &paths);
    if (!status.ok()) return printStatus(status);
    terrain::VersionStore store(store_root);
    status = store.initializeFromFiles(paths.height_field,
                                       paths.material_image);
    if (!status.ok()) {
        status = store.loadCurrent(nullptr, nullptr, nullptr);
        if (!status.ok()) return printStatus(status);
    }
    terrain::TerrainScene scene;
    status = scene.loadCommitted(&store);
    if (!status.ok()) return printStatus(status);
    std::vector<terrain::PatchBatch> batches;
    status = terrain::groupPatchDirectory(paths.patch_directory, &batches);
    if (!status.ok()) return printStatus(status);
    const terrain::PatchBatch* selected = nullptr;
    for (const terrain::PatchBatch& batch : batches) {
        if (batch.sequence == sequence) selected = &batch;
    }
    if (selected == nullptr) {
        std::cerr << "patch sequence not found\n";
        return 1;
    }
    terrain::SceneUpdateReport report;
    status = scene.applyBatch(
        *selected, &store, terrain::VersionStore::faultFromEnvironment(),
        []() { return false; }, &report);
    std::cout << report.stableString() << "\n";
    return printStatus(status);
}

int commandPpm(const std::string& samples, const std::string& output) {
    terrain::SamplePaths paths;
    terrain::Status status =
        terrain::generateDeterministicSamples(samples, &paths);
    if (!status.ok()) return printStatus(status);
    terrain::TerrainScene scene;
    status = scene.initializeFromFiles(paths.height_field,
                                       paths.material_image);
    if (!status.ok()) return printStatus(status);
    const terrain::OffscreenFrame& frame = scene.renderer().frame();
    std::ofstream stream(output, std::ios::binary);
    if (!stream) {
        std::cerr << "cannot create " << output << "\n";
        return 1;
    }
    stream << "P6\n" << frame.width << " " << frame.height << "\n255\n";
    for (std::size_t i = 0; i < frame.heat.size(); ++i) {
        stream.put(static_cast<char>(frame.color[i * 4u]));
        stream.put(static_cast<char>(frame.color[i * 4u + 1u]));
        stream.put(static_cast<char>(frame.color[i * 4u + 2u]));
    }
    std::cout << "wrote " << output << " "
              << scene.renderer().lastSummary().stableString() << "\n";
    return 0;
}

}  // namespace

int main(int argc, char** argv) {
    if (argc < 2) {
        usage();
        return 0;
    }
    const std::string command = argv[1];
    if (command == "gen" && argc >= 3) return commandGen(argv[2]);
    if (command == "inspect" && argc >= 4) {
        return commandInspect(argv[2], argv[3]);
    }
    if (command == "pipeline" && argc >= 3) {
        return commandPipeline(argv[2], argc >= 4 ? argv[3] : "");
    }
    if (command == "apply" && argc >= 5) {
        return commandApplyOne(argv[2], argv[3], std::stoull(argv[4]));
    }
    if (command == "recover" && argc >= 4) {
        return commandRecover(argv[2], argv[3]);
    }
    if (command == "ppm" && argc >= 4) {
        return commandPpm(argv[2], argv[3]);
    }
    usage();
    return 1;
}

