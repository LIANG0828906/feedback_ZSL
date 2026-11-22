"""Exact three-path comparison without tolerance-based masking."""

from __future__ import annotations

import struct
from typing import Any

import numpy as np

from .models import ComparisonReport, CompositeOutput

STAGE_ORDER = [
    "radiance",
    "rgb_chroma",
    "linear_rgb",
    "display",
    "gamut_channel_mask",
]


def _numeric(value: Any) -> Any:
    if isinstance(value, np.generic):
        return value.item()
    if isinstance(value, np.ndarray):
        return value.tolist()
    return value


def _first_array_mismatch(left: np.ndarray, right: np.ndarray) -> tuple[int, int] | None:
    if left.shape != right.shape or left.dtype != right.dtype:
        return (0, 0)
    if left.ndim == 2:
        neq = left != right
        if not np.any(neq):
            return None
        y, x = np.argwhere(neq)[0]
        return int(y), int(x)
    if left.ndim == 3:
        neq = left != right
        if not np.any(neq):
            return None
        y, x, channel = np.argwhere(neq)[0]
        return int(y), int(x)
    return None


def compare_outputs(left: CompositeOutput, right: CompositeOutput) -> ComparisonReport:
    stages_checked: list[str] = []
    for stage in STAGE_ORDER:
        stages_checked.append(stage)
        left_value = getattr(left, stage)
        right_value = getattr(right, stage)
        mismatch = _first_array_mismatch(left_value, right_value)
        if mismatch is not None:
            y, x = mismatch
            segment_id = int(left.segment_index[y, x]) if left.segment_index.size else 0
            detail = {
                "stage": stage,
                "x": x,
                "y": y,
                "segment_id": segment_id,
                "left": _numeric(left_value[y, x]),
                "right": _numeric(right_value[y, x]),
                "left_shape": list(left_value.shape),
                "right_shape": list(right_value.shape),
            }
            return ComparisonReport(
                equal=False,
                first_mismatch=detail,
                peak_equal=left.peak_radiance == right.peak_radiance,
                gamut_equal=bool(np.array_equal(left.gamut_channel_mask, right.gamut_channel_mask)),
                stages_checked=stages_checked,
                path_summaries={"left": left.compact_summary(), "right": right.compact_summary()},
            )
    left_peak = struct.pack("<d", float(left.peak_radiance))
    right_peak = struct.pack("<d", float(right.peak_radiance))
    if left_peak != right_peak:
        x, y = left.peak_location
        return ComparisonReport(
            equal=False,
            first_mismatch={
                "stage": "peak_radiance",
                "x": int(x),
                "y": int(y),
                "segment_id": int(left.segment_index[y, x]) if left.segment_index.size else 0,
                "left": float(left.peak_radiance),
                "right": float(right.peak_radiance),
            },
            peak_equal=False,
            stages_checked=stages_checked + ["peak_radiance"],
            path_summaries={"left": left.compact_summary(), "right": right.compact_summary()},
        )
    if left.gamut_count != right.gamut_count or left.gamut_coordinates != right.gamut_coordinates:
        return ComparisonReport(
            equal=False,
            first_mismatch={
                "stage": "gamut_set",
                "x": 0,
                "y": 0,
                "segment_id": 0,
                "left_count": int(left.gamut_count),
                "right_count": int(right.gamut_count),
            },
            peak_equal=True,
            gamut_equal=False,
            stages_checked=stages_checked + ["gamut_set"],
            path_summaries={"left": left.compact_summary(), "right": right.compact_summary()},
        )
    return ComparisonReport(
        equal=True,
        first_mismatch=None,
        peak_equal=True,
        gamut_equal=True,
        stages_checked=stages_checked + ["peak_radiance", "gamut_set"],
        path_summaries={"left": left.compact_summary(), "right": right.compact_summary()},
    )
