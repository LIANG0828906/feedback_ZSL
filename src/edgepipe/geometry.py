"""Geometry truth comparison for displacement and coverage diagnostics."""

from __future__ import annotations

import math
from typing import Any, Dict, Iterable, List, Mapping, Sequence, Tuple

import cv2
import numpy as np


def mask_to_2d(mask: np.ndarray) -> np.ndarray:
    if mask.ndim == 2:
        return (mask != 0).astype(np.uint8)
    if mask.shape[2] == 1:
        return (mask[:, :, 0] != 0).astype(np.uint8)
    return (np.max(mask, axis=2) != 0).astype(np.uint8)


def _distance_map(mask: np.ndarray) -> np.ndarray:
    binary = mask_to_2d(mask)
    inverse = (binary == 0).astype(np.uint8)
    return cv2.distanceTransform(inverse, cv2.DIST_L2, 5)


def _segment_points(segment: Mapping[str, Any], spacing: float = 1.0) -> np.ndarray:
    x1, y1 = float(segment["x1"]), float(segment["y1"])
    x2, y2 = float(segment["x2"]), float(segment["y2"])
    length = math.hypot(x2 - x1, y2 - y1)
    count = max(8, int(math.ceil(length / spacing)) + 1)
    t = np.linspace(0.0, 1.0, count)
    return np.column_stack((x1 + (x2 - x1) * t, y1 + (y2 - y1) * t))


def _segment_stats(
    mask: np.ndarray,
    distance: np.ndarray,
    segment: Mapping[str, Any],
    tolerance: float,
) -> Dict[str, Any]:
    height, width = mask.shape[:2]
    points = _segment_points(segment)
    xs = np.clip(np.rint(points[:, 0]).astype(int), 0, width - 1)
    ys = np.clip(np.rint(points[:, 1]).astype(int), 0, height - 1)
    sampled_distances = distance[ys, xs].astype(float)
    coverage = float(np.mean(sampled_distances <= tolerance))

    x1, y1 = float(segment["x1"]), float(segment["y1"])
    x2, y2 = float(segment["x2"]), float(segment["y2"])
    vx, vy = x2 - x1, y2 - y1
    length = max(1e-9, math.hypot(vx, vy))
    ux, uy = vx / length, vy / length
    nx, ny = -uy, ux
    binary = mask_to_2d(mask)
    border = int(math.ceil(tolerance + 3.0))
    bx0 = max(0, int(math.floor(min(x1, x2))) - border)
    bx1 = min(width, int(math.ceil(max(x1, x2))) + border + 1)
    by0 = max(0, int(math.floor(min(y1, y2))) - border)
    by1 = min(height, int(math.ceil(max(y1, y2))) + border + 1)
    patch = binary[by0:by1, bx0:bx1]
    py, px = np.nonzero(patch)
    px = px + bx0
    py = py + by0
    offsets: np.ndarray
    if px.size == 0:
        offsets = np.empty((0,), dtype=float)
    else:
        dx = px.astype(float) - x1
        dy = py.astype(float) - y1
        projection = dx * ux + dy * uy
        perp = dx * nx + dy * ny
        keep = (
            (projection >= -0.5)
            & (projection <= length + 0.5)
            & (np.abs(perp) <= tolerance + 2.0)
        )
        offsets = perp[keep]

    mean_offset = float(np.mean(offsets)) if offsets.size else 0.0
    max_abs_offset = float(np.max(np.abs(offsets))) if offsets.size else 0.0
    return {
        "id": segment.get("id"),
        "truth": {
            "x1": x1,
            "y1": y1,
            "x2": x2,
            "y2": y2,
            "length": length,
        },
        "coverage": round(coverage, 6),
        "mean_distance_px": round(float(np.mean(sampled_distances)), 6),
        "max_distance_px": round(float(np.max(sampled_distances)), 6),
        "mean_signed_perpendicular_offset_px": round(mean_offset, 6),
        "max_abs_perpendicular_offset_px": round(max_abs_offset, 6),
        "support_pixels": int(offsets.size),
    }


def _fit_circle(points: np.ndarray) -> Tuple[float, float, float]:
    x = points[:, 0].astype(float)
    y = points[:, 1].astype(float)
    matrix = np.column_stack((2.0 * x, 2.0 * y, np.ones_like(x)))
    rhs = x * x + y * y
    solution, _, _, _ = np.linalg.lstsq(matrix, rhs, rcond=None)
    cx, cy = float(solution[0]), float(solution[1])
    radius_sq = float(solution[2]) + cx * cx + cy * cy
    return cx, cy, math.sqrt(max(0.0, radius_sq))


