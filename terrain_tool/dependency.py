"""依赖图谱与增量失效判定。

链路节点（每个 chunk 各一份）::

    HEIGHT -> NORMALS -> RENDER
    HEIGHT -> LOD -> SEAM -> RENDER
    MATERIAL -> BLEND -> RENDER

补丁合并后从变更矩形出发，先得到直接受影响 chunk，再沿依赖边扩展闭包：

* 法线依赖相邻一圈高程（中心差分跨块），故 NORMALS 失效带 1 chunk 光环；
* LOD 变化会让相邻块的接缝失效，故 SEAM 失效带 1 chunk 光环；
* RENDER 汇总所有上游失效，采用保守策略——无法证明未受影响即重算，
  并为每个重算块记录原因，复用块记录其哈希证据，绝不静默复用陈旧节点。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum


class NodeKind(str, Enum):
    HEIGHT = "height"
    MATERIAL = "material"
    NORMALS = "normals"
    LOD = "lod"
    SEAM = "seam"
    BLEND = "blend"
    RENDER = "render"


# 依赖边：上游种类 -> 下游种类及光环半径（chunk 单位）
DEPENDENCY_EDGES: dict[NodeKind, tuple[tuple[NodeKind, int], ...]] = {
    NodeKind.HEIGHT: ((NodeKind.NORMALS, 1), (NodeKind.LOD, 0)),
    NodeKind.MATERIAL: ((NodeKind.BLEND, 1),),
    NodeKind.NORMALS: ((NodeKind.RENDER, 0),),
    NodeKind.LOD: ((NodeKind.SEAM, 1),),
    NodeKind.SEAM: ((NodeKind.RENDER, 0),),
    NodeKind.BLEND: ((NodeKind.RENDER, 0),),
    NodeKind.RENDER: (),
}

# 失效原因文案
REASON = {
    NodeKind.HEIGHT: "补丁直接覆盖该块高程",
    NodeKind.MATERIAL: "补丁直接覆盖该块材质",
    NodeKind.NORMALS: "中心差分依赖相邻块高程，保守重算",
    NodeKind.LOD: "高程起伏变化，重决策目标 LOD 层级",
    NodeKind.SEAM: "相邻块 LOD 变化，接缝需重新闭合",
    NodeKind.BLEND: "双线性混合依赖相邻块材质，保守重算",
    NodeKind.RENDER: "上游产物失效，离屏缓冲重渲",
}


@dataclass
class InvalidationResult:
    # kind -> set[(cx, cy)]
    invalid: dict[NodeKind, set[tuple[int, int]]] = field(default_factory=dict)
    # (kind, cx, cy) -> 原因
    reasons: dict[tuple[NodeKind, int, int], str] = field(default_factory=dict)
    height_chunks: set[tuple[int, int]] = field(default_factory=set)
    material_chunks: set[tuple[int, int]] = field(default_factory=set)

    def is_invalid(self, kind: NodeKind, cx: int, cy: int) -> bool:
        return (cx, cy) in self.invalid.get(kind, set())

    def render_chunks(self) -> set[tuple[int, int]]:
        return set(self.invalid.get(NodeKind.RENDER, set()))


def chunks_overlapping_rect(
    rect: tuple[int, int, int, int], chunk_size: int, height: int, width: int
) -> set[tuple[int, int]]:
    """返回与像素矩形 (x, y, w, h) 相交的全部 chunk 坐标。"""
    x, y, w, h = rect
    cx0 = x // chunk_size
    cy0 = y // chunk_size
    cx1 = (x + w - 1) // chunk_size
    cy1 = (y + h - 1) // chunk_size
    grid_cols = (width + chunk_size - 1) // chunk_size
    grid_rows = (height + chunk_size - 1) // chunk_size
    return {
        (cx, cy)
        for cy in range(max(0, cy0), min(grid_rows, cy1 + 1))
        for cx in range(max(0, cx0), min(grid_cols, cx1 + 1))
    }


def _expand_halo(
    chunks: set[tuple[int, int]], radius: int, grid_cols: int, grid_rows: int
) -> set[tuple[int, int]]:
    out: set[tuple[int, int]] = set()
    for cx, cy in chunks:
        for dy in range(-radius, radius + 1):
            for dx in range(-radius, radius + 1):
                nx, ny = cx + dx, cy + dy
                if 0 <= nx < grid_cols and 0 <= ny < grid_rows:
                    out.add((nx, ny))
    return out


def compute_invalidation(
    height_rect: tuple[int, int, int, int] | None,
    material_rect: tuple[int, int, int, int] | None,
    chunk_size: int,
    height: int,
    width: int,
) -> InvalidationResult:
    """从变更矩形出发计算全部链路节点的失效闭包。"""
    grid_cols = (width + chunk_size - 1) // chunk_size
    grid_rows = (height + chunk_size - 1) // chunk_size
    result = InvalidationResult()
    for kind in NodeKind:
        result.invalid[kind] = set()

    seeds: list[tuple[NodeKind, tuple[int, int, int, int]]] = []
    if height_rect is not None:
        seeds.append((NodeKind.HEIGHT, height_rect))
    if material_rect is not None:
        seeds.append((NodeKind.MATERIAL, material_rect))

    for seed_kind, rect in seeds:
        direct = chunks_overlapping_rect(rect, chunk_size, height, width)
        if seed_kind == NodeKind.HEIGHT:
            result.height_chunks |= direct
        else:
            result.material_chunks |= direct
        _mark(result, seed_kind, direct, REASON[seed_kind])

    # 按依赖拓扑逐层扩展（图谱深度固定，迭代到不动点）
    changed = True
    while changed:
        changed = False
        for upstream, edges in DEPENDENCY_EDGES.items():
            sources = result.invalid.get(upstream, set())
            if not sources:
                continue
            for downstream, radius in edges:
                targets = _expand_halo(sources, radius, grid_cols, grid_rows)
                before = len(result.invalid[downstream])
                _mark(result, downstream, targets, REASON[downstream])
                if len(result.invalid[downstream]) != before:
                    changed = True
    return result


def _mark(
    result: InvalidationResult,
    kind: NodeKind,
    chunks: set[tuple[int, int]],
    reason: str,
) -> None:
    bucket = result.invalid[kind]
    for cx, cy in chunks:
        if (cx, cy) not in bucket:
            bucket.add((cx, cy))
            result.reasons[(kind, cx, cy)] = reason
