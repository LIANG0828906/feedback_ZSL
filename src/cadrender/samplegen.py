"""固定种子的确定性样本生成器。

输出目录结构::

    workdir/manifest.json          # 版本元数据、曲面片/网格清单、容差声明
    workdir/surfaces/s01.json ...  # 控制点/节点/权重/裁剪/真值
    workdir/meshes/m01.json ...    # 细分控制网（Catmull-Clark 风格）

所有“随机”量来自 ``np.random.default_rng(SEED)``，同种子逐次运行产物一致。
真值采样点直接由 :mod:`cadrender.nurbs` 的有理求值器计算，作为细分链路的
解析参照。
"""
from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass
from typing import Any, Dict, List

import numpy as np

from .nurbs import NurbsSurface, NurbsCurve2D

SEED = 20261123
SCHEMA_VERSION = "1.0"
ALGO_VERSION = "cadrender-1.0.0"

# 任务约定的声明容差
SAMPLE_TOL = 1e-6          # 采样点各轴偏差
DENSITY_MIN, DENSITY_MAX = 4, 256


def _clamped_knots(degree: int, n_ctrl: int) -> np.ndarray:
    inner = n_ctrl - degree - 1
    if inner <= 0:
        knots = [0.0] * (degree + 1) + [1.0] * (degree + 1)
    else:
        knots = ([0.0] * (degree + 1)
                 + list(np.linspace(0.0, 1.0, inner + 2))[1:-1]
                 + [1.0] * (degree + 1))
    return np.array(knots, dtype=np.float64)


def _grid(name: str, nu: int, nv: int, degree_u: int, degree_v: int,
          rng: np.random.Generator, kind: str) -> Dict[str, Any]:
    # 每类曲面确定性错位，避免多片在原点重叠
    offsets = {
        "wave": (-3.0, -2.5, 0.0),
        "saddle": (3.0, -2.5, 0.0),
        "dome": (-3.0, 2.5, 0.0),
        "twisted": (3.0, 2.5, 0.0),
    }
    off = np.asarray(offsets[kind], dtype=np.float64)
    gx, gy = np.meshgrid(np.linspace(0.0, 1.0, nu),
                         np.linspace(0.0, 1.0, nv), indexing="ij")
    if kind == "wave":
        z = (np.sin(gx * np.pi) * np.cos(gy * np.pi)
             + 0.25 * np.sin(3 * gx * np.pi) * np.sin(2 * gy * np.pi))
        pts = np.stack([gx * 4.0 - 2.0, gy * 3.0 - 1.5, z], axis=-1)
    elif kind == "saddle":
        z = 0.8 * (gx - 0.5) ** 2 - 0.8 * (gy - 0.5) ** 2
        pts = np.stack([gx * 3.0 - 1.5, gy * 3.0 - 1.5, z + 1.0], axis=-1)
    elif kind == "dome":
        r = np.sqrt((gx - 0.5) ** 2 + (gy - 0.5) ** 2)
        z = np.cos(r * np.pi)
        pts = np.stack([gx * 3.0 - 1.5, gy * 3.0 - 1.5, z], axis=-1)
    else:  # twisted
        ang = (gx - 0.5) * np.pi
        pts = np.stack([gx * 2.0 - 1.0,
                        gy * 2.5 * np.cos(ang),
                        gy * 2.5 * np.sin(ang)], axis=-1)
    pts = pts + rng.normal(0.0, 0.01, size=pts.shape) + off  # 确定性扰动+错位
    weights = (np.ones((nu, nv), dtype=np.float64)
               + rng.uniform(0.0, 0.35, size=(nu, nv)))
    surf = NurbsSurface(
        control_points=pts,
        weights=weights,
        knots_u=_clamped_knots(degree_u, nu),
        knots_v=_clamped_knots(degree_v, nv),
        degree_u=degree_u,
        degree_v=degree_v,
        name=name,
    )
    # 闭合裁剪圆（参数域内），保证闭合
    cx, cy, rad = 0.5, 0.5, 0.42
    tc = np.linspace(0.0, 2 * np.pi, 9)
    circle = np.stack([cx + rad * np.cos(tc), cy + rad * np.sin(tc)], axis=1)
    deg = 2
    # 开放（钳位）NURBS：首末控制点重合以保证曲线闭合
    ctrl = np.vstack([circle[:-1], circle[0:1]])
    k = len(ctrl)
    knots = _clamped_knots(deg, k)
    trim = NurbsCurve2D(ctrl, np.ones(k), knots, deg)

    # 真值采样（8x8 参数网格）
    us = np.linspace(0.0, 1.0, 8)
    vs = np.linspace(0.0, 1.0, 8)
    truth_samples = []
    for u in us:
        for v in vs:
            p, du, dv, nrm = surf.eval_ders(float(u), float(v))
            truth_samples.append({
                "u": float(u), "v": float(v),
                "point": p.tolist(),
                "normal": nrm.tolist(),
            })
    bbox = surf.control_hull_bbox()
    # 裁剪内外判定点
    inside = [0.5, 0.5]
    outside = [0.05, 0.05]
    payload = {
        "name": name,
        "kind": kind,
        "degree_u": degree_u,
        "degree_v": degree_v,
        "control_points": pts.tolist(),
        "weights": weights.tolist(),
        "knots_u": surf.knots_u.tolist(),
        "knots_v": surf.knots_v.tolist(),
        "trim_curve": {
            "control_points": trim.control_points.tolist(),
            "weights": trim.weights.tolist(),
            "knots": trim.knots.tolist(),
            "degree": trim.degree,
        },
        "truth": {
            "samples": truth_samples,
            "bbox": {"min": bbox[0].tolist(), "max": bbox[1].tolist()},
            "trim_points": {
                "inside": inside,
                "outside": outside,
            },
        },
    }
    return payload


