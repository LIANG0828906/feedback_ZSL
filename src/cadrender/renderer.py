"""确定性软件光栅渲染器与预览/精确双路径。

为满足“并行与串行逐像素一致”的强约束，渲染核心采用纯 numpy 软件实现，
所有中间量按固定 float32/float64 顺序计算、无原子写竞争。OpenGL FBO
仅作为可选上下文/加速探测，驱动不支持时在错误链路记录 E_MEM 并回退。
"""
from __future__ import annotations
import math
from dataclasses import dataclass, field
from typing import Dict, List, Optional, Tuple

import numpy as np

from .camera import Camera, normalize
from .errors import E_MEM, E_PARA, MemError, ParaError
from .tessellation import TriangleMesh
from .versioning import Material


# 每线程像素/网格缓冲预算（元素数），超出即按 E_MEM 链路排队
DEFAULT_PIXEL_BUDGET = 2_000_000
DEFAULT_GRID_BUDGET = 1_000_000


@dataclass
class RenderParams:
    mode: str                 # "preview" | "precise"
    lod: int                  # 细分密度等级
    shade_model: str          # "simple" | "full"
    version: int


@dataclass
class RenderedFrame:
    color: np.ndarray         # (H,W,3) float32 [0,1]
    depth: np.ndarray         # (H,W) float32
    camera_sig: tuple
    params: RenderParams
    content_hash: str
    tile_grid: Tuple[int, int]
    tile_size: int
    coverage: float = 0.0
    bbox_projection: Optional[Tuple[int, int, int, int]] = None
    shade_levels: int = 0

    def silhouette_mask(self) -> np.ndarray:
        return self.depth < np.inf