def _circle_stats(
    mask: np.ndarray,
    circle: Mapping[str, Any],
    tolerance: float,
    angular_bins: int = 72,
) -> Dict[str, Any]:
    binary = mask_to_2d(mask)
    cx = float(circle["cx"])
    cy = float(circle["cy"])
    radius = float(circle["diameter"]) / 2.0
    ys, xs = np.nonzero(binary)
    if xs.size == 0:
        return {
            "id": circle.get("id"),
            "truth": {"cx": cx, "cy": cy, "diameter": radius * 2.0},
            "coverage": 0.0,
            "center_shift_px": float("inf"),
            "radius_error_px": float("inf"),
            "fit_center": [cx, cy],
            "fit_radius_px": 0.0,
            "valid": False,
        }
    dx = xs.astype(float) - cx
    dy = ys.astype(float) - cy
    radial = np.hypot(dx, dy)
    ring_mask = np.abs(radial - radius) <= tolerance + 2.0
    selected = np.column_stack((xs[ring_mask], ys[ring_mask]))

    coverage = 0.0
    if selected.size:
        angles = np.mod(np.arctan2(selected[:, 1] - cy, selected[:, 0] - cx), 2.0 * math.pi)
        bins = np.floor(angles / (2.0 * math.pi) * angular_bins).astype(int)
        bins = np.clip(bins, 0, angular_bins - 1)
        coverage = float(np.unique(bins).size / angular_bins)

    if len(selected) >= 8:
        fit_cx, fit_cy, fit_radius = _fit_circle(selected)
        valid = True
    else:
        fit_cx, fit_cy, fit_radius = cx, cy, 0.0
        valid = False
    shift = math.hypot(fit_cx - cx, fit_cy - cy) if valid else float("inf")
    radius_error = abs(fit_radius - radius) if valid else float("inf")
    return {
        "id": circle.get("id"),
        "truth": {"cx": cx, "cy": cy, "diameter": radius * 2.0},
        "coverage": round(coverage, 6),
        "center_shift_px": round(float(shift), 6),
        "radius_error_px": round(float(radius_error), 6),
        "fit_center": [round(float(fit_cx), 6), round(float(fit_cy), 6)],
        "fit_radius_px": round(float(fit_radius), 6),
        "support_pixels": int(len(selected)),
        "valid": bool(valid),
    }


def geometric_stats(
    mask: np.ndarray,
    truth: Mapping[str, Any],
    *,
    tolerance: float = 3.5,
) -> Dict[str, Any]:
    """Compare a final mask against line and circle truth primitives."""

    binary = mask_to_2d(mask)
    distance = _distance_map(binary)
    segments = [
        _segment_stats(binary, distance, segment, tolerance)
        for segment in truth.get("segments", [])
    ]
    circles = [
        _circle_stats(binary, circle, tolerance)
        for circle in truth.get("circles", [])
    ]

    segment_offsets = [
        abs(item["mean_signed_perpendicular_offset_px"]) for item in segments
    ]
    circle_shifts = [item["center_shift_px"] for item in circles]
    circle_radius_errors = [item["radius_error_px"] for item in circles]
    max_displacement = 0.0
    if segment_offsets:
        max_displacement = max(max_displacement, max(segment_offsets))
    if circle_shifts:
        max_displacement = max(max_displacement, max(circle_shifts))

    coverage_ok = all(item["coverage"] >= 0.55 for item in segments + circles)
    displacement_ok = max_displacement <= tolerance
    circle_fit_ok = all(
        item["valid"] and item["radius_error_px"] <= tolerance
        for item in circles
    )
    all_pass = bool(coverage_ok and displacement_ok and circle_fit_ok)
    return {
        "tolerance_px": float(tolerance),
        "mask_pixels": int(np.count_nonzero(binary)),
        "segments": segments,
        "circles": circles,
        "max_abs_systematic_displacement_px": round(float(max_displacement), 6),
        "max_circle_radius_error_px": round(
            float(max(circle_radius_errors)) if circle_radius_errors else 0.0, 6
        ),
        "coverage_ok": bool(coverage_ok),
        "displacement_ok": bool(displacement_ok),
        "circle_fit_ok": bool(circle_fit_ok),
        "all_pass": all_pass,
    }
