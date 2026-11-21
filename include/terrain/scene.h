#pragma once

#include <functional>
#include <map>
#include <set>
#include <string>
#include <vector>

#include "terrain/dependency.h"
#include "terrain/mesh.h"
#include "terrain/offscreen.h"
#include "terrain/quadtree.h"
#include "terrain/status.h"
#include "terrain/surface.h"
#include "terrain/types.h"
#include "terrain/version_store.h"

namespace terrain {

using CancelCheck = std::function<bool()>;

struct SceneOptions {
    QuadConfig quadtree;
    Camera camera;
    float seam_tolerance = 1e-4f;
    float normal_tolerance_deg = 0.01f;
    float material_tolerance = 1e-5f;
    bool validate_full_render = true;
};

struct SceneUpdateReport {
    Status status;
    std::uint64_t patch_sequence = 0;
    std::uint64_t committed_version = 0;
    Rect changed_height;
    Rect changed_material;
    InvalidationReport invalidation;
    LodRebuildReport lod;
    SurfaceUpdateReport surface;
    SeamReport seams;
    RenderSummary render;
    std::vector<std::uint64_t> recomputed_blocks;
    std::vector<std::uint64_t> reused_blocks;
    bool full_incremental_equal = false;
    bool repeated_incremental_equal = false;
    std::string full_render_hash;
    std::string incremental_render_hash;
    std::string repeated_render_hash;
    std::vector<std::string> notes;

    std::string stableString() const;
};

class TerrainScene {
public:
    Status initialize(HeightField height, MaterialImage material,
                      const SceneOptions& options = {});
    Status initializeFromFiles(const std::string& height_path,
                               const std::string& material_path,
                               const SceneOptions& options = {});
    Status loadCommitted(VersionStore* store,
                         const SceneOptions& options = {});

    Status applyBatch(const PatchBatch& batch, VersionStore* store,
                      CommitFault fault, const CancelCheck& cancelled,
                      SceneUpdateReport* report);

    Status validateFullRender(RenderSummary* summary,
                              std::string* mismatch_detail) const;
    SeamReport verifySeams() const;

    const HeightField& height() const { return height_; }
    const MaterialImage& material() const { return material_; }
    const Quadtree& quadtree() const { return tree_; }
    const SurfaceCache& surface() const { return surface_; }
    const std::map<std::uint64_t, BlockMesh>& meshes() const {
        return meshes_;
    }
    const SoftwareOffscreenRenderer& renderer() const { return renderer_; }
    const SceneOptions& options() const { return options_; }
    std::uint64_t version() const { return height_.baseline_version; }

    void rebuildOptions(const SceneOptions& options, Status* status,
                        const CancelCheck& cancelled = {});

private:
    Status rebuildAll(const HeightField& height, const MaterialImage& material,
                      const SceneOptions& options, bool render_full,
                      const CancelCheck& cancelled = {});
    static void applyHeightPatch(HeightField* height,
                                 const HeightPatch& patch);
    static void applyMaterialPatch(MaterialImage* material,
                                   const MaterialPatch& patch);
    void rebuildMeshes(const HeightField& height, const Quadtree& tree,
                       const SurfaceCache& surface,
                       const std::set<std::uint64_t>& affected,
                       std::map<std::uint64_t, BlockMesh>* meshes) const;

    HeightField height_;
    MaterialImage material_;
    Quadtree tree_;
    SurfaceCache surface_;
    std::map<std::uint64_t, BlockMesh> meshes_;
    SoftwareOffscreenRenderer renderer_;
    SceneOptions options_;
};

}  // namespace terrain

