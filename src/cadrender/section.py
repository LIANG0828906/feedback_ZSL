"""剖面平面与曲面交线提取、双端点标注。"""
from __future__ import annotations

from dataclasses import dataclass
from typing import List, Optional, Tuple

import numpy as np

from .tessellation import TriangleMesh


@dataclass(frozen=True)
class Plane:
    point: np.ndarray     # 平面上一点
    normal: np.ndarray    # 平面法线

    def signed(self, p: np.ndarray) -> float:
        return float((p - self.point) @ self.normal)


@dataclass
class SectionPoint:
    world: np.ndarray
    params: np.ndarray    # (u,v)


@dataclass
class SectionResult:
    points: List[SectionPoint]
    version: int
    frozen: bool
    annotation: Optional[dict] = None

    def annotate_endpoints(self) -> dict:
        if len(self.points) < 2:
            raise ValueError("交线点数不足，无法放置双端点标注")
        a, b = self.points[0], self.points[-1]
        self.annotation = {
            "start": {"world": a.world.tolist(),
                      "params": a.params.tolist()},
            "end": {"world": b.world.tolist(),
                    "params": b.params.tolist()},
            "world_distance": float(np.linalg.norm(b.world - a.world)),
            "param_distance": float(np.linalg.norm(b.params - a.params)),
            "version": self.version,
            "frozen": self.frozen,
        }
        return self.annotation


def extract_section(mesh: TriangleMesh, plane: Plane, version: int,
                    frozen: bool) -> SectionResult:
    """对每条三角边与剖面平面求交，插值得到世界坐标与参数域坐标。"""
    verts = mesh.vertices
    params = mesh.params
    edges = set()
    for tri in mesh.triangles:
        for a, b in ((tri[0], tri[1]), (tri[1], tri[2]),
                     (tri[2], tri[0])):
            edges.add((min(int(a), int(b)), max(int(a), int(b))))

    out: List[SectionPoint] = []
    for a, b in sorted(edges):
        d0, d1 = plane.signed(verts[a]), plane.signed(verts[b])
        if d0 == 0.0 and d1 == 0.0:
            continue
        if (d0 > 0) == (d1 > 0):
            continue
        denom = d0 - d1
        t = d0 / denom if denom != 0 else 0.0
        world = verts[a] * (1 - t) + verts[b] * t
        uv = params[a] * (1 - t) + params[b] * t
        out.append(SectionPoint(world=world, params=uv))

    # 按空间邻近度确定性排序，形成连续折线
    ordered: List[SectionPoint] = []
    remaining = list(out)
    if remaining:
        ordered.append(remaining.pop(0))
        while remaining:
            cur = ordered[-1].world
            dists = [np.linalg.norm(p.world - cur) for p in remaining]
            k = int(np.argmin(dists))
            ordered.append(remaining.pop(k))
    return SectionResult(points=ordered, version=version, frozen=frozen)
