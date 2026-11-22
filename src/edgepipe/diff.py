"""First-difference localization and E_DIFF reporting."""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any, Dict, Mapping, Optional, Sequence

import cv2
import numpy as np

from .config import STAGES
from .errors import EdgePipeError, ErrorCode
from .ops import BlockGrid


@dataclass
class FirstDifference:
    y: Optional[int]
    x: Optional[int]
    channel: Optional[int]
    block_id: Optional[str]
    stage: str
    shape_a: Sequence[int]
    shape_b: Sequence[int]
    value_a: Any = None
    value_b: Any = None
    reason: str = "pixel_mismatch"

    def to_dict(self) -> Dict[str, Any]:
        return {
            "coordinate": (
                None if self.y is None or self.x is None else {"y": self.y, "x": self.x}
            ),
            "channel": self.channel,
            "block_id": self.block_id,
            "stage": self.stage,
            "shape_a": list(self.shape_a),
            "shape_b": list(self.shape_b),
            "value_a": self.value_a,
            "value_b": self.value_b,
            "reason": self.reason,
        }


def first_mask_difference(
    first: np.ndarray,
    second: np.ndarray,
    *,
    stage: str = "final",
    grid: Optional[BlockGrid] = None,
) -> Optional[FirstDifference]:
    if first.shape != second.shape:
        return FirstDifference(
            y=None,
            x=None,
            channel=None,
            block_id=None,
            stage=stage,
            shape_a=first.shape,
            shape_b=second.shape,
            reason="shape_mismatch",
        )
    unequal = np.argwhere(first != second)
    if unequal.size == 0:
        return None
    coordinate = unequal[0]
    y = int(coordinate[0])
    x = int(coordinate[1])
    channel = int(coordinate[2]) if coordinate.size > 2 else None
    block_id = grid.block_at(y, x).block_id if grid is not None else None
    return FirstDifference(
        y=y,
        x=x,
        channel=channel,
        block_id=block_id,
        stage=stage,
        shape_a=first.shape,
        shape_b=second.shape,
        value_a=_scalar(first[tuple(int(v) for v in coordinate)]),
        value_b=_scalar(second[tuple(int(v) for v in coordinate)]),
    )


def raise_for_mask_difference(
    first: np.ndarray,
    second: np.ndarray,
    *,
    stage: str = "final",
    grid: Optional[BlockGrid] = None,
) -> None:
    difference = first_mask_difference(first, second, stage=stage, grid=grid)
    if difference is not None:
        raise EdgePipeError(
            ErrorCode.DIFF,
            "掩膜存在逐像素差异",
            field="mask",
            details=difference.to_dict(),
        )


def compare_stage_masks(
    first: Mapping[str, np.ndarray],
    second: Mapping[str, np.ndarray],
    *,
    grid: Optional[BlockGrid] = None,
) -> None:
    for stage in STAGES:
        if stage not in first or stage not in second:
            raise EdgePipeError(
                ErrorCode.DIFF,
                "阶段输出缺失，无法比较",
                field="stage",
                details={
                    "stage": stage,
                    "first_has_stage": stage in first,
                    "second_has_stage": stage in second,
                    "coordinate": None,
                    "block_id": None,
                },
            )
        raise_for_mask_difference(
            first[stage], second[stage], stage=stage, grid=grid
        )


def compare_geometry(
    first: Optional[Mapping[str, Any]],
    second: Optional[Mapping[str, Any]],
) -> None:
    first_json = json.dumps(first, sort_keys=True, ensure_ascii=False)
    second_json = json.dumps(second, sort_keys=True, ensure_ascii=False)
    if first_json != second_json:
        raise EdgePipeError(
            ErrorCode.DIFF,
            "几何统计不一致",
            field="geometry",
            details={
                "coordinate": None,
                "block_id": None,
                "stage": "geometry",
            },
        )


def compare_commit_sequences(
    first: Sequence[Mapping[str, Any]], second: Sequence[Mapping[str, Any]]
) -> None:
    first_json = json.dumps(list(first), sort_keys=True, ensure_ascii=False)
    second_json = json.dumps(list(second), sort_keys=True, ensure_ascii=False)
    if first_json != second_json:
        mismatch_index = None
        for index, (left, right) in enumerate(zip(first, second)):
            if dict(left) != dict(right):
                mismatch_index = index
                break
        raise EdgePipeError(
            ErrorCode.DIFF,
            "分块阶段提交序列不一致",
            field="commit_sequence",
            details={
                "coordinate": None,
                "block_id": (
                    first[mismatch_index].get("block_id")
                    if mismatch_index is not None
                    else None
                ),
                "stage": (
                    first[mismatch_index].get("stage")
                    if mismatch_index is not None
                    else "commit_sequence"
                ),
                "mismatch_index": mismatch_index,
                "first_length": len(first),
                "second_length": len(second),
            },
        )


def difference_heatmap(first: np.ndarray, second: np.ndarray) -> np.ndarray:
    if first.shape != second.shape:
        raise EdgePipeError(
            ErrorCode.DIFF,
            "差异热力图输入尺寸不一致",
            field="mask.shape",
            details={"shape_a": list(first.shape), "shape_b": list(second.shape)},
        )
    delta = np.abs(first.astype(np.int16) - second.astype(np.int16))
    if delta.ndim == 3:
        delta = np.max(delta, axis=2)
    normalized = np.clip(delta.astype(np.float32) * 255.0 / 255.0, 0, 255)
    heat = cv2.applyColorMap(normalized.astype(np.uint8), cv2.COLORMAP_TURBO)
    return heat


def _scalar(value: Any) -> Any:
    if isinstance(value, np.generic):
        return value.item()
    return value
