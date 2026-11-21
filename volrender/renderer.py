"""Deterministic CPU volume ray caster used for offscreen and preview images."""

from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass, replace
from typing import Protocol

import numpy as np

from .errors import ErrorCode, VolumeRenderError
from .params import RenderParameters
from .transfer import build_transfer_lut
from .volume import VolumeData, map_window


class CancellationToken(Protocol):
    def is_set(self) -> bool: ...


class RenderCancelled(Exception):
    """Raised at a ray-batch checkpoint after cooperative cancellation."""


@dataclass(frozen=True)
class RenderResult:
    color: np.ndarray
    parameter_hash: str
    buffer_hash: str
    width: int
    height: int
    mask_enabled: bool = False
    mask_coverage: np.ndarray | None = None
    parameters: RenderParameters | None = None

    def __post_init__(self) -> None:
        if not self.parameter_hash or len(self.parameter_hash) != 64:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "color buffer is not bound to a parameter hash",
                field="parameter_hash",
            )
        color = np.asarray(self.color, dtype=np.uint8)
        expected_shape = (self.height, self.width, 4)
        if color.shape != expected_shape:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "color buffer shape does not match its viewport",
                field="color",
                details={"expected": list(expected_shape), "actual": list(color.shape)},
            )
        color = np.ascontiguousarray(color)
        actual_hash = hashlib.sha256(color.tobytes(order="C")).hexdigest()
        if self.buffer_hash != actual_hash:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "color buffer content hash does not match its binding",
                field="buffer_hash",
                details={"expected": actual_hash, "actual": self.buffer_hash},
            )
        if self.mask_coverage is not None:
            coverage = np.ascontiguousarray(np.asarray(self.mask_coverage, dtype=bool))
            if coverage.shape != (self.height, self.width):
                raise VolumeRenderError(
                    ErrorCode.E_MASK_INVALID,
                    "mask coverage shape does not match the viewport",
                    field="mask_coverage",
                )
            coverage.flags.writeable = False
            object.__setattr__(self, "mask_coverage", coverage)
        color.flags.writeable = False
        object.__setattr__(self, "color", color)

    @property
    def rgba_bytes(self) -> bytes:
        return self.color.tobytes(order="C")

    @classmethod
    def from_color(
        cls,
        color: np.ndarray,
        params: RenderParameters,
        *,
        mask_coverage: np.ndarray | None = None,
        parameter_hash: str | None = None,
    ) -> "RenderResult":
        color_array = np.ascontiguousarray(np.asarray(color, dtype=np.uint8))
        height, width = color_array.shape[:2]
        return cls(
            color=color_array,
            parameter_hash=params.parameter_hash if parameter_hash is None else parameter_hash,
            buffer_hash=hashlib.sha256(color_array.tobytes(order="C")).hexdigest(),
            width=width,
            height=height,
            mask_enabled=params.mask_enabled,
            mask_coverage=mask_coverage,
            parameters=params,
        )

    def bound_to(self, params: RenderParameters) -> bool:
        return self.parameter_hash == params.parameter_hash

    def assert_bound_to(self, params: RenderParameters) -> None:
        if not self.bound_to(params):
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "color buffer is bound to a different parameter hash",
                field="parameter_hash",
                details={"expected": params.parameter_hash, "actual": self.parameter_hash},
            )

    def to_manifest_dict(self, params: RenderParameters | None = None) -> dict[str, object]:
        bound_params = self.parameters if params is None else params
        payload: dict[str, object] = {
            "schema": "volrender-buffer-v1",
            "version": 1,
            "alpha_mode": "straight",
            "parameter_hash": self.parameter_hash,
            "buffer_hash": self.buffer_hash,
            "width": self.width,
            "height": self.height,
            "channels": 4,
            "mask_enabled": self.mask_enabled,
            "mask_coverage_hash": (
                hashlib.sha256(np.ascontiguousarray(self.mask_coverage, dtype=np.uint8).tobytes()).hexdigest()
                if self.mask_coverage is not None
                else ""
            ),
        }
        if bound_params is not None:
            payload["parameters"] = bound_params.to_snapshot_dict()
        return payload


