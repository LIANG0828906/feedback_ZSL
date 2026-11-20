from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass
from typing import Any, Sequence

import numpy as np

from .errors import E_INVALID_ARGUMENT, TerrainError


def _vec3(value: Sequence[float], name: str) -> np.ndarray:
    array = np.asarray(value, dtype=np.float64)
    if array.shape != (3,) or not np.isfinite(array).all():
        raise TerrainError(
            E_INVALID_ARGUMENT, f"{name} must be a finite 3-vector", value=list(value)
        )
    return array


@dataclass(frozen=True)
class Camera:
    eye: tuple[float, float, float]
    target: tuple[float, float, float]
    up: tuple[float, float, float] = (0.0, 1.0, 0.0)
    fov_y_degrees: float = 45.0
    width: int = 96
    height: int = 64
    near: float = 0.05
    far: float = 20000.0

    def __post_init__(self) -> None:
        eye = _vec3(self.eye, "eye")
        target = _vec3(self.target, "target")
        up = _vec3(self.up, "up")
        if np.linalg.norm(target - eye) < 1e-9:
            raise TerrainError(E_INVALID_ARGUMENT, "camera eye and target must differ")
        if not (0.0 < self.fov_y_degrees < 179.0):
            raise TerrainError(
                E_INVALID_ARGUMENT,
                "vertical field of view must be between 0 and 179 degrees",
                fov_y_degrees=self.fov_y_degrees,
            )
        if self.width < 1 or self.height < 1:
            raise TerrainError(E_INVALID_ARGUMENT, "viewport dimensions must be positive")
        if self.near <= 0.0 or self.far <= self.near:
            raise TerrainError(
                E_INVALID_ARGUMENT,
                "camera near/far planes are invalid",
                near=self.near,
                far=self.far,
            )

    @property
    def tan_half_fov(self) -> float:
        return math.tan(math.radians(self.fov_y_degrees) * 0.5)

    @property
    def aspect(self) -> float:
        return self.width / self.height

    def axes(self) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        eye = np.asarray(self.eye, dtype=np.float64)
        target = np.asarray(self.target, dtype=np.float64)
        desired_up = np.asarray(self.up, dtype=np.float64)
        forward = target - eye
        forward /= np.linalg.norm(forward)
        right = np.cross(forward, desired_up)
        if np.linalg.norm(right) < 1e-12:
            raise TerrainError(E_INVALID_ARGUMENT, "camera up vector is parallel to view")
        right /= np.linalg.norm(right)
        true_up = np.cross(right, forward)
        true_up /= np.linalg.norm(true_up)
        return right, true_up, forward

    def view_matrix(self) -> np.ndarray:
        right, true_up, forward = self.axes()
        eye = np.asarray(self.eye, dtype=np.float64)
        matrix = np.eye(4, dtype=np.float64)
        matrix[0, :3] = right
        matrix[1, :3] = true_up
        matrix[2, :3] = forward
        matrix[0, 3] = -float(np.dot(right, eye))
        matrix[1, 3] = -float(np.dot(true_up, eye))
        matrix[2, 3] = -float(np.dot(forward, eye))
        return matrix

    def to_dict(self) -> dict[str, Any]:
        return {
            "eye": [float(v) for v in self.eye],
            "target": [float(v) for v in self.target],
            "up": [float(v) for v in self.up],
            "fov_y_degrees": float(self.fov_y_degrees),
            "width": int(self.width),
            "height": int(self.height),
            "near": float(self.near),
            "far": float(self.far),
            "view_matrix": self.view_matrix().round(12).tolist(),
        }

    def signature(self) -> str:
        encoded = repr(self.to_dict()).encode("utf-8")
        return hashlib.sha256(encoded).hexdigest()

    def project(
        self, points: np.ndarray
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
        points = np.asarray(points, dtype=np.float64)
        view = self.view_matrix()
        homogeneous = np.concatenate(
            [points, np.ones((points.shape[0], 1), dtype=np.float64)], axis=1
        )
        view_points = homogeneous @ view.T
        z = view_points[:, 2]
        safe_z = np.where(np.abs(z) < 1e-12, np.copysign(1e-12, z), z)
        ndc_x = view_points[:, 0] / (safe_z * self.tan_half_fov * self.aspect)
        ndc_y = view_points[:, 1] / (safe_z * self.tan_half_fov)
        screen_x = (ndc_x + 1.0) * 0.5 * (self.width - 1)
        screen_y = (1.0 - ndc_y) * 0.5 * (self.height - 1)
        visible = (z >= self.near) & (z <= self.far)
        return screen_x, screen_y, z, visible

    def projected_vertical_error_pixels(
        self, world_error: float, distance_to_point: float
    ) -> float:
        distance = max(float(distance_to_point), self.near)
        focal_pixels = self.height / (2.0 * self.tan_half_fov)
        return float(world_error) * focal_pixels / distance

    def point_distance(self, point: Sequence[float]) -> float:
        eye = np.asarray(self.eye, dtype=np.float64)
        return float(np.linalg.norm(np.asarray(point, dtype=np.float64) - eye))

