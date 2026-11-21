"""视点驱动的多分辨率显示选择与块间边界一致性校验。

抽样序列：对全局点序号 i 计算 SplitMix64(seed ^ i)，层级 L 保留
hash % 2**L == 0 的点。该判定只依赖 (seed, 全局序号, 层级)，
与切块方式无关，因此任意分块在相同层级下的抽样并集与整份单块
结果逐点一致，这是 E_BLOCK_SEAM 校验的数学基础。

内容哈希统计口径始终针对块的完整数据，降采样只影响显示集，
不改变哈希输入。
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field

import numpy as np

from .blocks import BlockIndex
from .errors import BlockSeam, LevelBudget

MAX_LEVEL = 30


def hash64(indices: np.ndarray, seed: int) -> np.ndarray:
    """SplitMix64 向量化实现，输入全局点序号，输出确定性伪随机 uint64。"""
    z = (indices.astype(np.uint64) ^ np.uint64(seed & 0xFFFFFFFFFFFFFFFF)).copy()
    z += np.uint64(0x9E3779B97F4A7C15)
    z = (z ^ (z >> np.uint64(30))) * np.uint64(0xBF58476D1CE4E5B9)
    z = (z ^ (z >> np.uint64(27))) * np.uint64(0x94D049BB133111EB)
    return z ^ (z >> np.uint64(31))


def sampling_mask(indices: np.ndarray, level: int, seed: int) -> np.ndarray:
    """层级 L 的抽样判定：保留 hash64(i) % 2**L == 0 的点。"""
    if level <= 0:
        return np.ones(indices.shape[0], dtype=bool)
    mod = np.uint64(1 << level)
    return (hash64(indices, seed) % mod) == np.uint64(0)


def sampling_sequence(count: int, level: int, seed: int) -> np.ndarray:
    """整份点云在层级 L 下的抽样序列（保留点的全局序号，升序）。"""
    idx = np.arange(count, dtype=np.int64)
    return idx[sampling_mask(idx, level, seed)]


def screen_size_px(world_size: float, distance: float, viewport_h: int,
                   fov_y_deg: float) -> float:
    distance = max(distance, 1e-6)
    proj = viewport_h / (2.0 * math.tan(math.radians(fov_y_deg) / 2.0))
    return world_size / distance * proj


def level_for(screen_px: float, error_threshold_px: float) -> int:
    """屏幕尺寸越小允许越粗的层级。error_threshold_px 为每级误差预算。"""
    if screen_px <= 0:
        return MAX_LEVEL
    ratio = error_threshold_px / screen_px
    if ratio >= 1.0:
        return 0
    return min(MAX_LEVEL, int(math.ceil(-math.log2(ratio))))


@dataclass
class BlockDisplay:
    block_id: tuple
    level: int
    displayed: int
    kept_indices: np.ndarray


@dataclass
class DisplaySelection:
    seed: int
    blocks: list = field(default_factory=list)
    total_displayed: int = 0

    @property
    def visible_block_count(self) -> int:
        return len(self.blocks)

    def all_indices(self) -> np.ndarray:
        if not self.blocks:
            return np.empty(0, dtype=np.int64)
        return np.sort(np.concatenate([b.kept_indices for b in self.blocks]))


def _block_kept(block, level: int, seed: int, ensure_nonempty: bool):
    mask = sampling_mask(block.indices, level, seed)
    kept = block.indices[mask]
    if kept.shape[0] == 0 and ensure_nonempty and block.count > 0:
        pick = int(np.argmin(hash64(block.indices, seed)))
        kept = block.indices[pick:pick + 1]
    return kept, int(kept.shape[0])


def select_display(index: BlockIndex, viewpoint, viewport_h: int = 1080,
                   fov_y_deg: float = 60.0, error_threshold_px: float = 64.0,
                   budget: int = 2_000_000, seed: int = 0,
                   forced_level: int = None,
                   ensure_nonempty: bool = True) -> DisplaySelection:
    """按视点距离与屏幕误差阈值为每块选择显示层级，并在超预算时整体降档。

    预算内无法完成（全部块已到最大层级仍超预算）时抛出 LevelBudget，
    detail.usage 给出各块预算用量。
    """
    vp = np.asarray(viewpoint, dtype=np.float64)
    entries = []
    for cid in sorted(index.blocks):
        block = index.blocks[cid]
        center = (block.bbox_min + block.bbox_max) / 2.0
        distance = float(np.linalg.norm(center - vp))
        world_size = float(np.linalg.norm(block.bbox_max - block.bbox_min))
        if forced_level is not None:
            level = forced_level
        else:
            px = screen_size_px(world_size, distance, viewport_h, fov_y_deg)
            level = level_for(px, error_threshold_px)
        entries.append([block, level])

    def compute():
        out = []
        total = 0
        for block, level in entries:
            kept, cnt = _block_kept(block, level, seed, ensure_nonempty)
            out.append(BlockDisplay(block.block_id, level, cnt, kept))
            total += cnt
        return out, total

    displays, total = compute()
    while total > budget:
        if all(level >= MAX_LEVEL for _, level in entries):
            usage = {d.block_id: d.displayed for d in displays}
            raise LevelBudget(
                "显示选择超出预算且已达最大层级",
                budget=budget, total=total, usage=usage,
            )
        for entry in entries:
            entry[1] += 1
        displays, total = compute()

    return DisplaySelection(seed=seed, blocks=displays, total_displayed=total)


def verify_seam_consistency(index: BlockIndex, level: int, seed: int) -> np.ndarray:
    """以整份点云单块显示为真值，校验分块显示逐点一致。

    返回真值抽样序列。不一致时抛出 BlockSeam 并定位边界块对。
    """
    truth = sampling_sequence(index.total_points, level, seed)
    selection = select_display(index, viewpoint=index.origin,
                               budget=index.total_points + 1,
                               seed=seed, forced_level=level,
                               ensure_nonempty=False)
    got = selection.all_indices()
    if got.shape[0] == truth.shape[0] and np.array_equal(got, truth):
        return truth
    truth_set = set(truth.tolist())
    got_set = set(got.tolist())
    diff = sorted(truth_set ^ got_set)
    bad = diff[0] if diff else -1
    pair = _locate_boundary_pair(index, bad)
    raise BlockSeam(
        "分块显示与单块真值不一致",
        level=level,
        point_index=bad,
        block_pair=pair,
        truth_count=int(truth.shape[0]),
        got_count=int(got.shape[0]),
    )


def _locate_boundary_pair(index: BlockIndex, point_index: int):
    """根据点序号定位其所在块及跨边界相邻块，用于 E_BLOCK_SEAM 定位。"""
    if point_index < 0 or not index.blocks:
        return None
    for cid, block in index.blocks.items():
        pos = np.searchsorted(block.indices, point_index)
        if pos < block.indices.shape[0] and block.indices[pos] == point_index:
            neighbors = []
            for delta in ((1, 0, 0), (0, 1, 0), (0, 0, 1),
                          (-1, 0, 0), (0, -1, 0), (0, 0, -1)):
                nb = (cid[0] + delta[0], cid[1] + delta[1], cid[2] + delta[2])
                if nb in index.blocks:
                    neighbors.append(nb)
            other = neighbors[0] if neighbors else None
            return (cid, other)
    return None