def _cooperative_check(cancel_token: CancellationToken | None) -> None:
    if cancel_token is not None and cancel_token.is_set():
        raise RenderCancelled("render cancelled at a ray-batch checkpoint")


def _gradient_3d(field: np.ndarray) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    gradients: list[np.ndarray] = []
    for axis in range(3):
        if field.shape[axis] < 2:
            gradients.append(np.zeros_like(field))
        else:
            gradients.append(np.gradient(field, axis=axis))
    return gradients[0], gradients[1], gradients[2]


def _camera_rays(
    view_matrix: np.ndarray,
    width: int,
    height: int,
) -> tuple[np.ndarray, np.ndarray]:
    inverse = np.linalg.inv(view_matrix)
    camera_origin = np.array([0.0, 0.0, 2.45, 1.0], dtype=np.float64)
    world_origin = (inverse @ camera_origin)[:3]

    ys, xs = np.mgrid[0:height, 0:width]
    ndc_x = ((xs.astype(np.float64) + 0.5) / width) * 2.0 - 1.0
    ndc_y = 1.0 - ((ys.astype(np.float64) + 0.5) / height) * 2.0
    aspect = width / height
    tan_half_fov = math.tan(math.radians(17.0))
    camera_directions = np.stack(
        (
            ndc_x * aspect * tan_half_fov,
            ndc_y * tan_half_fov,
            -np.ones_like(ndc_x),
        ),
        axis=-1,
    ).reshape(-1, 3)
    rotation = inverse[:3, :3]
    world_directions = camera_directions @ rotation.T
    norms = np.linalg.norm(world_directions, axis=1, keepdims=True)
    world_directions = world_directions / np.maximum(norms, 1e-15)
    origins = np.broadcast_to(world_origin, world_directions.shape).copy()
    return origins, world_directions


