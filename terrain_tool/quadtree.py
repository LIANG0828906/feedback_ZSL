"""分块网格与 LOD 四叉树。

栅格先按 ``chunk_size`` 切成规则小块（chunk），每块记录覆盖范围、高程极值
与内容哈希；四叉树构建在 chunk 网格之上，依据每块的目标 LOD 层级递归细分。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Optional

import numpy as np


@dataclass
class Chunk:
    cx: int
    cy: int
    x0: int
    y0: int
    x1: int  # 不含
    y1: int  # 不含
    elev_min: float = 0.0
    elev_max: float = 0.0
    elev_hash: str = ""
    material_hash: str = ""
    target_level: int = 0
    roughness: float = 0.0

    @property
    def key(self) -> tuple[int, int]:
        return (self.cx, self.cy)

    @property
    def pixel_rect(self) -> tuple[int, int, int, int]:
        return (self.x0, self.y0, self.x1, self.y0)


@dataclass
class QuadNode:
    """四叉树节点，坐标单位为 chunk 网格（右/下边界不含）。"""

    cx0: int
    cy0: int
    cx1: int
    cy1: int
    depth: int
    children: Optional[list["QuadNode"]] = None  # None 表示叶子
    parent: Optional["QuadNode"] = field(default=None, repr=False)

    @property
    def is_leaf(self) -> bool:
        return self.children is None

    def leaves(self) -> list["QuadNode"]:
        if self.is_leaf:
            return [self]
        out: list[QuadNode] = []
        for child in self.children:  # type: ignore[union-attr]
            out.extend(child.leaves())
        return out

    def contains_chunk(self, cx: int, cy: int) -> bool:
        return self.cx0 <= cx < self.cx1 and self.cy0 <= cy < self.cy1

    def leaf_at(self, cx: int, cy: int) -> "QuadNode":
        node = self
        while not node.is_leaf:
            for child in node.children:  # type: ignore[union-attr]
                if child.contains_chunk(cx, cy):
                    node = child
                    break
            else:  # pragma: no cover - 逻辑上不会发生
                raise KeyError((cx, cy))
        return node

    def edge_neighbors(self, root: "QuadNode", side: str) -> list["QuadNode"]:
        """返回某一侧相邻的已选叶子节点（可能多个，也可能同级单个）。"""
        result: list[QuadNode] = []
        if side in ("left", "right"):
            edge_x = self.cx0 - 1 if side == "left" else self.cx1
            if edge_x < 0 or edge_x >= root.cx1:
                return result
            for cy in range(self.cy0, self.cy1):
                leaf = root.leaf_at(edge_x, cy)
                if not result or result[-1] is not leaf:
                    result.append(leaf)
        else:
            edge_y = self.cy0 - 1 if side == "top" else self.cy1
            if edge_y < 0 or edge_y >= root.cy1:
                return result
            for cx in range(self.cx0, self.cx1):
                leaf = root.leaf_at(cx, edge_y)
                if not result or result[-1] is not leaf:
                    result.append(leaf)
        return result


def build_chunks(height: int, width: int, chunk_size: int) -> list[Chunk]:
    if chunk_size <= 0:
        raise ValueError("chunk_size 必须为正整数")
    chunks: list[Chunk] = []
    grid_cols = (width + chunk_size - 1) // chunk_size
    grid_rows = (height + chunk_size - 1) // chunk_size
    for cy in range(grid_rows):
        y0 = cy * chunk_size
        y1 = min(height, y0 + chunk_size)
        for cx in range(grid_cols):
            x0 = cx * chunk_size
            x1 = min(width, x0 + chunk_size)
            chunks.append(Chunk(cx=cx, cy=cy, x0=x0, y0=y0, x1=x1, y1=y1))
    return chunks


def chunk_grid_dims(height: int, width: int, chunk_size: int) -> tuple[int, int]:
    """返回 (grid_cols, grid_rows)。"""
    return (
        (width + chunk_size - 1) // chunk_size,
        (height + chunk_size - 1) // chunk_size,
    )


def compute_roughness(elevations: np.ndarray, cell_size: float) -> np.ndarray:
    """整图坡度粗糙度（世界单位下的梯度模长均值口径），形状与输入相同。

    边界使用单侧差分，内部中心差分，保证全量/增量逐位一致。
    """
    gy, gx = np.gradient(elevations.astype(np.float64, copy=False), cell_size, edge_order=1)
    return np.sqrt(gx * gx + gy * gy).astype(np.float64)


def target_level_for_roughness(roughness: float, lod_threshold: float, max_level: int) -> int:
    """粗糙度越大目标层级越高；阈值越大层级越低。"""
    level = int(np.floor(roughness / lod_threshold)) if lod_threshold > 0 else max_level
    return max(0, min(max_level, level))


def build_quadtree(
    chunk_levels: np.ndarray,
    grid_cols: int,
    grid_rows: int,
    max_level: int,
) -> QuadNode:
    """依据每 chunk 目标层级构建四叉树。

    规则：节点深度 d 上，若其覆盖范围内存在 target_level > d 的 chunk，
    且节点仍可细分、未达 max_level，则继续四分。
    """

    def build(cx0: int, cy0: int, cx1: int, cy1: int, depth: int) -> QuadNode:
        node = QuadNode(cx0=cx0, cy0=cy0, cx1=cx1, cy1=cy1, depth=depth)
        span_x = cx1 - cx0
        span_y = cy1 - cy0
        # 必须两维都还有切分空间且未达深度上限；单维已为 1 的长条不能再分
        can_split = depth < max_level and span_x > 1 and span_y > 1
        if can_split:
            region = chunk_levels[cy0:cy1, cx0:cx1]
            if int(region.max()) > depth:
                mid_x = cx0 + span_x // 2
                mid_y = cy0 + span_y // 2
                cuts = (
                    (cx0, cy0, mid_x, mid_y),
                    (mid_x, cy0, cx1, mid_y),
                    (cx0, mid_y, mid_x, cy1),
                    (mid_x, mid_y, cx1, cy1),
                )
                children = []
                for a, b, c, d in cuts:
                    if c > a and d > b:
                        child = build(a, b, c, d, depth + 1)
                        child.parent = node
                        children.append(child)
                node.children = children
        return node

    return build(0, 0, grid_cols, grid_rows, 0)
