"""Camera and ray generation with a persisted world-to-camera matrix."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .errors import E_INPUT_INVALID, VolRenderError


def normalize(vector: np.ndarray) -> np.ndarray:
    value = np.asarray(vector, dtype=np.float64)
    length = float(np.linalg.norm(value))
    if not np.isfinite(length) or length <= 1.0e-15:
        return np.zeros_like(value)
    return value / length


def look_at_view(
    eye: tuple[float, float, float] | np.ndarray,
    target: tuple[float, float, float] | np.ndarray = (0.0, 0.0, 0.0),
    up: tuple[float, float, float] | np.ndarray = (0.0, 1.0, 0.0),
) -> np.ndarray:
    """Return a world-to-camera matrix using the OpenGL camera convention."""

    eye_v = np.asarray(eye, dtype=np.float64)
    target_v = np.asarray(target, dtype=np.float64)
    forward = normalize(target_v - eye_v)
    right = normalize(np.cross(forward, np.asarray(up, dtype=np.float64)))
    if np.linalg.norm(right) <= 1.0e-15:
        right = normalize(np.cross(forward, np.array([1.0, 0.0, 0.0])))
    if np.linalg.norm(right) <= 1.0e-15:
        raise VolRenderError(E_INPUT_INVALID, "camera up vector is degenerate")
    camera_up = normalize(np.cross(right, forward))
    rotation = np.stack((right, camera_up, -forward), axis=0)
    view = np.eye(4, dtype=np.float64)
    view[:3, :3] = rotation
    view[:3, 3] = -rotation @ eye_v
    return view


def validate_view_matrix(view_matrix: np.ndarray) -> np.ndarray:
    matrix = np.asarray(view_matrix, dtype=np.float64)
    if matrix.shape != (4, 4):
        raise VolRenderError(E_INPUT_INVALID, "view matrix must be 4x4")
    if not np.all(np.isfinite(matrix)):
        raise VolRenderError(E_INPUT_INVALID, "view matrix contains non-finite values")
    rotation = matrix[:3, :3]
    if not np.allclose(rotation @ rotation.T, np.eye(3), atol=1.0e-6):
        raise VolRenderError(E_INPUT_INVALID, "view matrix rotation is not orthonormal")
    if not np.allclose(matrix[3], np.array([0.0, 0.0, 0.0, 1.0])):
        raise VolRenderError(E_INPUT_INVALID, "view matrix bottom row is invalid")
    return matrix


def default_view_matrix(
    extent_xyz: tuple[float, float, float],
) -> np.ndarray:
    largest = max(extent_xyz)
    eye = np.array([1.35 * largest, -1.70 * largest, 1.85 * largest])
    return look_at_view(eye, target=(0.0, 0.0, 0.0), up=(0.0, 0.0, 1.0))


@dataclass(frozen=True)
class RayBundle:
    origin: np.ndarray
    direction: np.ndarray
    t_near: np.ndarray
    t_far: np.ndarray
    valid: np.ndarray


def _ray_box_intersection(
    origin: np.ndarray,
    direction: np.ndarray,
    box_min: np.ndarray,
    box_max: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    with np.errstate(divide="ignore", invalid="ignore"):
        inv = 1.0 / direction
        t0 = (box_min - origin) * inv
        t1 = (box_max - origin) * inv
    near = np.minimum(t0, t1)
    far = np.maximum(t0, t1)
    near = np.where(np.abs(direction) < 1.0e-15, -np.inf, near)
    far = np.where(np.abs(direction) < 1.0e-15, np.inf, far)
    t_near = np.max(near, axis=-1)
    t_far = np.min(far, axis=-1)
    valid = np.isfinite(t_far) & (t_far > np.maximum(t_near, 0.0))
    t_near = np.maximum(t_near, 0.0)
    return t_near, t_far, valid


def build_rays(
    view_matrix: np.ndarray,
    extent_xyz: tuple[float, float, float],
    image_size: tuple[int, int],
    *,
    fov_degrees: float = 38.0,
) -> RayBundle:
    """Generate one normalized world-space ray per output pixel."""

    view = validate_view_matrix(view_matrix)
    width, height = int(image_size[0]), int(image_size[1])
    if width <= 0 or height <= 0:
        raise VolRenderError(E_INPUT_INVALID, "image size must be positive")
    if not np.isfinite(fov_degrees) or not 1.0 < fov_degrees < 179.0:
        raise VolRenderError(E_INPUT_INVALID, "field of view must be in (1, 179)")

    rotation_t = view[:3, :3].T
    eye = -rotation_t @ view[:3, 3]
    tan_half = np.tan(np.deg2rad(fov_degrees) * 0.5)
    aspect = width / height
    xs = (2.0 * (np.arange(width) + 0.5) / width - 1.0) * tan_half * aspect
    ys = (1.0 - 2.0 * (np.arange(height) + 0.5) / height) * tan_half
    grid_x, grid_y = np.meshgrid(xs, ys)
    cam_dirs = np.stack(
        (grid_x, grid_y, -np.ones_like(grid_x)),
        axis=-1,
    )
    cam_dirs /= np.linalg.norm(cam_dirs, axis=-1, keepdims=True)
    world_dirs = cam_dirs @ rotation_t.T

    origin = np.broadcast_to(eye, world_dirs.shape).copy()
    half_extent = 0.5 * np.asarray(extent_xyz, dtype=np.float64)
    t_near, t_far, valid = _ray_box_intersection(
        origin,
        world_dirs,
        -half_extent,
        half_extent,
    )
    return RayBundle(
        origin=origin,
        direction=world_dirs,
        t_near=t_near,
        t_far=t_far,
        valid=valid,
    )
