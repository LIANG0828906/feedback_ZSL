#include "terrain/scene.h"

#include <algorithm>
#include <sstream>
#include <unordered_set>

#include "terrain/terrain_io.h"

namespace terrain {
namespace {

bool isCancelled(const CancelCheck& cancelled) {
    return cancelled && cancelled();
}

Status cancelledStatus() {
    return Status::error(E_CANCELLED, "job",
                         "cooperative cancellation requested");
}

void appendUnique(std::vector<std::uint64_t>* values, std::uint64_t value) {
    if (std::find(values->begin(), values->end(), value) == values->end()) {
        values->push_back(value);
    }
}

}  // namespace

std::string SceneUpdateReport::stableString() const {
    std::ostringstream stream;
    stream << "status=" << errorCodeName(status.code)
           << " patch=" << patch_sequence
           << " version=" << committed_version
           << " invalid_blocks=" << invalidation.invalid_blocks.size()
           << " recomputed_blocks=" << recomputed_blocks.size()
           << " reused_blocks=" << reused_blocks.size()
           << " lod_rebuilt=" << lod.rebuilt_subtrees
           << " normals_max_deg=" << surface.max_normal_angle_deg
           << " normal_mismatches=" << surface.normal_mismatch_count
           << " normal_max_at=" << surface.max_normal_x << ","
           << surface.max_normal_y
           << " material_max_delta=" << surface.max_material_delta
           << " seam_max_deviation=" << seams.max_deviation
           << " full_incremental_equal=" << (full_incremental_equal ? 1 : 0)
           << " repeated_incremental_equal="
           << (repeated_incremental_equal ? 1 : 0)
           << " fallback_full=" << (invalidation.fallback_full ? 1 : 0)
           << " render={" << render.stableString() << "}";
    if (!status.ok()) {
        stream << " error={" << status.toString() << "}";
    }
    for (const std::string& note : notes) {
        stream << " note={" << note << "}";
    }
    return stream.str();
}

void TerrainScene::applyHeightPatch(HeightField* height,
                                    const HeightPatch& patch) {
    for (std::uint32_t y = 0; y < patch.target.height; ++y) {
        for (std::uint32_t x = 0; x < patch.target.width; ++x) {
            height->at(patch.target.x + x, patch.target.y + y) =
                patch.values[static_cast<std::size_t>(y) * patch.target.width +
                             x];
        }
    }
    height->baseline_version = patch.sequence;
    height->payload_hash = computeHeightHash(*height);
}

void TerrainScene::applyMaterialPatch(MaterialImage* material,
                                      const MaterialPatch& patch) {
    for (std::uint32_t y = 0; y < patch.target.height; ++y) {
        for (std::uint32_t x = 0; x < patch.target.width; ++x) {
            material->at(patch.target.x + x, patch.target.y + y) =
                patch.indices[static_cast<std::size_t>(y) *
                                  patch.target.width +
                              x];
        }
    }
    material->baseline_version = patch.sequence;
    material->payload_hash = computeMaterialHash(*material);
}

void TerrainScene::rebuildMeshes(
    const HeightField& height, const Quadtree& tree, const SurfaceCache& surface,
    const std::set<std::uint64_t>& affected,
    std::map<std::uint64_t, BlockMesh>* meshes) const {
    std::map<std::uint64_t, BlockMesh> next;
    for (std::uint64_t id : tree.leaves()) {
        const QuadNode* node = tree.node(id);
        if (node == nullptr) continue;
        if (affected.find(id) == affected.end()) {
            const auto existing = meshes->find(id);
            if (existing != meshes->end()) {
                next.emplace(id, existing->second);
                continue;
            }
        }
        next.emplace(id, buildBlockMesh(*node, height, surface));
    }
    *meshes = std::move(next);
}

Status TerrainScene::rebuildAll(const HeightField& height,
                                const MaterialImage& material,
                                const SceneOptions& options,
                                bool render_full,
                                const CancelCheck& cancelled) {
    if (isCancelled(cancelled)) return cancelledStatus();
    Status status = validateMaterialDimensions(height, material);
    if (!status.ok()) return status;
    Quadtree tree;
    status = tree.build(height, options.quadtree);
    if (!status.ok()) return status;
    if (isCancelled(cancelled)) return cancelledStatus();
    SurfaceCache surface;
    surface.computeFull(height, material);
    if (isCancelled(cancelled)) return cancelledStatus();
    std::set<std::uint64_t> affected(tree.leaves().begin(),
                                     tree.leaves().end());
    std::map<std::uint64_t, BlockMesh> meshes;
    for (std::uint64_t id : tree.leaves()) {
        const QuadNode* node = tree.node(id);
        meshes.emplace(id, buildBlockMesh(*node, height, surface));
    }
    (void)affected;
    SeamReport seams = verifyMeshSeams(tree, meshes, options.seam_tolerance);
    if (!seams.clean()) {
        return seams.status;
    }
    if (isCancelled(cancelled)) return cancelledStatus();
    SoftwareOffscreenRenderer renderer(options.camera);
    if (render_full) {
        renderer.renderFull(tree, height, surface, meshes,
                            height.baseline_version);
    }
    if (isCancelled(cancelled)) return cancelledStatus();
    height_ = height;
    material_ = material;
    tree_ = std::move(tree);
    surface_ = std::move(surface);
    meshes_ = std::move(meshes);
    renderer_ = std::move(renderer);
    options_ = options;
    return Status::success();
}

Status TerrainScene::initialize(HeightField height, MaterialImage material,
                                const SceneOptions& options) {
    return rebuildAll(height, material, options, true);
}

Status TerrainScene::initializeFromFiles(const std::string& height_path,
                                         const std::string& material_path,
                                         const SceneOptions& options) {
    HeightField height;
    MaterialImage material;
    Status status = loadHeightField(height_path, &height);
    if (!status.ok()) return status;
    status = loadMaterialImage(material_path, &material);
    if (!status.ok()) return status;
    return initialize(std::move(height), std::move(material), options);
}

Status TerrainScene::loadCommitted(VersionStore* store,
                                   const SceneOptions& options) {
    if (store == nullptr) {
        return Status::error(E_INVALID_ARGUMENT, "store",
                             "version store is required");
    }
    HeightField height;
    MaterialImage material;
    VersionManifest manifest;
    Status status = store->loadCurrent(&height, &material, &manifest);
    if (!status.ok()) return status;
    status = store->verifyChain();
    if (!status.ok()) return status;
    return rebuildAll(std::move(height), std::move(material), options, true);
}

Status TerrainScene::applyBatch(const PatchBatch& batch, VersionStore* store,
                                CommitFault fault,
                                const CancelCheck& cancelled,
                                SceneUpdateReport* report_out) {
    SceneUpdateReport report;
    auto finish = [&](Status status) -> Status {
        report.status = status;
        if (report_out != nullptr) *report_out = std::move(report);
        return status;
    };
    report.patch_sequence = batch.sequence;
    report.status = validateBatch(batch, height_, material_);
    if (!report.status.ok()) {
        return finish(report.status);
    }
    if (isCancelled(cancelled)) {
        report.status = cancelledStatus();
        return finish(report.status);
    }

    HeightField work_height = height_;
    MaterialImage work_material = material_;
    if (batch.height) {
        applyHeightPatch(&work_height, *batch.height);
        report.changed_height = batch.height->target;
    }
    if (batch.material) {
        applyMaterialPatch(&work_material, *batch.material);
        report.changed_material = batch.material->target;
    }
    work_height.baseline_version = batch.sequence;
    work_material.baseline_version = batch.sequence;

    Quadtree work_tree = tree_;
    if (batch.height) {
        report.status = work_tree.rebuildIncremental(
            work_height, batch.height->target, true, &report.lod);
        if (!report.status.ok()) {
            return finish(report.status);
        }
    } else {
        report.lod.reasons.push_back(
            "material-only patch; LOD decision nodes reused");
    }
    if (isCancelled(cancelled)) {
        report.status = cancelledStatus();
        return finish(report.status);
    }

    std::vector<std::uint64_t> forced;
    std::set<std::uint64_t> affected;
    if (batch.height) {
        const std::vector<std::uint64_t> direct =
            work_tree.blocksIntersecting(batch.height->target);
        for (std::uint64_t id : direct) {
            appendUnique(&forced, id);
            affected.insert(id);
            for (std::uint64_t neighbor : work_tree.neighbors(id)) {
                appendUnique(&forced, neighbor);
                affected.insert(neighbor);
            }
        }
        for (std::uint64_t id : report.lod.dirty_blocks) {
            if (work_tree.node(id) != nullptr) {
                appendUnique(&forced, id);
                affected.insert(id);
                for (std::uint64_t neighbor : work_tree.neighbors(id)) {
                    appendUnique(&forced, neighbor);
                    affected.insert(neighbor);
                }
            }
        }
        for (std::uint64_t id : report.lod.new_blocks) {
            appendUnique(&forced, id);
            affected.insert(id);
        }
    }
    std::set<std::uint64_t> material_affected;
    Rect material_seed;
    if (batch.material) {
        const Rect halo =
            batch.material->target.expanded(1, work_material.width,
                                            work_material.height);
        material_seed = halo;
        for (std::uint64_t id : work_tree.blocksIntersecting(halo)) {
            affected.insert(id);
            material_affected.insert(id);
        }
    }

    DependencyGraph graph;
    graph.build(work_tree);
    graph.invalidate(work_tree, report.changed_height,
                     batch.material ? &material_seed : nullptr,
                     forced, &report.invalidation);
    for (std::uint64_t id : report.invalidation.invalid_blocks) {
        if (work_tree.node(id) != nullptr) affected.insert(id);
    }
    for (std::uint64_t id : report.invalidation.invalid_blocks) {
        if (work_tree.node(id) == nullptr) continue;
        for (std::uint64_t neighbor : work_tree.neighbors(id)) {
            if (work_tree.node(neighbor) != nullptr) {
                affected.insert(neighbor);
            }
        }
    }
    for (std::uint64_t id : affected) {
        if (work_tree.node(id) == nullptr) continue;
        if (std::find(report.invalidation.invalid_blocks.begin(),
                      report.invalidation.invalid_blocks.end(),
                      id) == report.invalidation.invalid_blocks.end()) {
            report.invalidation.invalid_blocks.push_back(id);
            report.invalidation.block_reasons[id].push_back(
                "conservative surface/geometry rebuild");
        }
    }
    std::sort(report.invalidation.invalid_blocks.begin(),
              report.invalidation.invalid_blocks.end());
    report.invalidation.invalid_blocks.erase(
        std::unique(report.invalidation.invalid_blocks.begin(),
                    report.invalidation.invalid_blocks.end()),
        report.invalidation.invalid_blocks.end());
    {
        const std::vector<std::uint64_t>& current_leaves = work_tree.leaves();
        report.invalidation.invalid_blocks.erase(
            std::remove_if(
                report.invalidation.invalid_blocks.begin(),
                report.invalidation.invalid_blocks.end(),
                [&](std::uint64_t id) {
                    return std::find(current_leaves.begin(),
                                     current_leaves.end(),
                                     id) == current_leaves.end();
                }),
            report.invalidation.invalid_blocks.end());
    }
    if (report.invalidation.invalid_blocks.empty() && batch.material) {
        report.invalidation.invalid_blocks.assign(material_affected.begin(),
                                                  material_affected.end());
        report.invalidation.render_blocks =
            report.invalidation.invalid_blocks;
    }
    report.invalidation.render_blocks = report.invalidation.invalid_blocks;
    report.invalidation.reuse_blocks.clear();
    for (std::uint64_t id : work_tree.leaves()) {
        if (std::find(report.invalidation.invalid_blocks.begin(),
                      report.invalidation.invalid_blocks.end(),
                      id) == report.invalidation.invalid_blocks.end()) {
            report.invalidation.reuse_blocks.push_back(id);
        }
    }

    SurfaceCache work_surface = surface_;
    work_surface.updateIncremental(
        work_height, work_material, work_tree,
        std::vector<std::uint64_t>(affected.begin(), affected.end()),
        batch.height ? &report.changed_height : nullptr,
        batch.material ? &report.changed_material : nullptr,
        batch.height.has_value(),
        &report.surface);
    if (report.surface.max_normal_angle_deg > options_.normal_tolerance_deg) {
        report.status = Status::error(
            E_HEIGHT_INVALID, "normal_incremental",
            "incremental normal differs from full truth beyond tolerance");
        report.notes.push_back("normal tolerance exceeded");
        return finish(report.status);
    }
    if (report.surface.max_material_delta > options_.material_tolerance) {
        report.status = Status::error(
            E_HEIGHT_INVALID, "material_incremental",
            "incremental material blend differs from full truth beyond tolerance");
        report.notes.push_back("material tolerance exceeded");
        return finish(report.status);
    }
    if (isCancelled(cancelled)) {
        report.status = cancelledStatus();
        return finish(report.status);
    }

    std::map<std::uint64_t, BlockMesh> work_meshes = meshes_;
    rebuildMeshes(work_height, work_tree, work_surface, affected,
                  &work_meshes);
    report.seams =
        verifyMeshSeams(work_tree, work_meshes, options_.seam_tolerance);
    if (!report.seams.clean()) {
        report.status = report.seams.status;
        return finish(report.status);
    }
    for (std::uint64_t id : work_tree.leaves()) {
        if (affected.find(id) != affected.end()) {
            report.recomputed_blocks.push_back(id);
        } else {
            report.reused_blocks.push_back(id);
        }
    }

    std::vector<std::uint64_t> render_dirty =
        report.invalidation.invalid_blocks;
    SoftwareOffscreenRenderer work_renderer = renderer_;
    report.render = work_renderer.renderIncremental(
        work_tree, work_height, work_surface, work_meshes, render_dirty,
        static_cast<std::uint32_t>(report.invalidation.invalid_blocks.size()),
        batch.sequence);
    report.incremental_render_hash = report.render.color_hash;

    if (options_.validate_full_render) {
        SoftwareOffscreenRenderer full_renderer(options_.camera);
        const RenderSummary full = full_renderer.renderFull(
            work_tree, work_height, work_surface, work_meshes, batch.sequence);
        report.full_render_hash = full.color_hash;
        report.full_incremental_equal =
            full_renderer.frame().color == work_renderer.frame().color &&
            full_renderer.frame().heat == work_renderer.frame().heat;
        SoftwareOffscreenRenderer repeated = work_renderer;
        const RenderSummary second = repeated.renderIncremental(
            work_tree, work_height, work_surface, work_meshes, {},
            static_cast<std::uint32_t>(
                report.invalidation.invalid_blocks.size()),
            batch.sequence);
        report.repeated_render_hash = second.color_hash;
        report.repeated_incremental_equal =
            repeated.frame().color == work_renderer.frame().color &&
            repeated.frame().heat == work_renderer.frame().heat;
        if (!report.full_incremental_equal) {
            report.status = Status::error(
                E_RENDER_MISMATCH, "render",
                "incremental offscreen frame differs from full frame");
            return finish(report.status);
        }
        if (!report.repeated_incremental_equal) {
            report.status = Status::error(
                E_RENDER_MISMATCH, "render",
                "consecutive incremental frames are not deterministic");
            return finish(report.status);
        }
    }
    if (isCancelled(cancelled)) {
        report.status = cancelledStatus();
        return finish(report.status);
    }

    if (store != nullptr) {
        CommitMetadata metadata;
        metadata.patch_sequence = batch.sequence;
        metadata.patch_hash =
            batch.patch_hash.empty() ? computeBatchHash(batch)
                                     : batch.patch_hash;
        metadata.invalid_blocks = report.invalidation.invalid_blocks;
        metadata.invalidation_reasons = report.invalidation.flat_reasons;
        metadata.fallback_full = report.invalidation.fallback_full;
        report.status = store->commit(work_height, work_material, metadata, fault);
        if (!report.status.ok()) {
            return finish(report.status);
        }
    }

    height_ = std::move(work_height);
    material_ = std::move(work_material);
    tree_ = std::move(work_tree);
    surface_ = std::move(work_surface);
    meshes_ = std::move(work_meshes);
    renderer_ = std::move(work_renderer);
    report.committed_version = height_.baseline_version;
    report.status = Status::success();
    if (report_out != nullptr) *report_out = std::move(report);
    return Status::success();
}

Status TerrainScene::validateFullRender(RenderSummary* summary,
                                        std::string* mismatch_detail) const {
    SoftwareOffscreenRenderer full(options_.camera);
    const RenderSummary result =
        full.renderFull(tree_, height_, surface_, meshes_,
                        height_.baseline_version);
    const bool equal =
        full.frame().color == renderer_.frame().color &&
        full.frame().heat == renderer_.frame().heat;
    if (!equal && mismatch_detail != nullptr) {
        *mismatch_detail = "full hash " + result.color_hash +
                           " != incremental hash " +
                           renderer_.lastSummary().color_hash;
    }
    if (summary != nullptr) *summary = result;
    if (!equal) {
        return Status::error(E_RENDER_MISMATCH, "render",
                             mismatch_detail == nullptr
                                 ? "frame mismatch"
                                 : *mismatch_detail);
    }
    return Status::success();
}

SeamReport TerrainScene::verifySeams() const {
    return verifyMeshSeams(tree_, meshes_, options_.seam_tolerance);
}

void TerrainScene::rebuildOptions(const SceneOptions& options,
                                  Status* status,
                                  const CancelCheck& cancelled) {
    Status local =
        rebuildAll(height_, material_, options, true, cancelled);
    if (status != nullptr) *status = local;
}

}  // namespace terrain

