from __future__ import annotations

from typing import Any, Sequence

import numpy as np

from .config import PipelineConfig
from .stages import Stage


def _as_2d_mask(mask: np.ndarray) -> np.ndarray:
    array = np.asarray(mask)
    if array.ndim == 2:
        return array.astype(bool, copy=False)
    return np.any(array != 0, axis=2)


def stage_mask(stage: Stage, array: np.ndarray, config: PipelineConfig) -> np.ndarray:
    if stage is Stage.GRADIENT:
        values = np.asarray(array)
        if values.ndim == 2:
            return values >= config.low_threshold
        return np.any(values >= config.low_threshold, axis=2)
    return _as_2d_mask(array)


def _dilate_one(mask: np.ndarray) -> np.ndarray:
    padded = np.pad(mask, 1, mode="constant", constant_values=False)
    height, width = mask.shape
    output = np.zeros_like(mask, dtype=bool)
    for dy in range(3):
        for dx in range(3):
            output |= padded[dy : dy + height, dx : dx + width]
    return output


def coverage_against_truth(
    prediction: np.ndarray,
    truth: np.ndarray,
    tolerance: int = 1,
) -> dict[str, float | int]:
    pred = _as_2d_mask(prediction)
    target = np.asarray(truth, dtype=bool)
    if tolerance == 1:
        pred_supported = _dilate_one(pred)
        truth_supported = _dilate_one(target)
    elif tolerance <= 0:
        pred_supported = pred
        truth_supported = target
    else:
        pred_supported = _dilate_square(pred, tolerance)
        truth_supported = _dilate_square(target, tolerance)
    truth_count = int(target.sum())
    pred_count = int(pred.sum())
    matched_truth = int(np.count_nonzero(target & pred_supported))
    matched_pred = int(np.count_nonzero(pred & truth_supported))
    coverage = matched_truth / truth_count if truth_count else 1.0
    precision = matched_pred / pred_count if pred_count else 1.0
    f1 = (
        2.0 * coverage * precision / (coverage + precision)
        if coverage + precision > 0
        else 0.0
    )
    return {
        "coverage": float(coverage),
        "precision": float(precision),
        "f1": float(f1),
        "truth_pixels": truth_count,
        "predicted_pixels": pred_count,
        "matched_truth_pixels": matched_truth,
    }


def _dilate_square(mask: np.ndarray, radius: int) -> np.ndarray:
    padded = np.pad(mask, radius, mode="constant", constant_values=False)
    height, width = mask.shape
    output = np.zeros_like(mask, dtype=bool)
    for dy in range(2 * radius + 1):
        for dx in range(2 * radius + 1):
            output |= padded[dy : dy + height, dx : dx + width]
    return output


def _nearest_foreground_distance(
    mask: np.ndarray,
    x: int,
    y: int,
) -> float | None:
    foreground = np.argwhere(mask)
    if foreground.size == 0:
        return None
    dx = foreground[:, 1].astype(np.float64) - float(x)
    dy = foreground[:, 0].astype(np.float64) - float(y)
    squared = dx * dx + dy * dy
    return float(np.sqrt(float(squared.min())))


def endpoint_statistics(
    prediction: np.ndarray,
    lines: Sequence[dict[str, Any]],
) -> dict[str, Any]:
    mask = _as_2d_mask(prediction)
    entries: list[dict[str, Any]] = []
    finite_values: list[float] = []
    missing = 0
    for line in lines:
        line_id = str(line.get("id", "?"))
        for endpoint_name in ("start", "end"):
            point = line[endpoint_name]
            x, y = int(point[0]), int(point[1])
            distance = _nearest_foreground_distance(mask, x, y)
            if distance is None:
                missing += 1
            else:
                finite_values.append(distance)
            entries.append(
                {
                    "line_id": line_id,
                    "endpoint": endpoint_name,
                    "x": x,
                    "y": y,
                    "distance": distance,
                }
            )
    return {
        "endpoints": entries,
        "mean_distance": (
            float(sum(finite_values) / len(finite_values)) if finite_values else None
        ),
        "max_distance": float(max(finite_values)) if finite_values else None,
        "missing_endpoints": missing,
        "total_endpoints": len(entries),
    }


def _circle_ring(
    circle: dict[str, Any],
    width: int,
    height: int,
) -> np.ndarray:
    center_x, center_y = int(circle["center"][0]), int(circle["center"][1])
    radius = float(circle["diameter"]) / 2.0
    thickness = max(1.0, float(circle.get("thickness", 2)))
    stroke_radius = thickness / 2.0
    pad = int(np.ceil(stroke_radius)) + 2
    x0 = max(0, int(np.floor(center_x - radius)) - pad)
    x1 = min(width, int(np.ceil(center_x + radius)) + pad + 1)
    y0 = max(0, int(np.floor(center_y - radius)) - pad)
    y1 = min(height, int(np.ceil(center_y + radius)) + pad + 1)
    yy, xx = np.mgrid[y0:y1, x0:x1].astype(np.float32)
    distance = np.sqrt((xx - center_x) ** 2 + (yy - center_y) ** 2)
    local = np.abs(distance - radius) <= stroke_radius
    mask = np.zeros((height, width), dtype=bool)
    mask[y0:y1, x0:x1] = local
    return mask


def circle_statistics(
    prediction: np.ndarray,
    circles: Sequence[dict[str, Any]],
    width: int,
    height: int,
) -> dict[str, Any]:
    mask = _as_2d_mask(prediction)
    entries: list[dict[str, Any]] = []
    coverages: list[float] = []
    for circle in circles:
        ring = _circle_ring(circle, width, height)
        if not ring.any():
            coverage = 1.0
        else:
            supported = _dilate_one(mask)
            coverage = float(np.count_nonzero(ring & supported) / ring.sum())
        coverages.append(coverage)
        entries.append(
            {
                "id": circle.get("id", "?"),
                "center": list(circle["center"]),
                "diameter": int(circle["diameter"]),
                "coverage": coverage,
            }
        )
    return {
        "circles": entries,
        "mean_coverage": (
            float(sum(coverages) / len(coverages)) if coverages else None
        ),
    }


def compute_stage_metrics(
    stage: Stage,
    array: np.ndarray,
    truth_mask: np.ndarray,
    lines: Sequence[dict[str, Any]],
    circles: Sequence[dict[str, Any]],
    config: PipelineConfig,
) -> dict[str, Any]:
    prediction = stage_mask(stage, array, config)
    coverage = coverage_against_truth(prediction, truth_mask)
    endpoints = endpoint_statistics(prediction, lines)
    circle_result = circle_statistics(
        prediction,
        circles,
        truth_mask.shape[1],
        truth_mask.shape[0],
    )
    return {
        "stage": stage.value,
        "stage_label": stage.label,
        **coverage,
        "truth_edge_coverage": float(np.asarray(truth_mask, dtype=bool).mean()),
        "endpoint": endpoints,
        "circle": circle_result,
    }


def compute_all_metrics(
    stage_arrays: dict[Stage, np.ndarray],
    truth_mask: np.ndarray,
    lines: Sequence[dict[str, Any]],
    circles: Sequence[dict[str, Any]],
    config: PipelineConfig,
) -> dict[str, dict[str, Any]]:
    return {
        stage.value: compute_stage_metrics(
            stage,
            stage_arrays[stage],
            truth_mask,
            lines,
            circles,
            config,
        )
        for stage in Stage
        if stage in stage_arrays
    }

