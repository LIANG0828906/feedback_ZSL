"""Vector, plane and perspective-camera helpers (float64 for stability)."""

from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np


EPS = 1e-9


def normalize(vectors: np.ndarray, axis: int = -1) -> np.ndarray:
    vectors = np.asarray(vectors, dtype=np.float64)
    length = np.linalg.norm(vectors, axis=axis, keepdims=True)
    return vectors / np.maximum(length, EPS)


def dot(a: np.ndarray, b: np.ndarray) -> np.ndarray:
    return np.sum(np.asarray(a) * np.asarray(b), axis=-1)


def cross(a: np.ndarray, b: np.ndarray) -> np.ndarray:
    return np.cross(a, b)


def plane_normal_and_point(plane: dict) -> tuple[np.ndarray, np.ndarray]:
    normal = normalize(np.asarray(plane["normal"], dtype=np.float64))
    point = np.asarray(plane["point"], dtype=np.float64)
    return normal, point


def ray_plane_intersection(
    origin: np.ndarray,
    direction: np.ndarray,
    normal: np.ndarray,
    point: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    """Intersect rays with an infinite plane.

    Returns the intersection points and a boolean mask of finite hits.
    ``direction`` need not be normalised.
    """
    denominator = dot(direction, normal)
    numerator = dot(point - origin, normal)
    safe = np.where(np.abs(denominator) < EPS, EPS, denominator)
    distance = numerator / safe
    hits = np.abs(denominator) >= EPS
    return origin + direction * distance[..., None], hits


def point_plane_distance(point: np.ndarray, normal: np.ndarray,
                         plane_point: np.ndarray) -> np.ndarray:
    return np.abs(dot(point - plane_point, normal))


def triangle_area(a: np.ndarray, b: np.ndarray, c: np.ndarray) -> np.ndarray:
    return 0.5 * np.linalg.norm(cross(b - a, c - a), axis=-1)


def barycentric(
    points: np.ndarray,
    a: np.ndarray,
    b: np.ndarray,
    c: np.ndarray,
) -> np.ndarray:
    """Barycentric weights for ``points`` against triangle a/b/c (2-D inputs)."""
    v0 = b - a
    v1 = c - a
    v2 = points - a
    denominator = v0[..., 0] * v1[..., 1] - v0[..., 1] * v1[..., 0]
    safe = np.where(np.abs(denominator) < EPS, EPS, denominator)
    w_b = (v2[..., 0] * v1[..., 1] - v2[..., 1] * v1[..., 0]) / safe
    w_c = (v0[..., 0] * v2[..., 1] - v0[..., 1] * v2[..., 0]) / safe
    w_a = 1.0 - w_b - w_c
    return np.stack([w_a, w_b, w_c], axis=-1)


def interpolate(weights: np.ndarray, values: np.ndarray) -> np.ndarray:
    """Interpolate per-vertex ``values`` using barycentric ``weights``."""
    return np.sum(weights[..., None] * values, axis=-2)


@dataclass(frozen=True)
class Camera:
    eye: np.ndarray
    target: np.ndarray
    up: np.ndarray
    fov_y_deg: float
    width: int
    height: int

    @property
    def aspect(self) -> float:
        return self.width / self.height

    def basis(self) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        forward = normalize(self.target - self.eye)
        right = normalize(cross(forward, self.up))
        up = cross(right, forward)
        return forward, right, up

    def rays(self) -> tuple[np.ndarray, np.ndarray]:
        """Return per-pixel ray origins and unit directions, row-major."""
        forward, right, up = self.basis()
        half_height = math.tan(math.radians(self.fov_y_deg) * 0.5)
        half_width = half_height * self.aspect
        u_coords = (np.arange(self.width, dtype=np.float64) + 0.5) / self.width
        v_coords = (np.arange(self.height, dtype=np.float64) + 0.5) / self.height
        u_grid, v_grid = np.meshgrid(
            (u_coords * 2.0 - 1.0) * half_width,
            (1.0 - v_coords * 2.0) * half_height,
            indexing="xy",
        )
        directions = (forward[None, None, :]
                      + u_grid[..., None] * right[None, None, :]
                      + v_grid[..., None] * up[None, None, :])
        directions = normalize(directions.reshape(-1, 3)).reshape(
            self.height, self.width, 3)
        origins = np.broadcast_to(self.eye, directions.shape).copy()
        return origins, directions

    def project(self, world_points: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
        """Project world points to pixel coordinates plus camera-space depth."""
        forward, right, up = self.basis()
        relative = world_points - self.eye
        depth = dot(relative, forward)
        x_ndc = dot(relative, right) / np.maximum(depth, EPS)
        y_ndc = dot(relative, up) / np.maximum(depth, EPS)
        half_height = math.tan(math.radians(self.fov_y_deg) * 0.5)
        half_width = half_height * self.aspect
        px = (x_ndc / half_width + 1.0) * 0.5 * self.width - 0.5
        py = (1.0 - y_ndc / half_height) * 0.5 * self.height - 0.5
        return np.stack([px, py], axis=-1), depth


def orbit_eye(center: np.ndarray, radius: float, azimuth_deg: float,
              elevation_deg: float) -> np.ndarray:
    azimuth = math.radians(azimuth_deg)
    elevation = math.radians(elevation_deg)
    horizontal = radius * math.cos(elevation)
    offset = np.array([
        horizontal * math.sin(azimuth),
        radius * math.sin(elevation),
        horizontal * math.cos(azimuth),
    ], dtype=np.float64)
    return center + offset