def _ray_box(
    origins: np.ndarray,
    directions: np.ndarray,
    bounds: float = 0.5,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    with np.errstate(divide="ignore", invalid="ignore"):
        inverse_direction = 1.0 / directions
        t0 = (-bounds - origins) * inverse_direction
        t1 = (bounds - origins) * inverse_direction
    t_near = np.nanmax(np.minimum(t0, t1), axis=1)
    t_far = np.nanmin(np.maximum(t0, t1), axis=1)
    valid = (t_far > np.maximum(t_near, 0.0)) & np.isfinite(t_far)
    entry = np.where(valid, np.maximum(t_near, 0.0), 0.0)
    exit_distance = np.where(valid, t_far, 0.0)
    return entry, exit_distance, valid


def _trilinear(
    field: np.ndarray,
    x0: np.ndarray,
    x1: np.ndarray,
    y0: np.ndarray,
    y1: np.ndarray,
    z0: np.ndarray,
    z1: np.ndarray,
    fx: np.ndarray,
    fy: np.ndarray,
    fz: np.ndarray,
    shape: tuple[int, int, int],
) -> np.ndarray:
    ny, nz = shape[1], shape[2]
    sx, sy, sz = ny * nz, nz, 1
    c000 = field[x0 * sx + y0 * sy + z0 * sz]
    c100 = field[x1 * sx + y0 * sy + z0 * sz]
    c010 = field[x0 * sx + y1 * sy + z0 * sz]
    c110 = field[x1 * sx + y1 * sy + z0 * sz]
    c001 = field[x0 * sx + y0 * sy + z1 * sz]
    c101 = field[x1 * sx + y0 * sy + z1 * sz]
    c011 = field[x0 * sx + y1 * sy + z1 * sz]
    c111 = field[x1 * sx + y1 * sy + z1 * sz]
    c00 = c000 * (1.0 - fx) + c100 * fx
    c10 = c010 * (1.0 - fx) + c110 * fx
    c01 = c001 * (1.0 - fx) + c101 * fx
    c11 = c011 * (1.0 - fx) + c111 * fx
    c0 = c00 * (1.0 - fy) + c10 * fy
    c1 = c01 * (1.0 - fy) + c11 * fy
    return c0 * (1.0 - fz) + c1 * fz


def render_offscreen(
    volume: VolumeData,
    params: RenderParameters,
    *,
    cancel_token: CancellationToken | None = None,
    batch_size: int = 8,
) -> RenderResult:
    """Render a complete viewport with front-to-back alpha compositing.

    A small ray is advanced in normalized volume coordinates.  Scalar and
    gradient values are trilinearly interpolated; mask memberships are sampled
    at the same positions using nearest voxel lookup.
    """

    if params.volume_hash != volume.content_sha256:
        raise VolumeRenderError(
            ErrorCode.E_PARAM_STALE,
            "parameter group belongs to a different volume",
            field="volume_hash",
            details={"expected": volume.content_sha256, "actual": params.volume_hash},
        )
    if params.mask_enabled:
        if volume.mask is None:
            raise VolumeRenderError(ErrorCode.E_MASK_INVALID, "mask overlay requested without a mask", field="mask")
        if volume.mask.shape != volume.dims:
            raise VolumeRenderError(
                ErrorCode.E_MASK_INVALID,
                "mask dimensions do not match the density volume",
                field="mask_dims",
            )
        if params.mask_hash != volume.mask_sha256:
            raise VolumeRenderError(
                ErrorCode.E_MASK_INVALID,
                "parameter group is bound to a different mask hash",
                field="mask_hash",
                details={"expected": volume.mask_sha256, "actual": params.mask_hash},
            )
    if batch_size <= 0:
        raise ValueError("batch_size must be positive")

    width, height = params.viewport
    origins, directions = _camera_rays(np.asarray(params.view_matrix, dtype=np.float64).reshape(4, 4), width, height)

    mapped, _ = map_window(volume.density, params.window_center, params.window_width)
    lut, _ = build_transfer_lut(params.control_points)
    flat_map = np.ascontiguousarray(mapped, dtype=np.float32).reshape(-1)
    grad_x, grad_y, grad_z = _gradient_3d(mapped)
    spacing_x, spacing_y, spacing_z = volume.spacing
    flat_gx = np.ascontiguousarray(grad_x / np.float32(spacing_x)).reshape(-1)
    flat_gy = np.ascontiguousarray(grad_y / np.float32(spacing_y)).reshape(-1)
    flat_gz = np.ascontiguousarray(grad_z / np.float32(spacing_z)).reshape(-1)
    flat_mask = (
        np.ascontiguousarray(volume.mask, dtype=np.uint8).reshape(-1) if params.mask_enabled and volume.mask is not None else None
    )

    nx, ny, nz = volume.dims
    dimensions_minus_one = np.array([max(nx - 1, 1), max(ny - 1, 1), max(nz - 1, 1)], dtype=np.float64)
    step = params.step_size
    max_distance = math.sqrt(3.0)
    max_steps = int(math.ceil(max_distance / step)) + 2

    accum_rgb = np.zeros((height * width, 3), dtype=np.float64)
    transmittance = np.ones((height * width,), dtype=np.float64)
    coverage = np.zeros((height * width,), dtype=bool)
    light = np.array([0.35, 0.42, 0.84], dtype=np.float64)
    light /= np.linalg.norm(light)
    mask_color = np.array([0.95, 0.18, 0.10], dtype=np.float64)
    mask_opacity = 0.32

    rows_per_batch = max(1, int(batch_size))
    pixels_per_batch = rows_per_batch * width
    for batch_start in range(0, height * width, pixels_per_batch):
        _cooperative_check(cancel_token)
        batch_end = min(batch_start + pixels_per_batch, height * width)
        batch_origins = origins[batch_start:batch_end]
        batch_directions = directions[batch_start:batch_end]
        entry, exit_distance, valid = _ray_box(batch_origins, batch_directions)
        length = exit_distance - entry
        active = valid.copy()
        batch_transmittance = transmittance[batch_start:batch_end]
        batch_accum = accum_rgb[batch_start:batch_end]
        batch_coverage = coverage[batch_start:batch_end]

        for sample_index in range(max_steps):
            _cooperative_check(cancel_token)
            distance = sample_index * step
            step_active = active & (distance <= length)
            if not np.any(step_active):
                break
            step_ids = np.flatnonzero(step_active)
            ray_t = entry[step_ids] + distance
            positions = batch_origins[step_ids] + batch_directions[step_ids] * ray_t[:, None]
            grid = (positions + 0.5) * dimensions_minus_one
            grid = np.clip(grid, 0.0, dimensions_minus_one)

            x0 = np.floor(grid[:, 0]).astype(np.int64)
            y0 = np.floor(grid[:, 1]).astype(np.int64)
            z0 = np.floor(grid[:, 2]).astype(np.int64)
            x1 = np.minimum(x0 + 1, nx - 1)
            y1 = np.minimum(y0 + 1, ny - 1)
            z1 = np.minimum(z0 + 1, nz - 1)
            fx = grid[:, 0] - x0
            fy = grid[:, 1] - y0
            fz = grid[:, 2] - z0
            shape = (nx, ny, nz)

            scalar = np.clip(
                _trilinear(flat_map, x0, x1, y0, y1, z0, z1, fx, fy, fz, shape),
                0.0,
                1.0,
            )
            gx = _trilinear(flat_gx, x0, x1, y0, y1, z0, z1, fx, fy, fz, shape)
            gy = _trilinear(flat_gy, x0, x1, y0, y1, z0, z1, fx, fy, fz, shape)
            gz = _trilinear(flat_gz, x0, x1, y0, y1, z0, z1, fx, fy, fz, shape)

            lut_index = np.rint(scalar * (lut.shape[0] - 1)).astype(np.int32)
            lut_rgba = lut[lut_index]
            sample_color = lut_rgba[:, :3].astype(np.float64)
            sample_alpha = lut_rgba[:, 3].astype(np.float64)

            gradient_norm = np.sqrt(gx * gx + gy * gy + gz * gz)
            safe_norm = np.maximum(gradient_norm, 1e-8)
            diffuse = (
                (-gx / safe_norm) * light[0]
                + (-gy / safe_norm) * light[1]
                + (-gz / safe_norm) * light[2]
            )
            diffuse = np.clip(diffuse, 0.0, 1.0)
            shade = np.where(gradient_norm > 1e-7, 0.32 + 0.68 * diffuse, 1.0)
            sample_color *= shade[:, None]

            if flat_mask is not None:
                mask_x = np.clip(np.floor(grid[:, 0] + 0.5).astype(np.int64), 0, nx - 1)
                mask_y = np.clip(np.floor(grid[:, 1] + 0.5).astype(np.int64), 0, ny - 1)
                mask_z = np.clip(np.floor(grid[:, 2] + 0.5).astype(np.int64), 0, nz - 1)
                mask_flat_index = mask_x * (ny * nz) + mask_y * nz + mask_z
                mask_value = flat_mask[mask_flat_index].astype(np.float64)
                marked = mask_value > 0.0
                batch_coverage[step_ids[marked]] = True

            sample_alpha = np.clip(sample_alpha, 0.0, 0.999999)
            weighted_alpha = batch_transmittance[step_ids] * sample_alpha
            batch_accum[step_ids] += weighted_alpha[:, None] * sample_color
            batch_transmittance[step_ids] *= 1.0 - sample_alpha
            active &= batch_transmittance >= 1e-5

        transmittance[batch_start:batch_end] = batch_transmittance
        coverage[batch_start:batch_end] = batch_coverage

    alpha_flat = np.clip(1.0 - transmittance, 0.0, 1.0)
    straight_rgb = np.zeros_like(accum_rgb)
    visible = alpha_flat > 1e-8
    straight_rgb[visible] = accum_rgb[visible] / alpha_flat[visible, None]
    rgb = np.clip(straight_rgb.reshape(height, width, 3), 0.0, 1.0)
    alpha = alpha_flat.reshape(height, width, 1)
    coverage_grid = coverage.reshape(height, width)
    if flat_mask is not None:
        overlay = coverage_grid.astype(np.float64) * mask_opacity
        rgb = rgb * (1.0 - overlay[..., None]) + mask_color[None, None, :] * overlay[..., None]
        alpha = alpha + (1.0 - alpha) * overlay[..., None]
    rgba_float = np.concatenate((rgb, alpha), axis=2)
    rgba = np.floor(rgba_float * 255.0 + 0.5).astype(np.uint8)
    return RenderResult.from_color(rgba, params, mask_coverage=coverage_grid)


def rebind_result(result: RenderResult, parameter_hash: str) -> RenderResult:
    """Create a deliberately mismatched result for validation/error-path tests."""

    return replace(result, parameter_hash=parameter_hash)
