"""Piecewise-linear transfer function editing and LUT construction."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterable, Mapping, Sequence

import numpy as np

from .errors import ErrorCode, VolumeRenderError


def _unit_float(value: float, field: str) -> float:
    try:
        result = float(value)
    except (TypeError, ValueError) as exc:
        raise VolumeRenderError(ErrorCode.E_TF_INVALID, "control value is not numeric", field=field) from exc
    if not math.isfinite(result) or result < 0.0 or result > 1.0:
        raise VolumeRenderError(
            ErrorCode.E_TF_INVALID,
            "control value must be finite and inside [0, 1]",
            field=field,
            details={"actual": value},
        )
    return result


@dataclass(frozen=True)
class ControlPoint:
    position: float
    opacity: float
    color: tuple[float, float, float]

    def __post_init__(self) -> None:
        position = _unit_float(self.position, "position")
        opacity = _unit_float(self.opacity, "opacity")
        try:
            channels = tuple(float(channel) for channel in self.color)
        except (TypeError, ValueError) as exc:
            raise VolumeRenderError(ErrorCode.E_TF_INVALID, "color is not numeric", field="color") from exc
        if len(channels) != 3:
            raise VolumeRenderError(ErrorCode.E_TF_INVALID, "color must have three channels", field="color")
        checked = tuple(
            _unit_float(channel, ("color_r", "color_g", "color_b")[index])
            for index, channel in enumerate(channels)
        )
        object.__setattr__(self, "position", position)
        object.__setattr__(self, "opacity", opacity)
        object.__setattr__(self, "color", checked)

    def to_dict(self) -> dict[str, object]:
        return {
            "position": self.position,
            "opacity": self.opacity,
            "color": list(self.color),
        }

    @classmethod
    def coerce(cls, value: "ControlPoint | Mapping[str, object] | Sequence[object]") -> "ControlPoint":
        if isinstance(value, cls):
            return value
        if isinstance(value, Mapping):
            try:
                return cls(float(value["position"]), float(value["opacity"]), tuple(value["color"]))  # type: ignore[arg-type]
            except (KeyError, TypeError, ValueError) as exc:
                raise VolumeRenderError(
                    ErrorCode.E_TF_INVALID,
                    "control point mapping must contain position, opacity and color",
                    field="control_points",
                ) from exc
        if isinstance(value, Sequence) and not isinstance(value, (str, bytes)) and len(value) == 3:
            return cls(float(value[0]), float(value[1]), tuple(value[2]))  # type: ignore[arg-type]
        raise VolumeRenderError(
            ErrorCode.E_TF_INVALID,
            "control point must be a ControlPoint, mapping, or three-item sequence",
            field="control_points",
        )


DEFAULT_CONTROL_POINTS: tuple[ControlPoint, ...] = (
    ControlPoint(0.00, 0.00, (0.02, 0.05, 0.12)),
    ControlPoint(0.28, 0.02, (0.12, 0.35, 0.85)),
    ControlPoint(0.55, 0.18, (0.20, 0.85, 0.70)),
    ControlPoint(0.78, 0.55, (1.00, 0.76, 0.25)),
    ControlPoint(1.00, 0.92, (1.00, 0.97, 0.92)),
)


def validate_control_points(points: Iterable[ControlPoint | Mapping[str, object] | Sequence[object]]) -> tuple[ControlPoint, ...]:
    if isinstance(points, (str, bytes)):
        raise VolumeRenderError(ErrorCode.E_TF_INVALID, "control points must be a sequence", field="control_points")
    try:
        coerced = tuple(ControlPoint.coerce(point) for point in points)
    except TypeError as exc:
        raise VolumeRenderError(
            ErrorCode.E_TF_INVALID,
            "control points must be iterable",
            field="control_points",
        ) from exc
    if len(coerced) < 2:
        raise VolumeRenderError(
            ErrorCode.E_TF_INVALID,
            "at least two control points are required",
            field="control_points",
        )
    positions = [point.position for point in coerced]
    for index in range(1, len(coerced)):
        if positions[index] <= positions[index - 1]:
            raise VolumeRenderError(
                ErrorCode.E_TF_INVALID,
                "control point positions must be strictly increasing",
                field=f"control_points[{index}].position",
                details={"previous": positions[index - 1], "actual": positions[index]},
            )
    return coerced


def build_transfer_lut(
    points: Iterable[ControlPoint | Mapping[str, object] | Sequence[object]],
    size: int = 1024,
) -> tuple[np.ndarray, tuple[ControlPoint, ...]]:
    if size < 2:
        raise VolumeRenderError(ErrorCode.E_TF_INVALID, "LUT size must be at least 2", field="lut_size")
    valid = validate_control_points(points)
    positions = np.asarray([point.position for point in valid], dtype=np.float64)
    domain = np.linspace(0.0, 1.0, size, dtype=np.float64)
    lut = np.empty((size, 4), dtype=np.float32)
    for channel in range(3):
        values = np.asarray([point.color[channel] for point in valid], dtype=np.float64)
        lut[:, channel] = np.interp(domain, positions, values).astype(np.float32)
    alpha = np.asarray([point.opacity for point in valid], dtype=np.float64)
    lut[:, 3] = np.interp(domain, positions, alpha).astype(np.float32)
    return lut, valid


class TransferFunction:
    """Transactional TF editor: failed updates keep the previous valid LUT."""

    def __init__(self, points: Iterable[ControlPoint] = DEFAULT_CONTROL_POINTS, size: int = 1024) -> None:
        self._size = int(size)
        self._revision = 0
        self._control_points: tuple[ControlPoint, ...] = ()
        self._lut = np.zeros((0, 4), dtype=np.float32)
        self.update(points)

    @property
    def size(self) -> int:
        return self._size

    @property
    def revision(self) -> int:
        return self._revision

    @property
    def control_points(self) -> tuple[ControlPoint, ...]:
        return self._control_points

    @property
    def lut(self) -> np.ndarray:
        return self._lut.copy()

    def update(
        self, points: Iterable[ControlPoint | Mapping[str, object] | Sequence[object]]
    ) -> np.ndarray:
        new_lut, new_points = build_transfer_lut(points, self._size)
        self._lut = new_lut
        self._control_points = new_points
        self._revision += 1
        return self._lut.copy()

    def sample(self, mapped: np.ndarray) -> np.ndarray:
        clipped = np.clip(np.asarray(mapped, dtype=np.float32), 0.0, 1.0)
        indices = np.rint(clipped * (self._size - 1)).astype(np.int32)
        return self._lut[indices]
