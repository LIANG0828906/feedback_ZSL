"""曲面描述加载与一致性校验。"""
from __future__ import annotations

import json
import os
from dataclasses import dataclass
from typing import Any, Dict, List, Tuple

import numpy as np

from .errors import E_GEOM, E_META, GeomError, MetaError
from .nurbs import NurbsCurve2D, NurbsSurface

REQUIRED_FIELDS = (
    "name", "degree_u", "degree_v", "control_points", "weights",
    "knots_u", "knots_v", "trim_curve", "truth",
)


@dataclass
class SurfacePatch:
    surface: NurbsSurface
    trim: NurbsCurve2D
    truth: Dict[str, Any]
    name: str
    index: int

    def point_in_trim(self, u: float, v: float) -> bool:
        """射线法判定参数点是否在闭合裁剪曲线内部。"""
        poly = self.trim_polyline()
        x, y = u, v
        inside = False
        n = len(poly)
        j = n - 1
        for i in range(n):
            xi, yi = poly[i]
            xj, yj = poly[j]
            if ((yi > y) != (yj > y)) and \
                    (x < (xj - xi) * (y - yi) / ((yj - yi) or 1e-18) + xi):
                inside = not inside
            j = i
        return inside

    def points_in_trim(self, uv: np.ndarray) -> np.ndarray:
        """向量化射线法，uv 形状 (K,2)，返回布尔数组。"""
        poly = self.trim_polyline()
        x = uv[:, 0]
        y = uv[:, 1]
        inside = np.zeros(len(uv), dtype=bool)
        n = len(poly)
        j = n - 1
        for i in range(n):
            xi, yi = poly[i]
            xj, yj = poly[j]
            cond = ((yi > y) != (yj > y))
            xcross = (xj - xi) * (y - yi) / ((yj - yi) or 1e-18) + xi
            inside ^= cond & (x < xcross)
            j = i
        return inside

    def trim_polyline(self, samples: int = 128) -> np.ndarray:
        if not hasattr(self, "_trim_poly"):
            self._trim_poly = self.trim.polyline(samples)
        return self._trim_poly


def _require_fields(payload: Dict[str, Any], source: str) -> None:
    missing = [f for f in REQUIRED_FIELDS if f not in payload]
    if missing:
        raise GeomError(
            f"{source}: 缺少必需字段 {missing}",
            module="loader", field=",".join(missing),
            context={"source": source},
        )


def _check_knots(knots: np.ndarray, degree: int, n_ctrl: int, axis: str,
                 source: str) -> None:
    expected_len = n_ctrl + degree + 1
    if knots.ndim != 1 or knots.shape[0] != expected_len:
        raise GeomError(
            f"{source}: 节点向量 {axis} 长度 {knots.shape[0]} 与控制点数 "
            f"{n_ctrl}、阶数 {degree} 不满足 len=n+p+1={expected_len}",
            module="loader", field=f"knots_{axis}",
            context={"got": int(knots.shape[0]), "expected": expected_len},
        )
    if np.any(np.diff(knots) < -1e-12):
        raise GeomError(
            f"{source}: 节点向量 {axis} 非单调不减",
            module="loader", field=f"knots_{axis}",
        )
    if knots[0] == knots[-1]:
        raise GeomError(
            f"{source}: 节点向量 {axis} 退化（首末相等）",
            module="loader", field=f"knots_{axis}",
        )


def _check_trim_closed(trim: Dict[str, Any], source: str) -> None:
    for f in ("control_points", "weights", "knots", "degree"):
        if f not in trim:
            raise GeomError(f"{source}: 裁剪边界缺少字段 {f}",
                            module="loader", field=f"trim_curve.{f}")
    curve = NurbsCurve2D(
        np.asarray(trim["control_points"], dtype=np.float64),
        np.asarray(trim["weights"], dtype=np.float64),
        np.asarray(trim["knots"], dtype=np.float64),
        int(trim["degree"]),
    )
    if not np.all(np.isfinite(curve.weights)) or np.any(curve.weights <= 0):
        raise GeomError(f"{source}: 裁剪权重必须为正有限值",
                        module="loader", field="trim_curve.weights")
    _check_knots(curve.knots, curve.degree, len(curve.control_points),
                 "trim", source)
    start = curve.point(float(curve.knots[curve.degree]))
    end = curve.point(float(curve.knots[curve.n + 1]))
    if np.linalg.norm(start - end) > 1e-4:
        raise GeomError(
            f"{source}: 裁剪边界未闭合，首尾距离 "
            f"{np.linalg.norm(start - end):.3e}",
            module="loader", field="trim_curve",
            context={"gap": float(np.linalg.norm(start - end))},
        )


