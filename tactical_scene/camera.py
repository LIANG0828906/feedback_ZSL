"""Camera representation and deterministic look-at/orthographic helpers."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import ErrorCode, TacticalError


def _matrix_tuple(value: Any) -> tuple[float, ...]:
    array = np.asarray(value, dtype=np.float64)
    if array.shape == (16,):
        array = array.reshape(4, 4)
    if array.shape != (4, 4):
        raise TacticalError(
            ErrorCode.INVALID_ARGUMENT,
            "matrix must have shape (4, 4)",
            {"shape": list(array.shape)},
        )
    if not np.isfinite(array).all():
        raise TacticalError(ErrorCode.INVALID_ARGUMENT, "matrix contains non-finite values")
    return tuple(float(v) for v in array.reshape(-1))


def _canonical_float(value: float) -> str:
    value = float(value)
    if value == 0.0:
        value = 0.0
    return format(value, ".17g")


@dataclass(frozen=True, slots=True)
class Camera:
    """A row-major view/projection pair plus a top-left pixel viewport.

    Positive camera depth is ``-z_view``.  The near plane is at depth ``near``
    and the far plane at depth ``far``.
    """

    view: tuple[float, ...]
    projection: tuple[float, ...]
    width: int
    height: int
    near: float = 0.1
    far: float = 10_000.0

    def __post_init__(self) -> None:
        object.__setattr__(self, "near", float(self.near))
        object.__setattr__(self, "far", float(self.far))
        object.__setattr__(self, "width", int(self.width))
        object.__setattr__(self, "height", int(self.height))
        if self.width <= 0 or self.height <= 0:
            raise TacticalError(
                ErrorCode.INVALID_ARGUMENT,
                "viewport dimensions must be positive",
                {"width": self.width, "height": self.height},
            )
        if not (math.isfinite(self.near) and math.isfinite(self.far)):
            raise TacticalError(ErrorCode.INVALID_ARGUMENT, "near/far must be finite")
        if self.near <= 0.0 or self.far <= self.near:
            raise TacticalError(
                ErrorCode.INVALID_ARGUMENT,
                "camera must satisfy 0 < near < far",
                {"near": self.near, "far": self.far},
            )
        view = np.asarray(self.view, dtype=np.float64).reshape(4, 4)
        projection = np.asarray(self.projection, dtype=np.float64).reshape(4, 4)
        if not np.isfinite(view).all() or not np.isfinite(projection).all():
            raise TacticalError(ErrorCode.INVALID_ARGUMENT, "camera matrices must be finite")
        determinant = abs(float(np.linalg.det(view)))
        if not math.isfinite(determinant) or determinant < 1e-12:
            raise TacticalError(
                ErrorCode.INVALID_ARGUMENT,
                "view matrix is not invertible",
                {"determinant": determinant},
            )

    @property
    def view_array(self) -> np.ndarray:
        return np.asarray(self.view, dtype=np.float64).reshape(4, 4)

    @property
    def projection_array(self) -> np.ndarray:
        return np.asarray(self.projection, dtype=np.float64).reshape(4, 4)

    @property
    def viewport(self) -> tuple[float, float, float, float]:
        return (0.0, 0.0, float(self.width), float(self.height))

    def key(self) -> str:
        payload = {
            "view": [_canonical_float(v) for v in self.view],
            "projection": [_canonical_float(v) for v in self.projection],
            "width": self.width,
            "height": self.height,
            "near": _canonical_float(self.near),
            "far": _canonical_float(self.far),
        }
        encoded = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
        return hashlib.sha256(encoded).hexdigest()

    def to_dict(self) -> dict[str, Any]:
        return {
            "view": list(self.view),
            "projection": list(self.projection),
            "width": self.width,
            "height": self.height,
            "near": self.near,
            "far": self.far,
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "Camera":
        try:
            return cls(
                view=_matrix_tuple(value["view"]),
                projection=_matrix_tuple(value["projection"]),
                width=int(value["width"]),
                height=int(value["height"]),
                near=float(value["near"]),
                far=float(value["far"]),
            )
        except KeyError as exc:
            raise TacticalError(
                ErrorCode.INVALID_ARGUMENT,
                f"camera field missing: {exc.args[0]}",
            ) from exc

    @classmethod
    def look_at(
        cls,
        eye: Any,
        target: Any,
        up: Any,
        *,
        width: int,
        height: int,
        fov_y_degrees: float = 50.0,
        near: float = 0.1,
        far: float = 10_000.0,
    ) -> "Camera":
        eye_array = np.asarray(eye, dtype=np.float64)
        target_array = np.asarray(target, dtype=np.float64)
        up_array = np.asarray(up, dtype=np.float64)
        forward = target_array - eye_array
        forward_norm = float(np.linalg.norm(forward))
        if forward_norm < 1e-12:
            raise TacticalError(ErrorCode.INVALID_ARGUMENT, "eye and target must differ")
        forward /= forward_norm
        side = np.cross(forward, up_array)
        side_norm = float(np.linalg.norm(side))
        if side_norm < 1e-12:
            raise TacticalError(ErrorCode.INVALID_ARGUMENT, "up vector is parallel to view direction")
        side /= side_norm
        true_up = np.cross(side, forward)
        view = np.eye(4, dtype=np.float64)
        view[0, :3] = side
        view[1, :3] = true_up
        view[2, :3] = -forward
        view[0, 3] = -float(np.dot(side, eye_array))
        view[1, 3] = -float(np.dot(true_up, eye_array))
        view[2, 3] = float(np.dot(forward, eye_array))

        aspect = float(width) / float(height)
        focal = 1.0 / math.tan(math.radians(fov_y_degrees) * 0.5)
        projection = np.zeros((4, 4), dtype=np.float64)
        projection[0, 0] = focal / aspect
        projection[1, 1] = focal
        projection[2, 2] = (far + near) / (near - far)
        projection[2, 3] = (2.0 * far * near) / (near - far)
        projection[3, 2] = -1.0
        return cls(
            view=tuple(float(v) for v in view.reshape(-1)),
            projection=tuple(float(v) for v in projection.reshape(-1)),
            width=width,
            height=height,
            near=near,
            far=far,
        )

    @classmethod
    def orthographic(
        cls,
        *,
        left: float,
        right: float,
        bottom: float,
        top: float,
        width: int,
        height: int,
        near: float = 0.1,
        far: float = 10_000.0,
    ) -> "Camera":
        """Orthographic camera looking down world +Z.

        Camera-space depth is ``far - z`` and screen ``y`` grows downward.
        This helper is useful for exact coverage tests.
        """

        if not (right > left and top > bottom):
            raise TacticalError(ErrorCode.INVALID_ARGUMENT, "invalid orthographic extents")
        view = np.eye(4, dtype=np.float64)
        view[2, 2] = 1.0
        view[2, 3] = -far
        projection = np.eye(4, dtype=np.float64)
        projection[0, 0] = 2.0 / (right - left)
        projection[0, 3] = -(right + left) / (right - left)
        projection[1, 1] = 2.0 / (top - bottom)
        projection[1, 3] = -(top + bottom) / (top - bottom)
        projection[2, 2] = -2.0 / (far - near)
        projection[2, 3] = -(far + near) / (far - near)
        return cls(
            view=tuple(float(v) for v in view.reshape(-1)),
            projection=tuple(float(v) for v in projection.reshape(-1)),
            width=width,
            height=height,
            near=near,
            far=far,
        )
