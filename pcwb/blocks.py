"""空间分块显示层级。

划分规则：以点云包围盒最小角为原点，按可配置边长 edge_length 做规则网格，
点 p 所属块为 floor((p - origin) / edge_length)。落在共享边界面上的点按
floor 规则唯一归属一侧块，因此块间去重依据点坐标与点序精确判定，
不依赖任何浮点近似阈值。块内点序保持载入顺序（稳定排序），
同一版本点云重复切分得到逐字节一致的块数据。
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .errors import PcwbError
from .pcformat import PointCloud, bbox_of, content_hash

BlockId = tuple


@dataclass
class Block:
    block_id: BlockId
    indices: np.ndarray
    bbox_min: np.ndarray
    bbox_max: np.ndarray
    content_hash: str

    @property
    def count(self) -> int:
        return int(self.indices.shape[0])


@dataclass
class BlockIndex:
    edge_length: float
    origin: np.ndarray
    blocks: dict = field(default_factory=dict)
    total_points: int = 0

    def block_of_point(self, point: np.ndarray) -> BlockId:
        cell = np.floor((point - self.origin) / self.edge_length).astype(np.int64)
        return (int(cell[0]), int(cell[1]), int(cell[2]))


def cell_keys(points: np.ndarray, origin: np.ndarray, edge_length: float):
    """返回 (cell_coords int64 (N,3), 线性化 key (N,))。key 编码保证字典序唯一。"""
    cells = np.floor((points - origin) / edge_length).astype(np.int64)
    mins = cells.min(axis=0)
    shifted = cells - mins
    dims = shifted.max(axis=0) + 1
    key = (shifted[:, 0] * dims[1] + shifted[:, 1]) * dims[2] + shifted[:, 2]
    return cells, key


def build_blocks(cloud: PointCloud, edge_length: float) -> BlockIndex:
    if edge_length <= 0:
        raise PcwbError("块边长必须为正", edge_length=edge_length)
    origin, _ = bbox_of(cloud.points)
    origin = origin.astype(np.float64)
    index = BlockIndex(edge_length=float(edge_length), origin=origin)
    n = cloud.count
    index.total_points = n
    if n == 0:
        return index
    cells, key = cell_keys(cloud.points.astype(np.float64), origin, edge_length)
    order = np.argsort(key, kind="stable")
    sorted_keys = key[order]
    boundaries = np.flatnonzero(np.diff(sorted_keys)) + 1
    starts = np.concatenate(([0], boundaries))
    ends = np.concatenate((boundaries, [n]))
    for s, e in zip(starts, ends):
        idx = order[s:e]
        cid = tuple(int(v) for v in cells[idx[0]])
        bmin, bmax = bbox_of(cloud.points[idx])
        index.blocks[cid] = Block(
            block_id=cid,
            indices=idx,
            bbox_min=bmin,
            bbox_max=bmax,
            content_hash=content_hash(cloud.points[idx], cloud.colors[idx]),
        )
    return index


@dataclass
class RebuildReport:
    old_block_count: int
    new_block_count: int
    old_total_points: int
    new_total_points: int
    replaced_blocks: list

    @property
    def point_diff(self) -> int:
        return self.new_total_points - self.old_total_points


def rebuild(cloud: PointCloud, edge_length: float, old):
    """重建显示层级，返回 (新 BlockIndex, RebuildReport)。"""
    new = build_blocks(cloud, edge_length)
    if old is None:
        report = RebuildReport(0, len(new.blocks), 0, new.total_points,
                               sorted(new.blocks.keys()))
        return new, report
    changed = []
    for cid in sorted(set(old.blocks) | set(new.blocks)):
        ob = old.blocks.get(cid)
        nb = new.blocks.get(cid)
        if ob is None or nb is None or ob.content_hash != nb.content_hash:
            changed.append(cid)
    report = RebuildReport(
        old_block_count=len(old.blocks),
        new_block_count=len(new.blocks),
        old_total_points=old.total_points,
        new_total_points=new.total_points,
        replaced_blocks=changed,
    )
    return new, report
