"""编辑算子：裁剪、平滑、降采样、颜色覆盖。

全部为纯函数：输入 (points, colors, selection, params)，返回新数组，
不改动入参，相同输入与参数输出逐字节一致。选区为空、参数非法或
输出点数超出声明上限 max_output_points 时抛出 OpInvalid，
调用方（版本存储）因此不会改动已提交点云。
"""

from __future__ import annotations

import numpy as np

from .errors import OpInvalid
from .lod import hash64

OPERATORS = ("crop", "smooth", "downsample", "color_override")


def validate_selection(points: np.ndarray, selection: np.ndarray) -> np.ndarray:
    sel = np.asarray(selection, dtype=np.int64)
    if sel.ndim != 1:
        raise OpInvalid("选区必须为一维索引集合")
    if sel.shape[0] == 0:
        raise OpInvalid("选区为空", reason="empty_selection")
    if np.any(sel < 0) or np.any(sel >= points.shape[0]):
        raise OpInvalid("选区索引越界", reason="selection_out_of_range")
    if np.unique(sel).shape[0] != sel.shape[0]:
        raise OpInvalid("选区索引重复", reason="duplicate_selection")
    return np.sort(sel)


def _check_output_limit(count: int, params: dict):
    limit = params.get("max_output_points")
    if limit is not None and count > int(limit):
        raise OpInvalid(
            "算子输出点数超出声明上限",
            reason="output_limit",
            count=int(count),
            max_output_points=int(limit),
        )


def select_box(points: np.ndarray, box_min, box_max) -> np.ndarray:
    """矩形（轴对齐盒）框选，返回全局点序号（升序）。"""
    bmin = np.asarray(box_min, dtype=np.float32)
    bmax = np.asarray(box_max, dtype=np.float32)
    if bmin.shape != (3,) or bmax.shape != (3,) or np.any(bmin > bmax):
        raise OpInvalid("框选包围盒非法", reason="bad_box")
    mask = np.all(points >= bmin, axis=1) & np.all(points <= bmax, axis=1)
    return np.flatnonzero(mask).astype(np.int64)


def select_sphere(points: np.ndarray, center, radius: float) -> np.ndarray:
    """球形框选，返回全局点序号（升序）。"""
    c = np.asarray(center, dtype=np.float32)
    if c.shape != (3,) or not np.isfinite(radius) or radius <= 0:
        raise OpInvalid("球形框选参数非法", reason="bad_sphere")
    d2 = np.sum((points - c) ** 2, axis=1)
    return np.flatnonzero(d2 <= np.float32(radius) ** 2).astype(np.int64)


def op_crop(points, colors, selection, params):
    """裁剪。mode='keep' 仅保留选区；mode='remove' 删除选区。"""
    mode = params.get("mode", "keep")
    if mode not in ("keep", "remove"):
        raise OpInvalid("裁剪模式非法", reason="bad_mode", mode=mode)
    sel = validate_selection(points, selection)
    if mode == "keep":
        new_points, new_colors = points[sel], colors[sel]
    else:
        mask = np.ones(points.shape[0], dtype=bool)
        mask[sel] = False
        new_points, new_colors = points[mask], colors[mask]
    _check_output_limit(new_points.shape[0], params)
    return new_points.copy(), new_colors.copy()


def op_smooth(points, colors, selection, params):
    """拉普拉斯平滑：选区内点向半径 radius 内邻居均值移动 lam 比例。

    邻居搜索基于规则网格，候选按全局序号升序处理，结果确定性。
    """
    iterations = int(params.get("iterations", 1))
    lam = float(params.get("lambda", 0.5))
    radius = float(params.get("radius", 1.0))
    if iterations < 1:
        raise OpInvalid("平滑迭代次数非法", reason="bad_iterations")
    if not (0.0 < lam <= 1.0):
        raise OpInvalid("平滑系数非法", reason="bad_lambda")
    if not np.isfinite(radius) or radius <= 0:
        raise OpInvalid("平滑半径非法", reason="bad_radius")
    sel = validate_selection(points, selection)
    new_points = points.copy()
    new_colors = colors.copy()
    r2 = np.float64(radius) ** 2
    for _ in range(iterations):
        grid = _Grid(new_points, radius)
        moved = new_points[sel].astype(np.float64)
        for row, gi in enumerate(sel):
            cand = grid.candidates(new_points[gi])
            d2 = np.sum((new_points[cand].astype(np.float64)
                         - moved[row]) ** 2, axis=1)
            near = cand[d2 <= r2]
            if near.shape[0] > 1:
                mean = new_points[near].astype(np.float64).mean(axis=0)
                moved[row] = (1.0 - lam) * moved[row] + lam * mean
        new_points[sel] = moved.astype(np.float32)
    _check_output_limit(new_points.shape[0], params)
    return new_points, new_colors