class SoftwareRasterizer:
    def __init__(self, camera: Camera, light_dir: Optional[np.ndarray] = None,
                 pixel_budget: int = DEFAULT_PIXEL_BUDGET,
                 grid_budget: int = DEFAULT_GRID_BUDGET) -> None:
        self.camera = camera
        self.light_dir = normalize(np.asarray(
            light_dir if light_dir is not None else (0.4, 0.7, 0.9)))
        self.pixel_budget = pixel_budget
        self.grid_budget = grid_budget
        self.mem_events: List[Dict] = []

    def _check_budget(self, w: int, h: int, n_vert: int) -> None:
        need_px = w * h * 4
        need_grid = n_vert * 3
        if need_px > self.pixel_budget:
            self.mem_events.append({"code": E_MEM,
                                    "reason": "pixel_buffer_budget",
                                    "need": need_px,
                                    "budget": self.pixel_budget})
            raise MemError("像素缓冲超出预算", module="renderer",
                           field="pixel_buffer",
                           context={"need": need_px,
                                    "budget": self.pixel_budget})
        if need_grid > self.grid_budget:
            self.mem_events.append({"code": E_MEM,
                                    "reason": "grid_buffer_budget",
                                    "need": need_grid,
                                    "budget": self.grid_budget})
            raise MemError("网格缓冲超出预算", module="renderer",
                           field="grid_buffer",
                           context={"need": need_grid,
                                    "budget": self.grid_budget})

    def _transform(self, mesh: TriangleMesh
                   ) -> Tuple[np.ndarray, np.ndarray, np.ndarray]:
        mvp = self.camera.view_proj()
        n = len(mesh.vertices)
        homo = np.concatenate([mesh.vertices, np.ones((n, 1))], axis=1)
        clip = homo @ mvp.T
        w_clip = clip[:, 3:4].copy()
        w_clip[np.abs(w_clip) < 1e-12] = 1e-12
        ndc = clip[:, :3] / w_clip
        view = homo @ self.camera.view_matrix().T
        return ndc.astype(np.float64), view[:, :3], w_clip.ravel()

    def _shade(self, mesh: TriangleMesh, world: np.ndarray,
               material: Material, simple: bool) -> np.ndarray:
        nrm = mesh.normals
        if simple:
            # 仅环境光 + 漫反射
            diff = np.clip(nrm @ self.light_dir, 0.0, 1.0)
            intensity = material.ambient + material.diffuse * diff
            color = np.clip(intensity[:, None]
                            * np.asarray(material.base_color), 0, 1)
            return color.astype(np.float32)
        # 完整光照：环境 + 漫反射 + Blinn-Phong 高光
        diff = np.clip(nrm @ self.light_dir, 0.0, 1.0)
        view_dir = normalize(self.camera.eye - world)
        half = normalize(self.light_dir + view_dir)
        spec = np.clip(np.einsum("ij,ij->i", nrm, half), 0, 1)
        spec = np.power(spec, material.shininess) * material.specular
        intensity = material.ambient + material.diffuse * diff
        color = (intensity[:, None] * np.asarray(material.base_color)
                 + spec[:, None])
        return np.clip(color, 0, 1).astype(np.float32)

    def render_tile(self, meshes: List[Tuple[TriangleMesh, Material]],
                    x0: int, y0: int, w: int, h: int,
                    mode: str, version: int,
                    ) -> Tuple[np.ndarray, np.ndarray]:
        """渲染单个屏幕分块，返回该块 color/depth（确定性）。"""
        simple = mode == "preview"
        total_vert = sum(len(m.vertices) for m, _ in meshes)
        self._check_budget(w, h, total_vert)
        color = np.zeros((h, w, 3), dtype=np.float32)
        depth = np.full((h, w), np.inf, dtype=np.float32)
        W, H = self.camera.width, self.camera.height

        for mesh, material in meshes:
            ndc, view, _ = self._transform(mesh)
            screen = np.empty_like(ndc[:, :2])
            screen[:, 0] = (ndc[:, 0] * 0.5 + 0.5) * W
            screen[:, 1] = (1.0 - (ndc[:, 1] * 0.5 + 0.5)) * H
            vert_color = self._shade(mesh, mesh.vertices, material, simple)
            z_ndc = ndc[:, 2]
            for tri in mesh.triangles:
                self._raster_tri(tri, screen, z_ndc, vert_color,
                                 color, depth, x0, y0, w, h)
        return color, depth

    @staticmethod
    def _raster_tri(tri: np.ndarray, screen: np.ndarray, z_ndc: np.ndarray,
                    vert_color: np.ndarray, color: np.ndarray,
                    depth: np.ndarray, x0: int, y0: int,
                    w: int, h: int) -> None:
        p = screen[tri]
        z = z_ndc[tri]
        c = vert_color[tri]
        minx = max(x0, int(math.floor(p[:, 0].min())))
        maxx = min(x0 + w - 1, int(math.ceil(p[:, 0].max())))
        miny = max(y0, int(math.floor(p[:, 1].min())))
        maxy = min(y0 + h - 1, int(math.ceil(p[:, 1].max())))
        if minx > maxx or miny > maxy:
            return
        denom = ((p[1, 1] - p[2, 1]) * (p[0, 0] - p[2, 0])
                 + (p[2, 0] - p[1, 0]) * (p[0, 1] - p[2, 1]))
        if abs(denom) < 1e-12:
            return
        ys, xs = np.mgrid[miny:maxy + 1, minx:maxx + 1]
        px = xs + 0.5
        py = ys + 0.5
        w0 = ((p[1, 1] - p[2, 1]) * (px - p[2, 0])
              + (p[2, 0] - p[1, 0]) * (py - p[2, 1])) / denom
        w1 = ((p[2, 1] - p[0, 1]) * (px - p[2, 0])
              + (p[0, 0] - p[2, 0]) * (py - p[2, 1])) / denom
        w2 = 1.0 - w0 - w1
        inside = (w0 >= 0) & (w1 >= 0) & (w2 >= 0)
        if not inside.any():
            return
        interp_z = w0 * z[0] + w1 * z[1] + w2 * z[2]
        interp_c = (w0[..., None] * c[0] + w1[..., None] * c[1]
                    + w2[..., None] * c[2])
        ly = ys - y0
        lx = xs - x0
        closer = inside & (interp_z < depth[ly, lx])
        depth[ly, lx] = np.where(closer, interp_z.astype(np.float32),
                                 depth[ly, lx])
        for ch in range(3):
            color[ly, lx, ch] = np.where(
                closer, interp_c[..., ch].astype(np.float32),
                color[ly, lx, ch])

    def render_full(self, meshes: List[Tuple[TriangleMesh, Material]],
                    mode: str, version: int, tile_size: int = 32,
                    ) -> RenderedFrame:
        W, H = self.camera.width, self.camera.height
        gx = math.ceil(W / tile_size)
        gy = math.ceil(H / tile_size)
        color = np.zeros((H, W, 3), dtype=np.float32)
        depth = np.full((H, W), np.inf, dtype=np.float32)
        for ty in range(gy):
            for tx in range(gx):
                x0, y0 = tx * tile_size, ty * tile_size
                tw = min(tile_size, W - x0)
                th = min(tile_size, H - y0)
                tc, td = self.render_tile(meshes, x0, y0, tw, th,
                                          mode, version)
                color[y0:y0 + th, x0:x0 + tw] = tc
                depth[y0:y0 + th, x0:x0 + tw] = td
        return self._finalize(color, depth, mode, version, (gx, gy),
                              tile_size)

    def _finalize(self, color: np.ndarray, depth: np.ndarray, mode: str,
                  version: int, tile_grid: Tuple[int, int],
                  tile_size: int) -> RenderedFrame:
        mask = depth < np.inf
        coverage = float(mask.mean())
        ys, xs = np.where(mask)
        bbox = None
        if len(xs):
            bbox = (int(xs.min()), int(ys.min()), int(xs.max()),
                    int(ys.max()))
        lit = color[mask]
        if len(lit):
            lum = (lit @ np.array([0.299, 0.587, 0.114]))
            shade_levels = int(np.unique(np.round(lum * 16).astype(int)
                                         ).size)
        else:
            shade_levels = 0
        lod = 0
        rp = RenderParams(mode=mode, lod=lod,
                          shade_model="simple" if mode == "preview"
                          else "full",
                          version=version)
        return RenderedFrame(color, depth, self.camera.frame_signature(),
                             rp, "", tile_grid, tile_size, coverage,
                             bbox, shade_levels)