def load_patch(path: str, index: int) -> SurfacePatch:
    source = os.path.basename(path)
    try:
        with open(path, "r", encoding="utf-8") as fh:
            payload = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        raise GeomError(f"{source}: 曲面描述损坏，无法解析",
                        module="loader", cause=exc)
    _require_fields(payload, source)

    pts = np.asarray(payload["control_points"], dtype=np.float64)
    weights = np.asarray(payload["weights"], dtype=np.float64)
    if pts.ndim != 3 or pts.shape[2] != 3:
        raise GeomError(f"{source}: 控制点网格必须为 (N,M,3)",
                        module="loader", field="control_points")
    if weights.shape != pts.shape[:2]:
        raise GeomError(f"{source}: 权重形状 {weights.shape} 与控制点 "
                        f"{pts.shape[:2]} 不一致", module="loader",
                        field="weights")
    if not np.all(np.isfinite(weights)) or np.any(weights <= 0):
        raise GeomError(f"{source}: 权重必须为正有限值",
                        module="loader", field="weights")
    if not np.all(np.isfinite(pts)):
        raise GeomError(f"{source}: 控制点含非有限值",
                        module="loader", field="control_points")

    du, dv = int(payload["degree_u"]), int(payload["degree_v"])
    knots_u = np.asarray(payload["knots_u"], dtype=np.float64)
    knots_v = np.asarray(payload["knots_v"], dtype=np.float64)
    _check_knots(knots_u, du, pts.shape[0], "u", source)
    _check_knots(knots_v, dv, pts.shape[1], "v", source)

    _check_trim_closed(payload["trim_curve"], source)
    trim = NurbsCurve2D(
        np.asarray(payload["trim_curve"]["control_points"], dtype=np.float64),
        np.asarray(payload["trim_curve"]["weights"], dtype=np.float64),
        np.asarray(payload["trim_curve"]["knots"], dtype=np.float64),
        int(payload["trim_curve"]["degree"]),
    )
    surface = NurbsSurface(pts, weights, knots_u, knots_v, du, dv,
                           name=payload["name"])
    return SurfacePatch(surface, trim, payload["truth"], payload["name"], index)


def load_scene(workdir: str) -> Tuple[List[SurfacePatch], List[Dict[str, Any]],
                                      Dict[str, Any]]:
    """全量加载多曲面片集合，并校验真值清单数量一致性。"""
    with open(os.path.join(workdir, "manifest.json"), "r", encoding="utf-8") as fh:
        manifest = json.load(fh)
    patches: List[SurfacePatch] = []
    for idx, rel in enumerate(manifest["surfaces"]):
        patches.append(load_patch(os.path.join(workdir, rel), idx))

    # E_META：曲面片数与真值清单不一致
    declared = len(manifest["surfaces"])
    if len(patches) != declared:
        raise MetaError(
            f"加载曲面片数 {len(patches)} 与清单声明 {declared} 不一致",
            module="loader", context={"loaded": len(patches),
                                      "declared": declared},
        )
    for patch in patches:
        truth = patch.truth
        if "samples" not in truth or "bbox" not in truth:
            raise MetaError(
                f"曲面片 #{patch.index} ({patch.name}) 真值清单缺失 "
                f"samples/bbox", module="loader",
                context={"patch_index": patch.index, "patch": patch.name},
            )

    meshes: List[Dict[str, Any]] = []
    for rel in manifest.get("subdivision_meshes", []):
        with open(os.path.join(workdir, rel), "r", encoding="utf-8") as fh:
            meshes.append(json.load(fh))
    return patches, meshes, manifest


def verify_truth_samples(patch: SurfacePatch, tol: float) -> float:
    """重算采样点并与真值比对，返回各轴最大绝对偏差。"""
    worst = 0.0
    for rec in patch.truth["samples"]:
        p = patch.surface.point(rec["u"], rec["v"])
        ref = np.asarray(rec["point"], dtype=np.float64)
        worst = max(worst, float(np.max(np.abs(p - ref))))
    return worst
