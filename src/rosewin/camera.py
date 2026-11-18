"""Pinhole camera and deterministic primary ray generation."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np


def _normalize(vector: np.ndarray) -> np.ndarray:
    return vector / np.linalg.norm(vector)


@dataclass(frozen=True)
class Camera:
    eye: np.ndarray
    forward: np.ndarray
    right: np.ndarray
    up: np.ndarray
    half_height: float
    aspect: float
    width: int
    height: int
    primary_samples: int

    @classmethod
    def from_scene(cls, scene) -> "Camera":
        camera_block = scene.camera
        eye = np.asarray(camera_block["eye"], dtype=np.float64)
        target = np.asarray(camera_block["target"], dtype=np.float64)
        world_up = np.asarray(camera_block["up"], dtype=np.float64)
        forward = _normalize(target - eye)
        right = _normalize(np.cross(forward, world_up))
        up = np.cross(right, forward)
        half_height = math_tan(math_radians(camera_block["vertical_fov_deg"]) * 0.5)
        width = int(scene.render["width"])
        height = int(scene.render["height"])
        return cls(
            eye=eye,
            forward=forward,
            right=right,
            up=up,
            half_height=half_height,
            aspect=width / height,
            width=width,
            height=height,
            primary_samples=int(scene.render["primary_samples"]),
        )

    def pixel_rays(self, row_start: int, row_end: int) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        """Return ray origins, unit directions and sub-pixel offsets for a row band."""

        samples = self.primary_samples
        rows = row_end - row_start
        count = rows * self.width * samples
        origins = np.broadcast_to(self.eye, (count, 3)).copy()
        rows_idx = np.repeat(np.arange(row_start, row_end, dtype=np.float64), self.width * samples)
        cols_idx = np.tile(np.repeat(np.arange(self.width, dtype=np.float64), samples), rows)
        # Deterministic sub-pixel offsets: fixed 2x2 jitter pattern, no RNG order dependency.
        lane = np.tile(np.arange(samples, dtype=np.float64), rows * self.width)
        jitter_x = np.where(lane % 2 == 0, 0.25, 0.75)
        jitter_y = np.where(lane < 2, 0.25, 0.75)
        x_ndc = (2.0 * (cols_idx + jitter_x) / self.width - 1.0) * self.aspect * self.half_height
        y_ndc = (1.0 - 2.0 * (rows_idx + jitter_y) / self.height) * self.half_height
        directions = (
            self.forward[None, :]
            + x_ndc[:, None] * self.right[None, :]
            + y_ndc[:, None] * self.up[None, :]
        )
        directions /= np.linalg.norm(directions, axis=1, keepdims=True)
        pixel_rows = rows_idx.astype(np.int64)
        pixel_cols = cols_idx.astype(np.int64)
        return origins, directions, np.stack([pixel_rows, pixel_cols], axis=1)


def math_radians(degrees: float) -> float:
    return degrees * np.pi / 180.0


def math_tan(value: float) -> float:
    return float(np.tan(value))
