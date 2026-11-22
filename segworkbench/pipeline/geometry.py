"""掩码几何统计与闭合轮廓提取。

坐标约定：像素中心坐标为整数 (x=列, y=行)，原点在左上角，与掩码数组
的列/行索引完全一致。重心直接由掩码像素的均值计算（一阶矩），不经过
轮廓近似，因此与轮廓阶数无关。
"""
from __future__ import annotations

from typing import Any, Dict, List, Tuple

import numpy as np

from segworkbench.pipeline.morphology import connected_components

# Moore 邻域按顺时针排列，起始方向为正左。
MOORE_OFFSETS: Tuple[Tuple[int, int], ...] = (
    (0, -1), (1, -1), (1, 0), (1, 1),
    (0, 1), (-1, 1), (-1, 0), (-1, -1),
)


def trace_contour(mask: np.ndarray, start: Tuple[int, int]) -> List[Tuple[int, int]]:
    """Moore 邻域边界跟踪（Jacob 停止准则），返回闭合有序像素坐标。

    每一步从上一步进入方向的顺时针下一格开始扫描 8 个邻居，找到的
    第一个掩码像素即下一边界点；回到起点两次后停止，保证闭合。
    """
    start_x, start_y = start
    contour: List[Tuple[int, int]] = []
    height, width = mask.shape
    current = (start_x, start_y)
    previous: Tuple[int, int] | None = None
    start_visits = 0
    while True:
        contour.append(current)
        if previous is None:
            back_direction = 0
        else:
            dx = previous[0] - current[0]
            dy = previous[1] - current[1]
            back_direction = MOORE_OFFSETS.index((dy, dx))
        found = None
        for offset in range(1, 9):
            direction = (back_direction + offset) % 8
            dy, dx = MOORE_OFFSETS[direction]
            nx, ny = current[0] + dx, current[1] + dy
            if 0 <= nx < width and 0 <= ny < height and mask[ny, nx]:
                found = (nx, ny)
                break
        if found is None:  # 单点区域
            break
        previous, current = current, found
        if current == (start_x, start_y):
            start_visits += 1
            if start_visits >= 2:
                break
        if len(contour) > mask.size * 2:  # 防御性上限，正常闭合不会触发
            raise RuntimeError("轮廓跟踪未收敛")
    return contour


def region_statistics(mask: np.ndarray) -> List[Dict[str, Any]]:
    """对掩码中的每个 4-连通域计算几何统计与闭合轮廓。"""
    labels, count = connected_components(mask)
    stats: List[Dict[str, Any]] = []
    for label in range(1, count + 1):
        component = labels == label
        ys, xs = np.nonzero(component)
        area = int(xs.size)
        centroid = [float(xs.mean()), float(ys.mean())]
        bbox = [int(xs.min()), int(ys.min()), int(xs.max()), int(ys.max())]
        start = (int(xs.min()), int(ys[np.argmin(xs)]))
        contour = trace_contour(component, start)
        stats.append(
            {
                "label": label,
                "area_pixels": area,
                "centroid_xy": centroid,
                "bbox_xyxy": bbox,
                "contour": [[int(x), int(y)] for x, y in contour],
                "contour_closed": True,
            }
        )
    return stats


def deviation_report(measured: Dict[str, Any], truth: Dict[str, Any]) -> Dict[str, Any]:
    """逐区域比对实测几何与解析真值。"""
    area_tolerance = float(measured.get("area_tolerance", 0.10))
    truth_area = int(truth["mask_pixel_count"])
    measured_area = int(measured["area_pixels"])
    area_rel = abs(measured_area - truth_area) / max(truth_area, 1)
    tx, ty = truth["centroid_xy"]
    mx, my = measured["centroid_xy"]
    tx0, ty0, tx1, ty1 = truth["bbox_xyxy"]
    mx0, my0, mx1, my1 = measured["bbox_xyxy"]
    return {
        "area_truth": truth_area,
        "area_measured": measured_area,
        "area_relative_error": float(area_rel),
        "centroid_error_xy": [float(mx - tx), float(my - ty)],
        "centroid_euclidean_error": float(np.hypot(mx - tx, my - ty)),
        "bbox_error_xyxy": [mx0 - tx0, my0 - ty0, mx1 - tx1, my1 - ty1],
        "within_area_tolerance": bool(area_rel <= area_tolerance),
    }
