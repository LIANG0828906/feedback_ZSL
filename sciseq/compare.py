"""Cross-path equality checks and first-difference diagnostics."""

from __future__ import annotations

from typing import Any

import numpy as np

from .errors import SciSeqError
from .models import SequenceResult


def _first_pixel_difference(expected: np.ndarray, actual: np.ndarray) -> dict[str, Any]:
    if expected.shape != actual.shape:
        return {
            "frame": None,
            "y": None,
            "x": None,
            "expected_shape": list(expected.shape),
            "actual_shape": list(actual.shape),
            "mismatch_count": None,
            "max_abs_diff": None,
            "mean_abs_diff": None,
        }
    difference = np.abs(expected.astype(np.float64) - actual.astype(np.float64))
    mismatch = difference > 0.0
    mismatch_count = int(mismatch.sum())
    if mismatch_count:
        flat_index = int(np.flatnonzero(mismatch.ravel())[0])
        frame, y, x = np.unravel_index(flat_index, difference.shape)
    else:
        frame = y = x = None
    return {
        "frame": None if frame is None else int(frame),
        "y": None if y is None else int(y),
        "x": None if x is None else int(x),
        "mismatch_count": mismatch_count,
        "max_abs_diff": float(difference.max()) if difference.size else 0.0,
        "mean_abs_diff": float(difference.mean()) if difference.size else 0.0,
    }


def _first_mapping_difference(expected: Any, actual: Any, path: str = "") -> str | None:
    if type(expected) is not type(actual):
        return path or "value"
    if isinstance(expected, dict):
        if expected.keys() != actual.keys():
            return f"{path}.keys" if path else "keys"
        for key in expected:
            child = _first_mapping_difference(expected[key], actual[key], f"{path}.{key}" if path else str(key))
            if child is not None:
                return child
        return None
    if isinstance(expected, (list, tuple)):
        if len(expected) != len(actual):
            return f"{path}.length" if path else "length"
        for index, (left, right) in enumerate(zip(expected, actual)):
            child = _first_mapping_difference(left, right, f"{path}[{index}]")
            if child is not None:
                return child
        return None
    if isinstance(expected, float):
        if not (expected == actual or (np.isnan(expected) and np.isnan(actual))):
            return path or "value"
        return None
    return None if expected == actual else (path or "value")


def compare_results(
    expected: SequenceResult,
    actual: SequenceResult,
    *,
    label: str = "actual",
) -> dict[str, Any]:
    if expected.shape != actual.shape:
        raise SciSeqError(
            "E_DIVERGE",
            f"{label} result shape differs",
            field="shape",
            details={"expected": list(expected.shape), "actual": list(actual.shape)},
        )
    if expected.frame_count != actual.frame_count:
        raise SciSeqError(
            "E_DIVERGE",
            f"{label} frame count differs",
            field="frame_count",
            details={"expected": expected.frame_count, "actual": actual.frame_count},
        )
    if not np.array_equal(expected.offsets, actual.offsets):
        difference = np.abs(expected.offsets - actual.offsets)
        frame = int(np.argmax(np.max(difference, axis=1)))
        raise SciSeqError(
            "E_DIVERGE",
            f"{label} registration offsets differ",
            frame=frame,
            field="offset",
            details={
                "max_abs_diff": float(difference.max()),
                "mean_abs_diff": float(difference.mean()),
                "expected": expected.offsets[frame].tolist(),
                "actual": actual.offsets[frame].tolist(),
            },
        )
    difference = _first_pixel_difference(expected.denoised, actual.denoised)
    if difference["mismatch_count"]:
        raise SciSeqError(
            "E_DIVERGE",
            f"{label} denoised pixels differ",
            frame=difference["frame"],
            field="denoised",
            details={
                "y": difference["y"],
                "x": difference["x"],
                "mismatch_count": difference["mismatch_count"],
                "max_abs_diff": difference["max_abs_diff"],
                "mean_abs_diff": difference["mean_abs_diff"],
            },
        )
    expected_features = [[feature.to_dict() for feature in frame] for frame in expected.features]
    actual_features = [[feature.to_dict() for feature in frame] for frame in actual.features]
    feature_path = _first_mapping_difference(expected_features, actual_features)
    if feature_path is not None:
        raise SciSeqError(
            "E_DIVERGE",
            f"{label} extracted feature centers differ",
            field=f"features.{feature_path}",
            details={"expected_count": len(expected_features), "actual_count": len(actual_features)},
        )
    stats_path = _first_mapping_difference(expected.stats, actual.stats)
    if stats_path is not None:
        raise SciSeqError(
            "E_DIVERGE",
            f"{label} statistics differ",
            field=f"stats.{stats_path}",
        )
    return {
        "equal": True,
        "frames": expected.frame_count,
        "pixels": int(expected.denoised.size),
        "max_abs_diff": 0.0,
        "mismatch_count": 0,
    }


def assert_consistent(results: dict[str, SequenceResult]) -> dict[str, Any]:
    if not results:
        return {"equal": True, "paths": []}
    names = list(results)
    baseline = results[names[0]]
    reports: dict[str, Any] = {}
    for name in names[1:]:
        reports[name] = compare_results(baseline, results[name], label=name)
    return {"equal": True, "baseline": names[0], "comparisons": reports}

