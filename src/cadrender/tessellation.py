"""参数曲面细分与确定性法线估计。"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Optional

import numpy as np

from .errors import E_PARA, ParaError
from .loader import SurfacePatch
from .samplegen import DENSITY_MAX, DENSITY_MIN


@dataclass
class TriangleMesh:
    vertices: np.ndarray        # (V,3)
    normals: np.ndarray         # (V,3)
    triangles: np.ndarray       # (T,3) int32
    params: np.ndarray          # (V,2) 参数域 (u,v)
    curvature: np.ndarray       # (V,) 曲率近似
    density: int
    patch_index: int

    def __post_init__(self) -> None:
        self.triangles = np.asarray(self.triangles, dtype=np.int32)


def validate_density(density: int) -> int:
    if not isinstance(density, (int, np.integer)) or isinstance(density, bool):
        raise ParaError("细分密度必须为整数", module="tessellation",
                        field="density",
                        context={"got": repr(density)})
    if density < DENSITY_MIN or density > DENSITY_MAX:
        raise ParaError(
            f"细分密度 {density} 越界，允许区间 "
            f"[{DENSITY_MIN},{DENSITY_MAX}]",
            module="tessellation", field="density",
            context={"got": int(density), "range": [DENSITY_MIN, DENSITY_MAX]},
        )
    return int(density)


def _resolve_density(base: int, curvature_scale: float,
                     adaptive: bool) -> int:
    """曲率联动：曲率越大，采样越密；收紧（提高）时密度单调不减。"""
    if not adaptive:
        return base
    bump = int(np.ceil(base * float(np.clip(curvature_scale, 0.0, 3.0))
                       * 0.25))
    return min(DENSITY_MAX, base + bump)


def tessellate(patch: SurfacePatch, density: int, *,
               adaptive: bool = True,
               apply_trim: bool = True) -> TriangleMesh:
    """在参数域规则采样并三角化。

    - 顶点位置与法线完全由 NURBS 有理偏导确定，无随机量；
    - ``adaptive`` 时根据曲率收紧密度，密度随曲率单调不减；
    - ``apply_trim`` 时剔除裁剪边界外的三角形。
    """
    density = validate_density(density)
    surf = patch.surface

    # 先用粗采样估计整曲面曲率尺度，再决定最终密度
    probe = 12
    pu = np.linspace(0.0, 1.0, probe)
    pv = np.linspace(0.0, 1.0, probe)
    curv_acc = []
    for u in pu:
        for v in pv:
            _, du, dv, _ = surf.eval_ders(float(u), float(v))
            cross = np.linalg.norm(np.cross(du, dv))
            area = max(cross, 1e-12)
            curv_acc.append(np.linalg.norm(du) * np.linalg.norm(dv) / area)
    curvature_scale = float(np.mean(curv_acc))
    final_density = _resolve_density(density, curvature_scale, adaptive)

    us = np.linspace(0.0, 1.0, final_density)
    vs = np.linspace(0.0, 1.0, final_density)
    nu = nv = final_density

    vertices = np.zeros((nu * nv, 3), dtype=np.float64)
    normals = np.zeros((nu * nv, 3), dtype=np.float64)
    params = np.zeros((nu * nv, 2), dtype=np.float64)
    curvature = np.zeros(nu * nv, dtype=np.float64)
    idx = 0
    for i, u in enumerate(us):
        for j, v in enumerate(vs):
            p, du, dv, nrm = surf.eval_ders(float(u), float(v))
            vertices[idx] = p
            normals[idx] = nrm
            params[idx] = (u, v)
            cross = np.linalg.norm(np.cross(du, dv))
            curvature[idx] = (np.linalg.norm(du) * np.linalg.norm(dv)
                              / max(cross, 1e-12))
            idx += 1

    def vid(i: int, j: int) -> int:
        return i * nv + j

    raw_tri = []
    for i in range(nu - 1):
        for j in range(nv - 1):
            a, b = vid(i, j), vid(i, j + 1)
            c, d = vid(i + 1, j + 1), vid(i + 1, j)
            raw_tri.append((a, b, c))
            raw_tri.append((a, c, d))
    triangles = np.asarray(raw_tri, dtype=np.int32)

    if apply_trim:
        centroids = params[triangles].mean(axis=1)
        keep = patch.points_in_trim(centroids)
        triangles = triangles[keep]
        used = np.unique(triangles)
        remap = -np.ones(len(vertices), dtype=np.int32)
        remap[used] = np.arange(len(used))
        vertices = vertices[used]
        normals = normals[used]
        params = params[used]
        curvature = curvature[used]
        triangles = remap[triangles]

    # 由相邻面平均得到逐顶点法线（解析法线已在顶点处，面平均用于平滑）
    smooth = np.zeros_like(normals)
    for tri in triangles:
        face_n = np.cross(vertices[tri[1]] - vertices[tri[0]],
                          vertices[tri[2]] - vertices[tri[0]])
        ln = np.linalg.norm(face_n)
        if ln > 1e-15:
            face_n = face_n / ln
        for k in tri:
            smooth[k] += face_n
    lens = np.linalg.norm(smooth, axis=1, keepdims=True)
    lens[lens < 1e-15] = 1.0
    smooth = smooth / lens
    # 与解析法线保持一致（解析法线为主，几何平均仅作平滑修正方向）
    flip = np.einsum("ij,ij->i", smooth, normals) < 0
    smooth[flip] = -smooth[flip]
    normals = 0.5 * (normals + smooth)
    normals /= np.linalg.norm(normals, axis=1, keepdims=True)

    return TriangleMesh(vertices, normals, triangles, params, curvature,
                        final_density, patch.index)


def mesh_signature(mesh: TriangleMesh) -> bytes:
    """逐顶点一致性签名（用于重复细分确定性校验）。"""
    import hashlib
    h = hashlib.sha256()
    for arr in (mesh.vertices, mesh.normals, mesh.triangles, mesh.params):
        h.update(np.ascontiguousarray(arr).tobytes())
    return h.digest()
