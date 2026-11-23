"""Minimum-cost seam selection over registered overlap regions."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Literal

import cv2
import numpy as np

from .config import BlendConfig


@dataclass(frozen=True)
class SeamPath:
    orientation: Literal["vertical", "horizontal"]
    points_xy: tuple[tuple[int, int], ...]
    bbox: tuple[int, int, int, int]
    total_cost: float

    def to_dict(self) -> dict[str, object]:
        return {
            "orientation": self.orientation,
            "points_xy": [list(point) for point in self.points_xy],
            "bbox": list(self.bbox),
            "total_cost": self.total_cost,
        }


def _seam_cost(
    existing_color: np.ndarray,
    new_color: np.ndarray,
    existing_mask: np.ndarray,
    new_mask: np.ndarray,
    config: BlendConfig,
) -> np.ndarray:
    existing = existing_color.astype(np.float32)
    new = new_color.astype(np.float32)
    color_difference = np.mean(np.abs(existing - new), axis=2)
    gray_existing = cv2.cvtColor(existing_color, cv2.COLOR_BGR2GRAY)
    gray_new = cv2.cvtColor(new_color, cv2.COLOR_BGR2GRAY)
    # OpenCV 5.1's cv2.magnitude can vary by one ULP across calls in this
    # environment. NumPy's per-pixel square root keeps seam decisions stable.
    existing_x = cv2.Sobel(gray_existing, cv2.CV_32F, 1, 0, ksize=3)
    existing_y = cv2.Sobel(gray_existing, cv2.CV_32F, 0, 1, ksize=3)
    new_x = cv2.Sobel(gray_new, cv2.CV_32F, 1, 0, ksize=3)
    new_y = cv2.Sobel(gray_new, cv2.CV_32F, 0, 1, ksize=3)
    grad_existing = np.sqrt(existing_x * existing_x + existing_y * existing_y)
    grad_new = np.sqrt(new_x * new_x + new_y * new_y)
    gradient = np.abs(grad_existing - grad_new)
    cost = (
        config.seam_color_weight * color_difference
        + config.seam_gradient_weight * gradient
    ).astype(np.float64)
    valid = existing_mask & new_mask
    cost[~valid] = np.inf
    return cost


def _vertical_path(cost: np.ndarray) -> tuple[list[tuple[int, int]], float]:
    height, width = cost.shape
    dp = np.full_like(cost, np.inf, dtype=np.float64)
    parent = np.full((height, width), -1, dtype=np.int32)
    dp[0] = cost[0]
    for y in range(1, height):
        previous = dp[y - 1]
        left = np.full(width, np.inf)
        left[1:] = previous[:-1]
        middle = previous
        right = np.full(width, np.inf)
        right[:-1] = previous[1:]
        stacked = np.vstack([left, middle, right])
        choice = np.argmin(stacked, axis=0)
        parent[y] = choice - 1
        best_previous = stacked[choice, np.arange(width)]
        dp[y] = cost[y] + best_previous
    end_x = int(np.argmin(dp[-1]))
    if not np.isfinite(dp[-1, end_x]):
        return [], float("inf")
    points: list[tuple[int, int]] = []
    x = end_x
    for y in range(height - 1, -1, -1):
        points.append((x, y))
        x += int(parent[y, x])
    points.reverse()
    return points, float(dp[-1, end_x])


def find_minimum_cost_seam(
    existing_color: np.ndarray,
    new_color: np.ndarray,
    existing_mask: np.ndarray,
    new_mask: np.ndarray,
    config: BlendConfig,
) -> SeamPath | None:
    """Find a horizontal or vertical minimum-cost path through the overlap."""

    config.validate()
    overlap = existing_mask & new_mask
    if not np.any(overlap):
        return None
    ys, xs = np.nonzero(overlap)
    x0, x1 = int(np.min(xs)), int(np.max(xs)) + 1
    y0, y1 = int(np.min(ys)), int(np.max(ys)) + 1
    existing = existing_color[y0:y1, x0:x1]
    new = new_color[y0:y1, x0:x1]
    existing_crop = existing_mask[y0:y1, x0:x1]
    new_crop = new_mask[y0:y1, x0:x1]
    cost = _seam_cost(existing, new, existing_crop, new_crop, config)
    local_overlap = existing_crop & new_crop
    height, width = cost.shape

    if width >= height:
        rows = np.flatnonzero(np.any(local_overlap, axis=1))
        if len(rows) == 0:
            return None
        row0, row1 = int(rows[0]), int(rows[-1]) + 1
        local_points, total = _vertical_path(cost[row0:row1])
        if not local_points:
            return None
        points = [(x0 + x, y0 + row0 + y) for x, y in local_points]
        orientation: Literal["vertical", "horizontal"] = "vertical"
        bbox = (x0, y0 + row0, x1, y0 + row1)
    else:
        columns = np.flatnonzero(np.any(local_overlap, axis=0))
        if len(columns) == 0:
            return None
        col0, col1 = int(columns[0]), int(columns[-1]) + 1
        transposed = cost[:, col0:col1].T
        local_points, total = _vertical_path(transposed)
        if not local_points:
            return None
        points = [(x0 + col0 + y, y0 + x) for x, y in local_points]
        orientation = "horizontal"
        bbox = (x0 + col0, y0, x0 + col1, y1)
    return SeamPath(
        orientation=orientation,
        points_xy=tuple(points),
        bbox=bbox,
        total_cost=total,
    )


def fallback_straight_seam(
    existing_mask: np.ndarray,
    new_mask: np.ndarray,
) -> SeamPath | None:
    """Deterministic fallback when no finite overlap path exists.

    A straight split still guarantees that a newly registered image owns a
    deterministic part of the overlap instead of silently disappearing.
    """

    overlap = existing_mask & new_mask
    if not np.any(overlap):
        return None
    ys, xs = np.nonzero(overlap)
    x0, x1 = int(np.min(xs)), int(np.max(xs)) + 1
    y0, y1 = int(np.min(ys)), int(np.max(ys)) + 1
    if x1 - x0 >= y1 - y0:
        x = (x0 + x1) // 2
        points = tuple((x, y) for y in range(y0, y1))
        orientation: Literal["vertical", "horizontal"] = "vertical"
    else:
        y = (y0 + y1) // 2
        points = tuple((x, y) for x in range(x0, x1))
        orientation = "horizontal"
    return SeamPath(
        orientation=orientation,
        points_xy=points,
        bbox=(x0, y0, x1, y1),
        total_cost=1e18,
    )


def apply_seam_partition(
    owner: np.ndarray,
    new_index: int,
    new_mask: np.ndarray,
    seam: SeamPath | None,
) -> None:
    """Assign the new image on one side of the seam; untouched pixels stay."""

    owner[new_mask & (owner < 0)] = new_index
    if seam is None:
        return
    x0, y0, x1, y1 = seam.bbox
    height, width = owner.shape
    lookup: dict[int, int] = {}
    for x, y in seam.points_xy:
        lookup[y] = x
    if seam.orientation == "vertical":
        for y in range(max(0, y0), min(height, y1)):
            seam_x = lookup.get(y, (x0 + x1) // 2)
            row = owner[y]
            valid = new_mask[y] & (row >= 0)
            columns = np.flatnonzero(valid)
            if len(columns) == 0:
                continue
            selected = columns[columns >= seam_x]
            row[selected] = new_index
    else:
        lookup_y: dict[int, int] = {}
        for x, y in seam.points_xy:
            lookup_y[x] = y
        for x in range(max(0, x0), min(width, x1)):
            seam_y = lookup_y.get(x, (y0 + y1) // 2)
            column = owner[:, x]
            valid = new_mask[:, x] & (column >= 0)
            rows = np.flatnonzero(valid)
            if len(rows) == 0:
                continue
            selected = rows[rows >= seam_y]
            column[selected] = new_index
