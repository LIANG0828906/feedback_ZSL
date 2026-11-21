"""Dependency-free matrix and camera helpers used by the OpenGL front end."""

from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np


def perspective(
    fov_y_degrees: float,
    aspect: float,
    near: float,
    far: float,
) -> np.ndarray:
    fov = math.radians(float(fov_y_degrees))
    f = 1.0 / math.tan(fov * 0.5)
    matrix = np.zeros((4, 4), dtype=np.float64)
    matrix[0, 0] = f / max(aspect, 1e-9)
    matrix[1, 1] = f
    matrix[2, 2] = (far + near) / (near - far)
    matrix[2, 3] = (2.0 * far * near) / (near - far)
    matrix[3, 2] = -1.0
    return matrix


def look_at(
    eye: tuple[float, float, float] | np.ndarray,
    target: tuple[float, float, float] | np.ndarray,
    up: tuple[float, float, float] | np.ndarray = (0.0, 1.0, 0.0),
) -> np.ndarray:
    eye_array = np.asarray(eye, dtype=np.float64)
    target_array = np.asarray(target, dtype=np.float64)
    forward = target_array - eye_array
    norm = np.linalg.norm(forward)
    if norm < 1e-12:
        forward = np.asarray((0.0, 0.0, -1.0))
    else:
        forward = forward / norm
    up_array = np.asarray(up, dtype=np.float64)
    right = np.cross(forward, up_array)
    if np.linalg.norm(right) < 1e-12:
        right = np.cross(forward, np.asarray((1.0, 0.0, 0.0)))
    right = right / np.linalg.norm(right)
    true_up = np.cross(right, forward)
    matrix = np.eye(4, dtype=np.float64)
    matrix[0, :3] = right
    matrix[1, :3] = true_up
    matrix[2, :3] = -forward
    matrix[0, 3] = -float(np.dot(right, eye_array))
    matrix[1, 3] = -float(np.dot(true_up, eye_array))
    matrix[2, 3] = float(np.dot(forward, eye_array))
    return matrix


def invert(matrix: np.ndarray) -> np.ndarray:
    return np.linalg.inv(np.asarray(matrix, dtype=np.float64))


def project_clip(view_projection: np.ndarray, xyz: np.ndarray) -> np.ndarray:
    points = np.asarray(xyz, dtype=np.float64)
    homogeneous = np.concatenate(
        [points, np.ones((points.shape[0], 1), dtype=np.float64)], axis=1
    )
    clip = homogeneous @ np.asarray(view_projection, dtype=np.float64).T
    return clip


def project_ndc(view_projection: np.ndarray, xyz: np.ndarray) -> np.ndarray:
    clip = project_clip(view_projection, xyz)
    with np.errstate(divide="ignore", invalid="ignore"):
        ndc = clip[:, :3] / clip[:, 3:4]
    return ndc


def horizontal_fov(fov_y_degrees: float, aspect: float) -> float:
    return 2.0 * math.atan(math.tan(math.radians(fov_y_degrees) * 0.5) * aspect)


@dataclass
class OrbitCamera:
    target: tuple[float, float, float] = (0.0, 0.0, 0.0)
    distance: float = 5.0
    yaw_degrees: float = 35.0
    pitch_degrees: float = 22.0
    fov_y_degrees: float = 45.0
    near: float = 0.01
    far: float = 10000.0

    def set_scene(self, center: tuple[float, float, float], diagonal: float) -> None:
        self.target = center
        self.distance = max(diagonal * 1.35, 1.0)
        self.near = max(self.distance / 1000.0, 0.001)
        self.far = max(self.distance * 100.0, 100.0)

    @property
    def eye(self) -> tuple[float, float, float]:
        yaw = math.radians(self.yaw_degrees)
        pitch = math.radians(self.pitch_degrees)
        offset = np.asarray(
            (
                math.cos(pitch) * math.sin(yaw),
                math.sin(pitch),
                math.cos(pitch) * math.cos(yaw),
            ),
            dtype=np.float64,
        )
        eye = np.asarray(self.target, dtype=np.float64) + offset * self.distance
        return tuple(float(v) for v in eye)

    def view_matrix(self) -> np.ndarray:
        return look_at(self.eye, self.target)

    def projection_matrix(self, width: int, height: int) -> np.ndarray:
        aspect = max(width, 1) / max(height, 1)
        return perspective(
            self.fov_y_degrees,
            aspect,
            max(self.near, 1e-5),
            max(self.far, self.near * 10.0),
        )

    def view_projection(self, width: int, height: int) -> np.ndarray:
        return self.projection_matrix(width, height) @ self.view_matrix()

    def orbit(self, delta_x: float, delta_y: float) -> None:
        self.yaw_degrees = (self.yaw_degrees + delta_x * 0.35) % 360.0
        self.pitch_degrees = float(
            np.clip(self.pitch_degrees + delta_y * 0.35, -89.0, 89.0)
        )

    def zoom(self, scroll_steps: float) -> None:
        factor = math.pow(1.12, -float(scroll_steps))
        self.distance = float(np.clip(self.distance * factor, 0.01, 1e7))

    def screen_world(
        self,
        x_pixels: float,
        y_pixels: float,
        width: int,
        height: int,
    ) -> tuple[float, float, float]:
        """Intersect a screen ray with the plane through the orbit target."""

        if width <= 0 or height <= 0:
            return self.target
        ndc_x = (2.0 * x_pixels / width) - 1.0
        ndc_y = 1.0 - (2.0 * y_pixels / height)
        inverse = invert(self.view_projection(width, height))
        near_point = inverse @ np.asarray((ndc_x, ndc_y, -1.0, 1.0))
        far_point = inverse @ np.asarray((ndc_x, ndc_y, 1.0, 1.0))
        near_point = near_point[:3] / near_point[3]
        far_point = far_point[:3] / far_point[3]
        eye = np.asarray(self.eye, dtype=np.float64)
        direction = far_point - near_point
        direction_norm = np.linalg.norm(direction)
        if direction_norm < 1e-12:
            return self.target
        direction /= direction_norm
        plane_normal = np.asarray(self.target, dtype=np.float64) - eye
        normal_norm = np.linalg.norm(plane_normal)
        if normal_norm < 1e-12:
            return self.target
        plane_normal /= normal_norm
        denominator = float(np.dot(direction, plane_normal))
        if abs(denominator) < 1e-9:
            return self.target
        distance = (
            float(np.dot(np.asarray(self.target) - near_point, plane_normal))
            / denominator
        )
        intersection = near_point + direction * distance
        return tuple(float(v) for v in intersection)


def matrix_for_uniform(matrix: np.ndarray) -> np.ndarray:
    """Return a tightly packed float32 row-major matrix."""

    return np.ascontiguousarray(matrix, dtype=np.float32)
