"""Piecewise-linear transfer function with atomic LUT replacement."""

from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
import math
import threading
from typing import Iterable, Sequence

import numpy as np

from .errors import TransferFunctionError


@dataclass(frozen=True)
class ControlPoint:
    x: float
    r: float
    g: float
    b: float
    opacity: float

    def as_list(self) -> list[float]:
        return [self.x, self.r, self.g, self.b, self.opacity]


DEFAULT_CONTROL_POINTS: tuple[ControlPoint, ...] = (
    ControlPoint(0.0, 0.0, 0.0, 0.0, 0.0),
    ControlPoint(0.25, 0.2, 0.1, 0.05, 0.05),
    ControlPoint(0.5, 0.9, 0.6, 0.3, 0.35),
    ControlPoint(0.75, 1.0, 0.9, 0.7, 0.8),
    ControlPoint(1.0, 1.0, 1.0, 1.0, 1.0),
)


def _as_points(points: Iterable[ControlPoint | Sequence[float]]) -> list[ControlPoint]:
    result: list[ControlPoint] = []
    for item in points:
        if isinstance(item, ControlPoint):
            result.append(item)
        else:
            if len(item) != 5:
                raise TransferFunctionError(
                    "BAD_CONTROL_POINT_ARITY",
                    f"控制点必须包含 x,r,g,b,opacity 五个值: {item!r}",
                    field="control_points",
                )
            result.append(
                ControlPoint(*(float(value) for value in item))
            )
    return result


def validate_control_points(
    points: Iterable[ControlPoint | Sequence[float]],
) -> tuple[ControlPoint, ...]:
    items = _as_points(points)
    if len(items) < 2:
        raise TransferFunctionError(
            "TOO_FEW_CONTROL_POINTS",
            "传递函数至少需要两个控制点",
            field="control_points",
            offset=len(items),
        )
    previous_x: float | None = None
    for index, point in enumerate(items):
        values = (point.x, point.r, point.g, point.b, point.opacity)
        if not all(math.isfinite(value) for value in values):
            raise TransferFunctionError(
                "CONTROL_POINT_NON_FINITE",
                f"控制点 {index} 含非有限值: {point.as_list()!r}",
                field=f"control_points[{index}]",
                offset=index,
            )
        if not 0.0 <= point.x <= 1.0:
            raise TransferFunctionError(
                "CONTROL_POINT_OUT_OF_RANGE",
                f"控制点 {index} 的 x={point.x} 越出 [0,1]",
                field=f"control_points[{index}]",
                offset=index,
            )
        for channel, value in (("r", point.r), ("g", point.g), ("b", point.b), ("opacity", point.opacity)):
            if not 0.0 <= value <= 1.0:
                raise TransferFunctionError(
                    "CONTROL_POINT_OUT_OF_RANGE",
                    f"控制点 {index} 的 {channel}={value} 越出 [0,1]",
                    field=f"control_points[{index}].{channel}",
                    offset=index,
                )
        if previous_x is not None and point.x <= previous_x:
            raise TransferFunctionError(
                "CONTROL_POINT_NOT_MONOTONIC",
                f"控制点 x 必须严格递增: {previous_x} -> {point.x} (索引 {index})",
                field=f"control_points[{index}]",
                offset=index,
            )
        previous_x = point.x
    return tuple(items)


