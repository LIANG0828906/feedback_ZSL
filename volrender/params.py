"""Immutable render parameters and their content-addressed identity."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass, replace
from typing import Any, Iterable, Mapping, Sequence

import numpy as np

from .errors import ErrorCode, VolumeRenderError
from .transfer import ControlPoint, validate_control_points
from .volume import validate_window

PARAM_SCHEMA = "volrender-params-v1"


def _finite(value: float, field: str) -> float:
    try:
        result = float(value)
    except (TypeError, ValueError) as exc:
        raise VolumeRenderError(ErrorCode.E_TF_INVALID, "parameter is not numeric", field=field) from exc
    if not math.isfinite(result):
        raise VolumeRenderError(ErrorCode.E_TF_INVALID, "parameter must be finite", field=field)
    return result


def _float_hex(value: float) -> str:
    return float(value).hex()


@dataclass(frozen=True)
class RenderParameters:
    """All inputs that can influence an offscreen image.

    ``view_matrix`` is a row-major world-to-camera matrix flattened to sixteen
    values.  The renderer's documented camera convention is in the README.
    """

    volume_hash: str
    window_center: float
    window_width: float
    control_points: tuple[ControlPoint, ...]
    step_size: float
    view_matrix: tuple[float, ...]
    viewport: tuple[int, int]
    mask_enabled: bool = False
    mask_hash: str = ""

    def __post_init__(self) -> None:
        if (
            not isinstance(self.volume_hash, str)
            or len(self.volume_hash) != 64
            or any(character not in "0123456789abcdef" for character in self.volume_hash)
        ):
            raise VolumeRenderError(
                ErrorCode.E_TF_INVALID,
                "volume_hash must be a 64 character SHA-256 digest",
                field="volume_hash",
            )
        center, width = validate_window(self.window_center, self.window_width)
        object.__setattr__(self, "window_center", center)
        object.__setattr__(self, "window_width", width)
        object.__setattr__(self, "control_points", validate_control_points(self.control_points))
        step = _finite(self.step_size, "step_size")
        if step <= 0.0:
            raise VolumeRenderError(ErrorCode.E_TF_INVALID, "step_size must be positive", field="step_size")
        object.__setattr__(self, "step_size", step)

        matrix = tuple(_finite(value, f"view_matrix[{index}]") for index, value in enumerate(self.view_matrix))
        if len(matrix) != 16:
            raise VolumeRenderError(
                ErrorCode.E_TF_INVALID,
                "view_matrix must contain sixteen values",
                field="view_matrix",
                details={"actual": len(matrix)},
            )
        determinant = float(np.linalg.det(np.asarray(matrix, dtype=np.float64).reshape(4, 4)))
        if not math.isfinite(determinant) or abs(determinant) < 1e-12:
            raise VolumeRenderError(
                ErrorCode.E_TF_INVALID,
                "view_matrix must be invertible",
                field="view_matrix",
            )
        object.__setattr__(self, "view_matrix", matrix)

        if len(self.viewport) != 2:
            raise VolumeRenderError(
                ErrorCode.E_TF_INVALID,
                "viewport must contain width and height",
                field="viewport",
            )
        width_px, height_px = int(self.viewport[0]), int(self.viewport[1])
        if width_px <= 0 or height_px <= 0:
            raise VolumeRenderError(
                ErrorCode.E_TF_INVALID,
                "viewport dimensions must be positive",
                field="viewport",
            )
        object.__setattr__(self, "viewport", (width_px, height_px))
        object.__setattr__(self, "mask_enabled", bool(self.mask_enabled))
        if self.mask_enabled:
            if not self.mask_hash or len(self.mask_hash) != 64:
                raise VolumeRenderError(
                    ErrorCode.E_MASK_INVALID,
                    "mask_hash must be a SHA-256 digest when mask overlay is enabled",
                    field="mask_hash",
                )
        elif self.mask_hash:
            raise VolumeRenderError(
                ErrorCode.E_MASK_INVALID,
                "mask_hash must be empty when mask overlay is disabled",
                field="mask_hash",
            )

    @property
    def parameter_hash(self) -> str:
        return hashlib.sha256(self.canonical_bytes()).hexdigest()

    @property
    def param_hash(self) -> str:
        return self.parameter_hash

    def canonical_dict(self) -> dict[str, Any]:
        return {
            "schema": PARAM_SCHEMA,
            "volume_hash": self.volume_hash,
            "window_center": _float_hex(self.window_center),
            "window_width": _float_hex(self.window_width),
            "control_points": [
                {
                    "position": _float_hex(point.position),
                    "opacity": _float_hex(point.opacity),
                    "color": [_float_hex(channel) for channel in point.color],
                }
                for point in self.control_points
            ],
            "step_size": _float_hex(self.step_size),
            "view_matrix": [_float_hex(value) for value in self.view_matrix],
            "viewport": [self.viewport[0], self.viewport[1]],
            "mask_enabled": self.mask_enabled,
            "mask_hash": self.mask_hash,
        }

    def canonical_bytes(self) -> bytes:
        return json.dumps(
            self.canonical_dict(),
            ensure_ascii=True,
            sort_keys=True,
            separators=(",", ":"),
            allow_nan=False,
        ).encode("utf-8")

    def to_snapshot_dict(self, include_hash: bool = True) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "volume_hash": self.volume_hash,
            "window_center": self.window_center,
            "window_width": self.window_width,
            "control_points": [point.to_dict() for point in self.control_points],
            "step_size": self.step_size,
            "view_matrix": [list(self.view_matrix[row * 4 : row * 4 + 4]) for row in range(4)],
            "viewport": [self.viewport[0], self.viewport[1]],
            "mask_enabled": self.mask_enabled,
            "mask_hash": self.mask_hash,
        }
        if include_hash:
            payload["parameter_hash"] = self.parameter_hash
        return payload

    @classmethod
    def from_snapshot_dict(cls, payload: Mapping[str, Any]) -> "RenderParameters":
        try:
            matrix_raw = payload["view_matrix"]
            if len(matrix_raw) == 16 and all(not isinstance(value, (list, tuple)) for value in matrix_raw):
                matrix = tuple(float(value) for value in matrix_raw)
            else:
                matrix = tuple(float(value) for row in matrix_raw for value in row)
            viewport_raw = payload["viewport"]
            params = cls(
                volume_hash=str(payload["volume_hash"]),
                window_center=float(payload["window_center"]),
                window_width=float(payload["window_width"]),
                control_points=tuple(ControlPoint.coerce(point) for point in payload["control_points"]),
                step_size=float(payload["step_size"]),
                view_matrix=matrix,
                viewport=(int(viewport_raw[0]), int(viewport_raw[1])),
                mask_enabled=bool(payload.get("mask_enabled", False)),
                mask_hash=str(payload.get("mask_hash", "")),
            )
        except VolumeRenderError:
            raise
        except (KeyError, TypeError, ValueError, IndexError) as exc:
            raise VolumeRenderError(
                ErrorCode.E_SNAPSHOT_INVALID,
                "parameter payload is incomplete or malformed",
                field="params",
            ) from exc
        expected_hash = payload.get("parameter_hash")
        if expected_hash is not None and expected_hash != params.parameter_hash:
            raise VolumeRenderError(
                ErrorCode.E_SNAPSHOT_INVALID,
                "stored parameter hash does not match parameter contents",
                field="params.parameter_hash",
                details={"expected": params.parameter_hash, "actual": expected_hash},
            )
        return params

    def with_changes(self, **changes: Any) -> "RenderParameters":
        return replace(self, **changes)

    def describe_differences(self, other: "RenderParameters") -> list[str]:
        fields: list[str] = []
        scalar_fields = (
            "volume_hash",
            "window_center",
            "window_width",
            "step_size",
            "view_matrix",
            "viewport",
            "mask_enabled",
            "mask_hash",
        )
        for field in scalar_fields:
            if getattr(self, field) != getattr(other, field):
                fields.append(field)
        if [point.to_dict() for point in self.control_points] != [point.to_dict() for point in other.control_points]:
            fields.append("control_points")
        return fields


def default_view_matrix() -> tuple[float, ...]:
    return (
        1.0,
        0.0,
        0.0,
        0.0,
        0.0,
        1.0,
        0.0,
        0.0,
        0.0,
        0.0,
        1.0,
        0.0,
        0.0,
        0.0,
        0.0,
        1.0,
    )


def make_parameters(
    volume_hash: str,
    window_center: float,
    window_width: float,
    control_points: Iterable[ControlPoint | Mapping[str, object] | Sequence[object]],
    *,
    step_size: float = 0.01,
    view_matrix: Sequence[float] | Sequence[Sequence[float]] | None = None,
    viewport: tuple[int, int] = (192, 192),
    mask_enabled: bool = False,
    mask_hash: str = "",
) -> RenderParameters:
    if view_matrix is None:
        flat_matrix = default_view_matrix()
    elif len(view_matrix) == 4 and all(isinstance(row, (list, tuple)) for row in view_matrix):
        flat_matrix = tuple(float(value) for row in view_matrix for value in row)  # type: ignore[union-attr]
    else:
        flat_matrix = tuple(float(value) for value in view_matrix)  # type: ignore[arg-type]
    return RenderParameters(
        volume_hash=volume_hash,
        window_center=float(window_center),
        window_width=float(window_width),
        control_points=tuple(ControlPoint.coerce(point) for point in control_points),
        step_size=float(step_size),
        view_matrix=flat_matrix,
        viewport=(int(viewport[0]), int(viewport[1])),
        mask_enabled=bool(mask_enabled),
        mask_hash=mask_hash,
    )