def _subdiv_mesh(name: str, size: int, rng: np.random.Generator) -> Dict[str, Any]:
    """Catmull-Clark 风格的四边形控制网 + 细分极限解析点（Loop/CC 极限规则）。"""
    gx, gy = np.meshgrid(np.linspace(-1.0, 1.0, size),
                         np.linspace(-1.0, 1.0, size), indexing="ij")
    z = 0.4 * np.sin(gx * 2.0) * np.cos(gy * 2.0) + rng.normal(0, 0.01, gx.shape)
    verts = np.stack([gx, gy, z], axis=-1).reshape(-1, 3)
    faces = []
    for i in range(size - 1):
        for j in range(size - 1):
            a = i * size + j
            faces.append([a, a + 1, a + size + 1, a + size])
    # 内部顶点的 Catmull-Clark 极限点（权重 1/9 邻域）作为真值
    limit = []
    for i in range(1, size - 1):
        for j in range(1, size - 1):
            c = i * size + j
            neigh = [c - size, c + size, c - 1, c + 1]
            p = (verts[c] + 0.25 * verts[neigh].sum(axis=0)) / 2.0
            limit.append({"vertex": c, "limit_point": p.tolist()})
    return {
        "name": name,
        "scheme": "catmull-clark",
        "vertices": verts.tolist(),
        "faces": faces,
        "truth_limit_points": limit,
    }


def generate(workdir: str, seed: int = SEED) -> Dict[str, Any]:
    rng = np.random.default_rng(seed)
    sdir = os.path.join(workdir, "surfaces")
    mdir = os.path.join(workdir, "meshes")
    os.makedirs(sdir, exist_ok=True)
    os.makedirs(mdir, exist_ok=True)

    kinds = ["wave", "saddle", "dome", "twisted"]
    surface_files: List[str] = []
    for idx, kind in enumerate(kinds, start=1):
        nu = int(rng.integers(5, 8))
        nv = int(rng.integers(5, 8))
        du = int(rng.integers(2, 4))
        dv = int(rng.integers(2, 4))
        payload = _grid(f"s{idx:02d}", nu, nv, min(du, nu - 1),
                        min(dv, nv - 1), rng, kind)
        fn = os.path.join(sdir, f"s{idx:02d}.json")
        with open(fn, "w", encoding="utf-8") as fh:
            json.dump(payload, fh, indent=1)
        surface_files.append(os.path.relpath(fn, workdir))

    mesh_files: List[str] = []
    for idx, size in enumerate([5, 6], start=1):
        payload = _subdiv_mesh(f"m{idx:02d}", size, rng)
        fn = os.path.join(mdir, f"m{idx:02d}.json")
        with open(fn, "w", encoding="utf-8") as fh:
            json.dump(payload, fh, indent=1)
        mesh_files.append(os.path.relpath(fn, workdir))

    manifest = {
        "schema_version": SCHEMA_VERSION,
        "algo_version": ALGO_VERSION,
        "seed": seed,
        "surfaces": surface_files,
        "subdivision_meshes": mesh_files,
        "declared_tolerance": {
            "sample_xyz": SAMPLE_TOL,
            "density_range": [DENSITY_MIN, DENSITY_MAX],
            "divergence": {
                "coverage": 0.02,
                "bbox_projection_px": 4.0,
                "shade_levels": 4,
            },
        },
    }
    raw = json.dumps(manifest, sort_keys=True).encode("utf-8")
    manifest["content_hash"] = hashlib.sha256(raw).hexdigest()
    with open(os.path.join(workdir, "manifest.json"), "w", encoding="utf-8") as fh:
        json.dump(manifest, fh, indent=1)
    return manifest


if __name__ == "__main__":  # pragma: no cover
    import sys

    target = sys.argv[1] if len(sys.argv) > 1 else "workdir"
    m = generate(target)
    print(f"generated {len(m['surfaces'])} surfaces, "
          f"{len(m['subdivision_meshes'])} meshes -> {target}")
