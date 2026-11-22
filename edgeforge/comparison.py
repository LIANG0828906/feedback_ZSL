from __future__ import annotations

from typing import Any

import numpy as np

from .blocks import Block, block_at
from .stages import STAGE_ORDER, Stage


def first_stage_difference(
    left: dict[Stage, np.ndarray],
    right: dict[Stage, np.ndarray],
    blocks: tuple[Block, ...],
    tile_size: int,
) -> dict[str, Any] | None:
    for stage in STAGE_ORDER:
        if stage not in left or stage not in right:
            return {
                "x": 0,
                "y": 0,
                "stage": stage.value,
                "block_id": 0,
                "reason": "missing_stage",
            }
        left_array = np.asarray(left[stage])
        right_array = np.asarray(right[stage])
        if left_array.shape != right_array.shape:
            return {
                "x": 0,
                "y": 0,
                "stage": stage.value,
                "block_id": 0,
                "reason": "shape_mismatch",
                "left_shape": list(left_array.shape),
                "right_shape": list(right_array.shape),
            }
        difference = left_array != right_array
        if left_array.ndim == 3:
            difference = np.any(difference, axis=2)
        if difference.any():
            y, x = np.unravel_index(int(np.argmax(difference)), difference.shape)
            block = block_at(blocks, tile_size, int(y), int(x))
            return {
                "x": int(x),
                "y": int(y),
                "stage": stage.value,
                "stage_label": stage.label,
                "block_id": block.block_id,
                "reason": "pixel_mismatch",
            }
    return None


def metrics_differences(
    left: dict[str, dict[str, Any]],
    right: dict[str, dict[str, Any]],
) -> list[dict[str, Any]]:
    differences: list[dict[str, Any]] = []
    for stage in STAGE_ORDER:
        left_metrics = left.get(stage.value)
        right_metrics = right.get(stage.value)
        if left_metrics != right_metrics:
            differences.append(
                {
                    "stage": stage.value,
                    "left": left_metrics,
                    "right": right_metrics,
                }
            )
    return differences
