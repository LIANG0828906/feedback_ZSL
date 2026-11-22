"""仅依赖 numpy 的二值形态学与连通域工具。

结构元素固定为半径 ``r`` 的实心方块；连通域使用 4-邻域、按行扫描的
BFS，标签编号按首次出现顺序分配，保证跨进程确定性。
"""
from __future__ import annotations

from collections import deque
from typing import List, Tuple

import numpy as np


def square_kernel(radius: int) -> np.ndarray:
    size = 2 * int(radius) + 1
    return np.ones((size, size), dtype=bool)


def _shift(mask: np.ndarray, dy: int, dx: int) -> np.ndarray:
    shifted = np.zeros_like(mask)
    src_y0 = max(0, -dy)
    src_y1 = mask.shape[0] - max(0, dy)
    src_x0 = max(0, -dx)
    src_x1 = mask.shape[1] - max(0, dx)
    dst_y0 = max(0, dy)
    dst_y1 = mask.shape[0] - max(0, -dy)
    dst_x0 = max(0, dx)
    dst_x1 = mask.shape[1] - max(0, -dx)
    shifted[dst_y0:dst_y1, dst_x0:dst_x1] = mask[src_y0:src_y1, src_x0:src_x1]
    return shifted


def binary_dilate(mask: np.ndarray, radius: int) -> np.ndarray:
    if radius <= 0:
        return mask.copy()
    result = np.zeros_like(mask, dtype=bool)
    for dy in range(-radius, radius + 1):
        for dx in range(-radius, radius + 1):
            result |= _shift(mask, dy, dx)
    return result


def binary_erode(mask: np.ndarray, radius: int) -> np.ndarray:
    if radius <= 0:
        return mask.copy()
    result = np.ones_like(mask, dtype=bool)
    for dy in range(-radius, radius + 1):
        for dx in range(-radius, radius + 1):
            result &= _shift(mask, dy, dx)
    return result


def opening(mask: np.ndarray, radius: int) -> np.ndarray:
    return binary_dilate(binary_erode(mask, radius), radius)


def closing(mask: np.ndarray, radius: int) -> np.ndarray:
    return binary_erode(binary_dilate(mask, radius), radius)


def connected_components(mask: np.ndarray) -> Tuple[np.ndarray, int]:
    """返回标签图（0 为背景）与连通域数量，4-邻域。"""
    labels = np.zeros(mask.shape, dtype=np.int32)
    next_label = 0
    height, width = mask.shape
    for y in range(height):
        for x in range(width):
            if not mask[y, x] or labels[y, x] != 0:
                continue
            next_label += 1
            labels[y, x] = next_label
            queue = deque([(y, x)])
            while queue:
                cy, cx = queue.popleft()
                for ny, nx in ((cy - 1, cx), (cy + 1, cx), (cy, cx - 1), (cy, cx + 1)):
                    if 0 <= ny < height and 0 <= nx < width and mask[ny, nx] and labels[ny, nx] == 0:
                        labels[ny, nx] = next_label
                        queue.append((ny, nx))
    return labels, next_label


def component_masks(mask: np.ndarray) -> List[np.ndarray]:
    labels, count = connected_components(mask)
    return [labels == index for index in range(1, count + 1)]