class TransferFunction:
    """A validated piecewise-linear color/opacity mapping.

    Edits are transactional: validation and LUT construction happen against a
    local candidate, then all public fields are swapped in one critical section.
    """

    def __init__(
        self,
        points: Iterable[ControlPoint | Sequence[float]] = DEFAULT_CONTROL_POINTS,
        *,
        resolution: int = 256,
    ) -> None:
        if resolution < 2:
            raise TransferFunctionError(
                "BAD_LUT_RESOLUTION",
                f"LUT 分辨率必须 >= 2: {resolution}",
                field="lut_resolution",
            )
        validated = validate_control_points(points)
        lut, bounds = self._build(validated, resolution)
        self.resolution = int(resolution)
        self._points = validated
        self._lut = lut
        self._lut_float = lut.astype(np.float32) / np.float32(255.0)
        self._bounds = bounds
        self._lock = threading.RLock()

    @staticmethod
    def _build(
        points: tuple[ControlPoint, ...], resolution: int
    ) -> tuple[np.ndarray, list[dict[str, object]]]:
        xs = np.asarray([point.x for point in points], dtype=np.float64)
        values = np.asarray(
            [[point.r, point.g, point.b, point.opacity] for point in points],
            dtype=np.float64,
        )
        grid = np.linspace(0.0, 1.0, resolution, dtype=np.float64)
        channels = np.empty((resolution, 4), dtype=np.float64)
        for channel in range(4):
            channels[:, channel] = np.interp(grid, xs, values[:, channel])
        lut = np.rint(np.clip(channels, 0.0, 1.0) * 255.0).astype(np.uint8)
        bounds: list[dict[str, object]] = []
        for left, right in zip(points[:-1], points[1:]):
            # Two half-step 8-bit quantization contributions over the interval.
            opacity_error = abs(right.opacity - left.opacity) * (2.0 / 255.0)
            bounds.append(
                {
                    "interval": [left.x, right.x],
                    "opacity_error_bound": float(opacity_error),
                }
            )
        return lut, bounds

    @property
    def points(self) -> tuple[ControlPoint, ...]:
        return self._points

    @property
    def lut(self) -> np.ndarray:
        return self._lut.copy()

    @property
    def lut_float(self) -> np.ndarray:
        return self._lut_float

    @property
    def interval_error_bounds(self) -> list[dict[str, object]]:
        return [dict(item) for item in self._bounds]

    @property
    def max_opacity(self) -> float:
        return float(np.max(self._lut_float[:, 3]))

    def digest(self) -> str:
        payload = {
            "points": [point.as_list() for point in self._points],
            "resolution": self.resolution,
        }
        return hashlib.sha256(
            json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ).hexdigest()

    def replace(self, points: Iterable[ControlPoint | Sequence[float]]) -> "TransferFunction":
        validated = validate_control_points(points)
        lut, bounds = self._build(validated, self.resolution)
        lut_float = lut.astype(np.float32) / np.float32(255.0)
        # Commit only after every candidate operation has succeeded.
        with self._lock:
            self._points = validated
            self._lut = lut
            self._lut_float = lut_float
            self._bounds = bounds
        return self

    def set_control_point(self, index: int, point: ControlPoint | Sequence[float]) -> "TransferFunction":
        if not 0 <= index < len(self._points):
            raise TransferFunctionError(
                "CONTROL_POINT_INDEX_OUT_OF_RANGE",
                f"控制点索引 {index} 越界",
                field=f"control_points[{index}]",
                offset=index,
            )
        candidate = list(self._points)
        candidate[index] = _as_points([point])[0]
        return self.replace(candidate)

    def insert_control_point(
        self, index: int, point: ControlPoint | Sequence[float]
    ) -> "TransferFunction":
        if not 0 <= index <= len(self._points):
            raise TransferFunctionError(
                "CONTROL_POINT_INDEX_OUT_OF_RANGE",
                f"控制点索引 {index} 越界",
                field=f"control_points[{index}]",
                offset=index,
            )
        candidate = list(self._points)
        candidate.insert(index, _as_points([point])[0])
        return self.replace(candidate)

    def remove_control_point(self, index: int) -> "TransferFunction":
        if not 0 <= index < len(self._points):
            raise TransferFunctionError(
                "CONTROL_POINT_INDEX_OUT_OF_RANGE",
                f"控制点索引 {index} 越界",
                field=f"control_points[{index}]",
                offset=index,
            )
        candidate = list(self._points)
        candidate.pop(index)
        return self.replace(candidate)

    def sample(self, values: np.ndarray) -> np.ndarray:
        indices = np.rint(np.clip(values, 0.0, 1.0) * (self.resolution - 1)).astype(np.intp)
        return self._lut_float[indices]


__all__ = [
    "ControlPoint",
    "DEFAULT_CONTROL_POINTS",
    "TransferFunction",
    "validate_control_points",
]

