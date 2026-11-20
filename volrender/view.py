"""Deterministic orthographic camera definition used by the ray caster."""

from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
import math
from typing import Sequence

import numpy as np


def _basis(yaw_deg: float, pitch_deg: float, roll_deg: float) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    yaw = math.radians(yaw_deg)
    pitch = math.radians(pitch_deg)
    roll = math.radians(roll_deg)
    direction = np.asarray(
        [
            math.sin(yaw) * math.cos(pitch),
            math.sin(pitch),
            math.cos(yaw) * math.cos(pitch),
        ],
        dtype=np.float64,
    )
    direction /= np.linalg.norm(direction)
    world_up = np.asarray([0.0, 1.0, 0.0], dtype=np.float64)
    if abs(float(np.dot(world_up, direction))) > 0.995:
        world_up = np.asarray([0.0, 0.0, 1.0], dtype=np.float64)
    right = np.cross(world_up, direction)
    right /= np.linalg.norm(right)
    up = np.cross(direction, right)
    up /= np.linalg.norm(up)
    if roll:
        cos_r = math.cos(roll)
        sin_r = math.sin(roll)
        rolled_right = cos_r * right + sin_r * up
        rolled_up = -sin_r * right + cos_r * up
        right, up = rolled_right, rolled_up
    return direction, right, up


def hash_view_matrix(matrix: Sequence[Sequence[float]] | np.ndarray) -> str:
    canonical = [
        [float(f"{float(value):.15g}") for value in row]
        for row in np.asarray(matrix, dtype=np.float64)
    ]
    payload = json.dumps(canonical, separators=(",", ":"))
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()[:16]


@dataclass(frozen=True)
class CameraView:
    yaw_deg: float = 35.0
    pitch_deg: float = 24.0
    roll_deg: float = 0.0
    zoom: float = 1.0

    def __post_init__(self) -> None:
        if not all(math.isfinite(v) for v in (self.yaw_deg, self.pitch_deg, self.roll_deg, self.zoom)):
            raise ValueError("view parameters must be finite")
        if self.zoom <= 0.0:
            raise ValueError("zoom must be positive")

    @property
    def direction(self) -> np.ndarray:
        return _basis(self.yaw_deg, self.pitch_deg, self.roll_deg)[0]

    @property
    def right(self) -> np.ndarray:
        return _basis(self.yaw_deg, self.pitch_deg, self.roll_deg)[1]

    @property
    def up(self) -> np.ndarray:
        return _basis(self.yaw_deg, self.pitch_deg, self.roll_deg)[2]

    @property
    def matrix(self) -> np.ndarray:
        direction, right, up = _basis(self.yaw_deg, self.pitch_deg, self.roll_deg)
        matrix = np.eye(4, dtype=np.float64)
        matrix[:3, 0] = right
        matrix[:3, 1] = up
        matrix[:3, 2] = -direction
        matrix[:3, 3] = direction * 2.0
        return matrix

    @property
    def matrix_list(self) -> list[list[float]]:
        return [[float(value) for value in row] for row in self.matrix]

    @property
    def hash(self) -> str:
        return hash_view_matrix(self.matrix)

    @classmethod
    def from_matrix(cls, matrix: Sequence[Sequence[float]] | np.ndarray) -> "CameraView":
        # The matrix is stored in snapshots for exact change detection.  This
        # constructor recovers the equivalent camera axes when a caller already
        # knows the angles; it is intentionally conservative and preserves the
        # matrix by constructing a view from its rotation direction.
        arr = np.asarray(matrix, dtype=np.float64)
        if arr.shape != (4, 4):
            raise ValueError("view matrix must be 4x4")
        direction = -arr[:3, 2]
        direction /= np.linalg.norm(direction)
        yaw = math.degrees(math.atan2(direction[0], direction[2]))
        pitch = math.degrees(math.asin(max(-1.0, min(1.0, direction[1]))))
        return cls(yaw_deg=yaw, pitch_deg=pitch)


__all__ = ["CameraView", "hash_view_matrix"]

