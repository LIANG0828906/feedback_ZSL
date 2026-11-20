"""Deterministic orbit camera with yaw/pitch/zoom interaction."""

from __future__ import annotations

import math

import numpy as np


def _perspective(fovy_rad: float, aspect: float, near: float,
                 far: float) -> np.ndarray:
    f = 1.0 / math.tan(fovy_rad / 2.0)
    proj = np.zeros((4, 4), dtype=np.float64)
    proj[0, 0] = f / aspect
    proj[1, 1] = f
    proj[2, 2] = (far + near) / (near - far)
    proj[2, 3] = (2.0 * far * near) / (near - far)
    proj[3, 2] = -1.0
    return proj


def _look_at(eye: np.ndarray, target: np.ndarray, up: np.ndarray) -> np.ndarray:
    forward = target - eye
    forward /= np.linalg.norm(forward)
    right = np.cross(forward, up)
    right /= np.linalg.norm(right)
    true_up = np.cross(right, forward)
    view = np.eye(4, dtype=np.float64)
    view[0, :3] = right
    view[1, :3] = true_up
    view[2, :3] = -forward
    view[0, 3] = -np.dot(right, eye)
    view[1, 3] = -np.dot(true_up, eye)
    view[2, 3] = np.dot(forward, eye)
    return view


class OrbitCamera:
    """Orbit camera: yaw/pitch rotation, distance zoom, fixed target."""

    def __init__(self, target=(0.0, 0.0, 0.0), distance: float = 10.0,
                 yaw: float = 0.6, pitch: float = 0.35,
                 fovy_deg: float = 55.0, near: float = 0.05,
                 far: float = 1000.0):
        self.target = np.asarray(target, dtype=np.float64)
        self.distance = float(distance)
        self.yaw = float(yaw)
        self.pitch = float(pitch)
        self.fovy = math.radians(fovy_deg)
        self.near = float(near)
        self.far = float(far)

    def rotate(self, delta_yaw: float, delta_pitch: float) -> None:
        self.yaw += float(delta_yaw)
        limit = math.pi / 2.0 - 1e-4
        self.pitch = float(np.clip(self.pitch + delta_pitch, -limit, limit))

    def zoom(self, factor: float) -> None:
        self.distance = max(1e-3, self.distance / float(factor))

    def fit(self, bbox: np.ndarray) -> None:
        bbox = np.asarray(bbox, dtype=np.float64).reshape(6)
        center = (bbox[:3] + bbox[3:]) / 2.0
        extent = float(np.max(bbox[3:] - bbox[:3]))
        self.target = center
        self.distance = max(extent * 1.6, 1.0)

    def eye(self) -> np.ndarray:
        cp = math.cos(self.pitch)
        offset = np.array([
            self.distance * cp * math.sin(self.yaw),
            self.distance * math.sin(self.pitch),
            self.distance * cp * math.cos(self.yaw),
        ])
        return self.target + offset

    def view_matrix(self) -> np.ndarray:
        return _look_at(self.eye(), self.target, np.array([0.0, 1.0, 0.0]))

    def view_proj(self, width: int, height: int) -> np.ndarray:
        aspect = width / max(1, height)
        return _perspective(self.fovy, aspect, self.near,
                            self.far) @ self.view_matrix()
