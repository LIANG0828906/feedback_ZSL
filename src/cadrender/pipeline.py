"""应用核心编排：加载 -> 细分 -> 版本快照 -> 预览/精确 -> 比对 -> 剖面。"""
from __future__ import annotations

import json
import os
import threading
from dataclasses import dataclass
from typing import Any, Dict, List, Optional, Tuple

import numpy as np

from .camera import Camera
from .checkpoint import CheckpointStore
from .compare import DivergenceReport, compare_frames, diff_heatmap
from .errors import E_DIVERGE, DivergeError
from .glcontext import GlCapability, probe_opengl
from .loader import SurfacePatch, load_scene, verify_truth_samples
from .renderer import RenderedFrame, SoftwareRasterizer
from .scheduler import ScheduleResult, TileScheduler
from .section import Plane, SectionResult, extract_section
from .tessellation import TriangleMesh, tessellate
from .versioning import Material, SceneSnapshot, VersionStore, stable_hash


def scene_content_from_patches(patches: List[SurfacePatch]) -> Dict[str, Any]:
    content: Dict[str, Any] = {}
    for p in patches:
        s = p.surface
        content[p.name] = {
            "control_points": s.control_points.tolist(),
            "weights": s.weights.tolist(),
            "knots_u": s.knots_u.tolist(),
            "knots_v": s.knots_v.tolist(),
            "degree_u": s.degree_u,
            "degree_v": s.degree_v,
        }
    return content


@dataclass
class PipelineConfig:
    workdir: str
    checkpoint_dir: str
    export_dir: str
    width: int = 256
    height: int = 256
    tile_size: int = 32
    max_workers: int = 2
    preview_density: int = 16
    precise_density: int = 48
    max_inflight_pixels: int = 200_000