class _Grid:
    """确定性邻居搜索网格：cell 内候选按全局序号升序（numpy 向量化）。"""

    def __init__(self, points: np.ndarray, cell_size: float):
        self.cell_size = float(cell_size)
        cells = np.floor(points.astype(np.float64)
                         / self.cell_size).astype(np.int64)
        self.shift = cells.min(axis=0)
        shifted = cells - self.shift
        self.dims = shifted.max(axis=0) + 1
        keys = (shifted[:, 0] * self.dims[1] + shifted[:, 1]) \
            * self.dims[2] + shifted[:, 2]
        self.order = np.argsort(keys, kind="stable")
        self.sorted_keys = keys[self.order]

    def candidates(self, point: np.ndarray) -> np.ndarray:
        base = np.floor(point.astype(np.float64)
                        / self.cell_size).astype(np.int64) - self.shift
        parts = []
        for dx in (-1, 0, 1):
            x = base[0] + dx
            if x < 0 or x >= self.dims[0]:
                continue
            for dy in (-1, 0, 1):
                y = base[1] + dy
                if y < 0 or y >= self.dims[1]:
                    continue
                for dz in (-1, 0, 1):
                    z = base[2] + dz
                    if z < 0 or z >= self.dims[2]:
                        continue
                    key = (x * self.dims[1] + y) * self.dims[2] + z
                    lo = np.searchsorted(self.sorted_keys, key, side="left")
                    hi = np.searchsorted(self.sorted_keys, key, side="right")
                    if hi > lo:
                        parts.append(self.order[lo:hi])
        if not parts:
            return np.empty(0, dtype=np.int64)
        return np.concatenate(parts)


def op_downsample(points, colors, selection, params):
    """降采样：在选区内确定性保留 target_count 个点（按 hash 优先级）。"""
    sel = validate_selection(points, selection)
    seed = int(params.get("seed", 0))
    if "target_count" in params:
        target = int(params["target_count"])
        if target < 1 or target > sel.shape[0]:
            raise OpInvalid("降采样目标点数非法", reason="bad_target")
    elif "ratio" in params:
        ratio = float(params["ratio"])
        if not (0.0 < ratio <= 1.0):
            raise OpInvalid("降采样比例非法", reason="bad_ratio")
        target = max(1, int(round(sel.shape[0] * ratio)))
    else:
        raise OpInvalid("降采样缺少 target_count 或 ratio", reason="missing_param")
    prio = hash64(sel, seed)
    order = np.argsort(prio, kind="stable")
    keep = np.sort(sel[order[:target]])
    drop_mask = np.ones(points.shape[0], dtype=bool)
    drop_mask[np.setdiff1d(sel, keep, assume_unique=True)] = False
    new_points = points[drop_mask].copy()
    new_colors = colors[drop_mask].copy()
    _check_output_limit(new_points.shape[0], params)
    return new_points, new_colors


def op_color_override(points, colors, selection, params):
    """颜色覆盖：将选区内点颜色设为指定 RGB。"""
    rgb = params.get("rgb")
    if (not isinstance(rgb, (list, tuple)) or len(rgb) != 3
            or any(not isinstance(v, int) or v < 0 or v > 255 for v in rgb)):
        raise OpInvalid("颜色参数非法", reason="bad_rgb")
    sel = validate_selection(points, selection)
    new_points = points.copy()
    new_colors = colors.copy()
    new_colors[sel] = np.asarray(rgb, dtype=np.uint8)
    _check_output_limit(new_points.shape[0], params)
    return new_points, new_colors


DISPATCH = {
    "crop": op_crop,
    "smooth": op_smooth,
    "downsample": op_downsample,
    "color_override": op_color_override,
}


def apply_operator(op: str, points: np.ndarray, colors: np.ndarray,
                   selection: np.ndarray, params: dict):
    """纯函数入口：返回 (new_points, new_colors)，不修改入参。"""
    fn = DISPATCH.get(op)
    if fn is None:
        raise OpInvalid("未知算子", reason="unknown_op", op=op)
    if not isinstance(params, dict):
        raise OpInvalid("算子参数必须为字典", reason="bad_params")
    return fn(points, colors, selection, params)
