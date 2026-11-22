"""确定性区域生长。

生长规则：从种子像素出发做 4-邻域 BFS，候选像素同时满足
``颜色距离(候选, 种子颜色) <= color_tol`` 与
``边梯度(候选, 当前像素) <= gradient_tol`` 才并入。
BFS 使用固定顺序的队列，集合仅作访问标记，输出与遍历顺序无关，
因此同一图像与参数在任何执行路径下逐像素一致。
"""
from __future__ import annotations
from collections import deque
from typing import Any, Dict, Optional, Tuple

import numpy as np

from segworkbench.core.errors import ErrorCode, fail
from segworkbench.pipeline.parameters import SegmentationParameters
from segworkbench.pipeline.samplegen import BLOCK_HEIGHT, BLOCK_WIDTH
from segworkbench.utils.logging_setup import get_logger

LOG = get_logger()
NEIGHBORS = ((-1, 0), (1, 0), (0, -1), (0, 1))


def default_seed(block_id: str) -> Tuple[int, int]:
    """默认种子：块中心（样本区域均覆盖块中心）。"""
    index = int(block_id[1:])
    row, col = divmod(index, 2)
    return col * BLOCK_WIDTH + BLOCK_WIDTH // 2, row * BLOCK_HEIGHT + BLOCK_HEIGHT // 2


def resolve_seed(block_id: str, params: SegmentationParameters) -> Tuple[int, int]:
    if params.seed_points and block_id in params.seed_points:
        point = params.seed_points[block_id]
        return int(point[0]), int(point[1])
    return default_seed(block_id)


def _seed_in_truth(seed_xy: Tuple[int, int], truth: Dict[str, Any]) -> bool:
    x, y = seed_xy
    x0, y0, x1, y1 = truth["bbox_xyxy"]
    return x0 <= x <= x1 and y0 <= y <= y1


def region_grow(
    block_id: str,
    block_image: np.ndarray,
    origin_xy: Tuple[int, int],
    params: SegmentationParameters,
    truth: Optional[Dict[str, Any]] = None,
) -> Dict[str, np.ndarray]:
    """对单个块执行生长，返回原始掩码与逐像素颜色距离图。"""
    params.validate()
    height, width, channels = block_image.shape
    if channels != 3:
        raise fail(ErrorCode.E_PARA, "区域生长仅支持 3 通道图像", field="channels", actual=channels)

    seed_x, seed_y = resolve_seed(block_id, params)
    ox, oy = origin_xy
    local_seed = (seed_x - ox, seed_y - oy)
    if not (0 <= local_seed[0] < width and 0 <= local_seed[1] < height):
        raise fail(ErrorCode.E_PARA, "种子点越出块边界", field=f"seed_points.{block_id}",
                   seed=list((seed_x, seed_y)), block_origin=[ox, oy], block_size=[width, height])

    injection = params.injections.get("seed_background", {})
    if block_id in injection.get("blocks", []):
        LOG.warning("注入点 E_SEED: block=%s 种子被注入到背景 seed=%s", block_id, local_seed)

    # E_SEED：注入种子落在背景（真值 bbox 之外），或种子本身与区域颜色不匹配。
    if truth is not None and not _seed_in_truth((seed_x, seed_y), truth):
        raise fail(ErrorCode.E_SEED, "种子点落在区域外（背景）", block_id=block_id,
                   seed=[seed_x, seed_y], injection="seed_background",
                   bbox=truth["bbox_xyxy"])

    image_f = block_image.astype(np.float32)
    seed_color = image_f[local_seed[1], local_seed[0]].copy()
    color_distance = np.sqrt(((image_f - seed_color[None, None, :]) ** 2).sum(axis=2))

    mask = np.zeros((height, width), dtype=bool)
    visited = np.zeros((height, width), dtype=bool)
    queue = deque([local_seed])
    visited[local_seed[1], local_seed[0]] = True
    while queue:
        cx, cy = queue.popleft()
        if color_distance[cy, cx] > params.color_tol:
            continue
        mask[cy, cx] = True
        current = image_f[cy, cx]
        for dy, dx in NEIGHBORS:
            nx, ny = cx + dx, cy + dy
            if not (0 <= nx < width and 0 <= ny < height) or visited[ny, nx]:
                continue
            edge_gradient = float(np.abs(image_f[ny, nx] - current).max())
            if edge_gradient > params.gradient_tol:
                continue
            visited[ny, nx] = True
            queue.append((nx, ny))

    return {"raw_mask": mask, "color_distance": color_distance.astype(np.float32)}


def detect_spill(block_id: str, raw_mask: np.ndarray, truth: Dict[str, Any],
                 params: SegmentationParameters) -> None:
    """E_SPILL：生长面积相对真值面积超过比例阈值（参数注入大容差触发）。"""
    grown = int(raw_mask.sum())
    truth_area = int(truth["mask_pixel_count"])
    ratio = grown / max(truth_area, 1)
    if ratio > params.spill_area_ratio:
        raise fail(ErrorCode.E_SPILL, "区域生长外溢：掩码面积显著超过真值",
                   block_id=block_id, grown_pixels=grown, truth_pixels=truth_area,
                   ratio=round(ratio, 4), threshold=params.spill_area_ratio,
                   injection="spill_tolerance")
