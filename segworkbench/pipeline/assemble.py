"""把已提交的分块掩码拼接为全图掩码并汇总几何偏差。"""
from __future__ import annotations

from typing import Any, Dict, List

import numpy as np

from segworkbench.core.errors import ErrorCode, fail
from segworkbench.persistence.checkpoint import CheckpointStore
from segworkbench.pipeline.blocks import all_block_ids, block_rect
from segworkbench.pipeline.samplegen import BLOCK_COLS, BLOCK_ROWS, IMAGE_HEIGHT, IMAGE_WIDTH


def stitch_final_mask(store: CheckpointStore, block_ids: List[str] | None = None) -> np.ndarray:
    """仅从持久化的 clean 阶段产物拼回 HxW 布尔掩码。"""
    block_ids = block_ids or all_block_ids()
    full = np.zeros((IMAGE_HEIGHT, IMAGE_WIDTH), dtype=np.uint8)
    for block_id in block_ids:
        record = store.load_phase(block_id, "clean")
        if record is None:
            raise fail(ErrorCode.E_STATE, "块 clean 阶段尚未提交，无法拼接", block_id=block_id)
        x0, y0, x1, y1 = block_rect(block_id)
        full[y0:y1, x0:x1] = record["array"]
    return full


def collect_deviation_table(store: CheckpointStore, block_ids: List[str] | None = None) -> List[Dict[str, Any]]:
    block_ids = block_ids or all_block_ids()
    table: List[Dict[str, Any]] = []
    for block_id in block_ids:
        record = store.load_phase(block_id, "stats")
        if record is None:
            raise fail(ErrorCode.E_STATE, "块 stats 阶段尚未提交", block_id=block_id)
        table.append(record["metadata"])
    return table


def overall_area_tolerance(table: List[Dict[str, Any]], tolerance: float = 0.10) -> Dict[str, Any]:
    errors = [item["deviation"]["area_relative_error"] for item in table]
    centroid = [item["deviation"]["centroid_euclidean_error"] for item in table]
    return {
        "declared_area_tolerance": tolerance,
        "max_area_relative_error": float(max(errors)),
        "mean_area_relative_error": float(np.mean(errors)),
        "max_centroid_error": float(max(centroid)),
        "all_within_tolerance": bool(all(value <= tolerance for value in errors)),
        "block_count": len(table),
        "grid": {"rows": BLOCK_ROWS, "cols": BLOCK_COLS},
    }
