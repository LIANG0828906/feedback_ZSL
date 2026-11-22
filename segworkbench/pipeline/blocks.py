"""分块裁剪、坏块注入与块级真值访问。"""
from __future__ import annotations

import os
from typing import Any, Dict, List, Tuple

import numpy as np

from segworkbench.core.errors import ErrorCode, fail
from segworkbench.pipeline.samplegen import BLOCK_COLS, BLOCK_HEIGHT, BLOCK_ROWS, BLOCK_WIDTH
from segworkbench.utils.hashing import sha256_array
from segworkbench.utils.logging_setup import get_logger

LOG = get_logger()


def all_block_ids() -> List[str]:
    return [f"b{index:02d}" for index in range(BLOCK_ROWS * BLOCK_COLS)]


def block_rect(block_id: str) -> Tuple[int, int, int, int]:
    index = int(block_id[1:])
    row, col = divmod(index, BLOCK_COLS)
    x0 = col * BLOCK_WIDTH
    y0 = row * BLOCK_HEIGHT
    return x0, y0, x0 + BLOCK_WIDTH, y0 + BLOCK_HEIGHT


def crop_block(image: np.ndarray, block_id: str) -> np.ndarray:
    x0, y0, x1, y1 = block_rect(block_id)
    return np.ascontiguousarray(image[y0:y1, x0:x1])


def apply_bad_block_injection(block_id: str, block_image: np.ndarray,
                              injections: Dict[str, Any]) -> np.ndarray:
    """E_BLOCK：输入文件/参数驱动的坏块（损坏字节、全零或尺寸异常）。"""
    spec = injections.get("bad_block", {})
    targets = spec.get("blocks", [])
    if block_id not in targets:
        return block_image
    mode = spec.get("mode", "corrupt")
    LOG.warning("注入点 E_BLOCK: block=%s mode=%s", block_id, mode)
    if mode == "zero":
        return np.zeros_like(block_image)
    if mode == "corrupt":
        corrupted = block_image.copy()
        rng = np.random.default_rng(spec.get("seed", 777))
        positions = rng.integers(0, block_image.size, size=max(1, block_image.size // 20))
        flat = corrupted.reshape(-1)
        flat[positions] = rng.integers(0, 256, size=positions.size, dtype=np.int64).astype(np.uint8)
        return corrupted
    if mode == "hash_break":
        corrupted = block_image.copy()
        corrupted.reshape(-1)[0] ^= 0xFF
        return corrupted
    raise fail(ErrorCode.E_PARA, "未知坏块注入模式", field="bad_block.mode", value=mode)


def verify_block_baseline(block_id: str, block_image: np.ndarray, expected_block_hash: str | None) -> None:
    if expected_block_hash is None:
        return
    actual = sha256_array(block_image)
    if actual != expected_block_hash:
        raise fail(ErrorCode.E_BLOCK, "块内容哈希与基线不一致（坏块）", block_id=block_id,
                   expected=expected_block_hash, actual=actual)


def truth_for_block(regions: List[Any], block_id: str) -> Dict[str, Any]:
    for region in regions:
        if region.block_id == block_id:
            return region.truth
    raise fail(ErrorCode.E_REGION, "真值区域缺失", block_id=block_id, missing=[block_id])


def localize_truth(truth: Dict[str, Any], block_id: str) -> Dict[str, Any]:
    """把图像级真值坐标换算为块局部坐标，供块内判定使用。"""
    x0, y0, _, _ = block_rect(block_id)
    bx0, by0, bx1, by1 = truth["bbox_xyxy"]
    return {
        "mask_pixel_count": truth["mask_pixel_count"],
        "centroid_xy": [truth["centroid_xy"][0] - x0, truth["centroid_xy"][1] - y0],
        "bbox_xyxy": [bx0 - x0, by0 - y0, bx1 - x0, by1 - y0],
    }