class RenderPipeline:
    def __init__(self, cfg: PipelineConfig) -> None:
        self.cfg = cfg
        os.makedirs(cfg.export_dir, exist_ok=True)
        self.patches, self.meshes, self.manifest = load_scene(cfg.workdir)
        self.versions = VersionStore()
        self.store = CheckpointStore(cfg.checkpoint_dir)
        self.gl: Optional[GlCapability] = None
        self._build_camera()
        content = scene_content_from_patches(self.patches)
        self.material = Material()
        self.snapshot: SceneSnapshot = self.versions.initial(content,
                                                              self.material)
        self.rast = SoftwareRasterizer(
            self.camera,
            pixel_budget=cfg.tile_size * cfg.tile_size * 4 * 4,
            grid_budget=2_000_000)
        self.scheduler = TileScheduler(
            self.rast, self.store, self.versions,
            max_workers=cfg.max_workers, tile_size=cfg.tile_size,
            max_inflight_pixels=cfg.max_inflight_pixels)
        self._mesh_cache: Dict[Tuple[int, int, str], TriangleMesh] = {}
        self.last_preview: Optional[RenderedFrame] = None
        self.last_precise: Optional[RenderedFrame] = None

    def _build_camera(self) -> None:
        all_pts = np.concatenate([p.surface.control_points.reshape(-1, 3)
                                  for p in self.patches], axis=0)
        center = all_pts.mean(axis=0)
        extent = np.linalg.norm(all_pts.max(0) - all_pts.min(0))
        eye = center + np.array([0.0, -0.35, 1.0]) * extent * 1.1
        self.camera = Camera(eye=eye, target=center, up=(0, 0, 1),
                             fov_y_deg=45.0, near=0.1,
                             far=extent * 8.0,
                             width=self.cfg.width, height=self.cfg.height)

    # ---- 几何 / 细分 ----
    def verify_geometry(self, tol: Optional[float] = None) -> Dict[str, float]:
        tol = tol or self.manifest["declared_tolerance"]["sample_xyz"]
        result = {}
        for p in self.patches:
            result[p.name] = verify_truth_samples(p, tol)
        return result

    def tessellate_all(self, density: int, adaptive: bool = True
                       ) -> List[Tuple[TriangleMesh, Material]]:
        key = (density, int(adaptive), self.snapshot.version)
        if key not in self._mesh_cache:
            meshes = []
            for p in self.patches:
                meshes.append(tessellate(p, density, adaptive=adaptive))
            self._mesh_cache[key] = meshes  # type: ignore[assignment]
        return [(m, self.snapshot.material)
                for m in self._mesh_cache[key]]  # type: ignore[index]

    # ---- 版本编辑 ----
    def edit_control_point(self, patch_name: str, i: int, j: int,
                           delta: np.ndarray, note: str = "cp_edit"
                           ) -> SceneSnapshot:
        for p in self.patches:
            if p.name == patch_name:
                p.surface.control_points[i, j] += np.asarray(delta)
                break
        else:
            raise KeyError(patch_name)
        content = scene_content_from_patches(self.patches)
        self._mesh_cache.clear()
        self.snapshot = self.versions.commit(content, self.material, note)
        return self.snapshot

    def edit_material(self, **kwargs) -> SceneSnapshot:
        new_mat = Material(**{**self.material.__dict__, **kwargs})
        self.material = new_mat
        content = self.snapshot.scene_content
        self.snapshot = self.versions.commit(content, new_mat,
                                             "material_edit")
        return self.snapshot

    # ---- 双路径渲染 ----
    def render_preview(self) -> RenderedFrame:
        meshes = self.tessellate_all(self.cfg.preview_density)
        frame = self.rast.render_full(meshes, "preview",
                                      self.snapshot.version,
                                      tile_size=self.cfg.tile_size)
        frame.content_hash = self.snapshot.content_hash
        frame.params.lod = self.cfg.preview_density
        self.last_preview = frame
        return frame

    def render_precise(self, parallel: bool = True,
                       cancel_event: Optional[threading.Event] = None
                       ) -> Tuple[RenderedFrame, ScheduleResult]:
        version = self.snapshot.version
        content_hash = self.snapshot.content_hash
        self.versions.lock_for_precise(version)
        meshes = self.tessellate_all(self.cfg.precise_density)
        result = self.scheduler.run(meshes, "precise", version, content_hash,
                                    parallel=parallel,
                                    cancel_event=cancel_event)
        frame = self.scheduler.assemble_frame(version, content_hash,
                                              "precise")
        frame.params.lod = self.cfg.precise_density
        self.last_precise = frame
        return frame, result

    def compare(self) -> DivergenceReport:
        if self.last_preview is None or self.last_precise is None:
            raise DivergeError("缺少预览或精确帧，拒绝比对",
                               module="pipeline")
        tol = self.manifest["declared_tolerance"]["divergence"]
        return compare_frames(self.last_preview, self.last_precise, tol)

    def heatmap(self) -> np.ndarray:
        assert self.last_preview and self.last_precise
        return diff_heatmap(self.last_preview, self.last_precise)

    # ---- 剖面 ----
    def section(self, plane: Plane) -> SectionResult:
        meshes = self.tessellate_all(self.cfg.precise_density)
        merged = self._merge_meshes([m for m, _ in meshes])
        frozen = self.snapshot.version in self.store.list_versions()
        res = extract_section(merged, plane, self.snapshot.version, frozen)
        if res.points:
            res.annotate_endpoints()
        return res

    @staticmethod
    def _merge_meshes(meshes: List[TriangleMesh]) -> TriangleMesh:
        verts = np.concatenate([m.vertices for m in meshes])
        normals = np.concatenate([m.normals for m in meshes])
        params = np.concatenate([m.params for m in meshes])
        curv = np.concatenate([m.curvature for m in meshes])
        offsets = np.cumsum([0] + [len(m.vertices) for m in meshes[:-1]])
        tris = np.concatenate([m.triangles + off
                               for m, off in zip(meshes, offsets)])
        return TriangleMesh(verts, normals, tris, params, curv,
                            meshes[0].density, -1)

    # ---- GL 探测 ----
    def probe_gl(self) -> GlCapability:
        self.gl = probe_opengl(self.cfg.width, self.cfg.height)
        return self.gl

    # ---- 导出 ----
    def export_frame_png(self, frame: RenderedFrame, name: str) -> str:
        from PIL import Image
        img = (np.clip(frame.color, 0, 1) * 255).astype(np.uint8)
        path = os.path.join(self.cfg.export_dir, name)
        Image.fromarray(img).save(path)
        return path

    def export_heatmap_png(self, name: str) -> str:
        from PIL import Image
        hm = self.heatmap()
        norm = (hm / max(hm.max(), 1e-9) * 255).astype(np.uint8)
        path = os.path.join(self.cfg.export_dir, name)
        Image.fromarray(norm).save(path)
        return path

    def export_report(self, payload: Dict[str, Any], name: str) -> str:
        path = os.path.join(self.cfg.export_dir, name)
        with open(path, "w", encoding="utf-8") as fh:
            json.dump(payload, fh, indent=1, default=str, ensure_ascii=False)
        return path
