from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .validation import CameraData


@dataclass(frozen=True)
class Camera:
    position: np.ndarray
    forward: np.ndarray
    right: np.ndarray
    up: np.ndarray
    tan_half_fov: float
    aspect: float
    width: int
    height: int
    near: float
    far: float

    @classmethod
    def from_data(
        cls,
        data: CameraData,
        width: int,
        height: int,
    ) -> "Camera":
        forward = data.target - data.position
        forward = forward / np.linalg.norm(forward)
        right = np.cross(forward, data.up)
        right = right / np.linalg.norm(right)
        up = np.cross(right, forward)
        up = up / np.linalg.norm(up)
        aspect = float(width) / float(height)
        return cls(
            position=data.position,
            forward=forward,
            right=right,
            up=up,
            tan_half_fov=float(np.tan(np.deg2rad(data.fov_y_degrees) * 0.5)),
            aspect=aspect,
            width=int(width),
            height=int(height),
            near=float(data.near),
            far=float(data.far),
        )

    def rays(self, x0: int, y0: int, width: int, height: int) -> np.ndarray:
        xs = np.arange(x0, x0 + width, dtype=np.float64) + 0.5
        ys = np.arange(y0, y0 + height, dtype=np.float64) + 0.5
        sx = (
            (2.0 * xs / float(self.width) - 1.0)
            * self.tan_half_fov
            * self.aspect
        )
        sy = (1.0 - 2.0 * ys / float(self.height)) * self.tan_half_fov
        grid_x, grid_y = np.meshgrid(sx, sy)
        return (
            self.forward[None, None, :]
            + grid_x[:, :, None] * self.right[None, None, :]
            + grid_y[:, :, None] * self.up[None, None, :]
        )

    def ray_at_pixel(self, x: float, y: float) -> np.ndarray:
        sx = (
            (2.0 * x / float(self.width) - 1.0)
            * self.tan_half_fov
            * self.aspect
        )
        sy = (1.0 - 2.0 * y / float(self.height)) * self.tan_half_fov
        direction = self.forward + sx * self.right + sy * self.up
        return direction / np.linalg.norm(direction)

    def project_points(self, points: np.ndarray) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        """Return screen x, screen y and positive view depth."""
        rel = points - self.position
        depth = rel @ self.forward
        horizontal = rel @ self.right
        vertical = rel @ self.up
        valid = depth > self.near
        safe_depth = np.where(valid, depth, 1.0)
        ndc_x = horizontal / safe_depth / (self.tan_half_fov * self.aspect)
        ndc_y = vertical / safe_depth / self.tan_half_fov
        screen_x = (ndc_x + 1.0) * 0.5 * self.width
        screen_y = (1.0 - ndc_y) * 0.5 * self.height
        return screen_x, screen_y, np.where(valid, depth, -1.0)

    def project_point(self, point: np.ndarray) -> tuple[float, float, float] | None:
        x, y, depth = self.project_points(point[None, :])
        if float(depth[0]) <= self.near:
            return None
        return float(x[0]), float(y[0]), float(depth[0])


def intersect_wall_rays(
    camera: Camera,
    origin: np.ndarray,
    normal: np.ndarray,
    directions: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Intersect camera rays with a plane and return points, t, hit mask."""
    denominator = directions @ normal
    numerator = float(np.dot(origin - camera.position, normal))
    safe_denominator = np.where(np.abs(denominator) > 1e-12, denominator, 1.0)
    t = numerator / safe_denominator
    points = camera.position[None, None, :] + directions * t[:, :, None]
    hit = (
        (np.abs(denominator) > 1e-12)
        & (t > camera.near)
        & (t < camera.far)
    )
    return points, t, hit


def wall_uv(
    points: np.ndarray,
    origin: np.ndarray,
    u_axis: np.ndarray,
    v_axis: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    relative = points - origin
    return relative @ u_axis, relative @ v_axis

