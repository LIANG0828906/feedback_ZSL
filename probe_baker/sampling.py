"""Analytic local sky sampling and occluder visibility."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Callable

import numpy as np

from .errors import ErrorCode, ProbeError
from .scene import Occluder, Scene


def sky_radiance(directions: np.ndarray, scene: Scene) -> np.ndarray:
    dirs = np.asarray(directions, dtype=np.float64)
    if dirs.ndim != 2 or dirs.shape[1] != 3 or not np.isfinite(dirs).all():
        raise ProbeError(ErrorCode.SH_INVALID, "sky sampling directions must be finite vec3 values")
    up = np.clip(dirs[:, 1], -1.0, 1.0)
    zenith = np.asarray(scene.sky.zenith, dtype=np.float64)
    horizon = np.asarray(scene.sky.horizon, dtype=np.float64)
    ground = np.asarray(scene.sky.ground, dtype=np.float64)
    colors = np.empty((dirs.shape[0], 3), dtype=np.float64)
    above = up >= 0.0
    blend = np.sqrt(np.maximum(up, 0.0))
    colors[above] = (
        horizon[None, :] * (1.0 - blend[above, None])
        + zenith[None, :] * blend[above, None]
    )
    below_amount = np.abs(up[~above])
    colors[~above] = (
        horizon[None, :] * (1.0 - below_amount[:, None])
        + ground[None, :] * below_amount[:, None]
    )
    return colors * float(scene.sky.intensity)


def _ray_hits_box(
    origins: np.ndarray,
    directions: np.ndarray,
    lower: tuple[float, float, float],
    upper: tuple[float, float, float],
) -> np.ndarray:
    lower_array = np.asarray(lower, dtype=np.float64)
    upper_array = np.asarray(upper, dtype=np.float64)
    with np.errstate(divide="ignore", invalid="ignore"):
        inverse = 1.0 / directions
        t0 = (lower_array[None, None, :] - origins[:, None, :]) * inverse[None, :, :]
        t1 = (upper_array[None, None, :] - origins[:, None, :]) * inverse[None, :, :]
    parallel = np.abs(directions) < 1e-15
    if np.any(parallel):
        origins_3d = origins[:, None, :]
        inside = (origins_3d >= lower_array[None, None, :]) & (
            origins_3d <= upper_array[None, None, :]
        )
        t0 = np.where(parallel[None, :, :], np.where(inside, -np.inf, np.inf), t0)
        t1 = np.where(parallel[None, :, :], np.where(inside, np.inf, -np.inf), t1)
    t_min = np.minimum(t0, t1).max(axis=2)
    t_max = np.maximum(t0, t1).min(axis=2)
    return (t_max >= np.maximum(t_min, 0.0)) & (t_max > 0.0)


def _ray_hits_sphere(
    origins: np.ndarray,
    directions: np.ndarray,
    center: tuple[float, float, float],
    radius: float,
) -> np.ndarray:
    offset = origins - np.asarray(center, dtype=np.float64)[None, :]
    projection = offset @ directions.T
    distance_squared = np.einsum("nd,nd->n", offset, offset)
    discriminant = projection**2 - (distance_squared[:, None] - radius * radius)
    hit_shape = discriminant >= 0.0
    if not np.any(hit_shape):
        return np.zeros(discriminant.shape, dtype=bool)
    nearest = -projection - np.sqrt(np.maximum(discriminant, 0.0))
    return hit_shape & (nearest >= 0.0)


def occluder_hit_masks(
    origins: np.ndarray,
    directions: np.ndarray,
    occluders: tuple[Occluder, ...],
) -> np.ndarray:
    origins_array = np.asarray(origins, dtype=np.float64)
    dirs = np.asarray(directions, dtype=np.float64)
    if origins_array.ndim != 2 or origins_array.shape[1] != 3:
        raise ProbeError(ErrorCode.SCENE_INVALID, "probe origins must have shape (N, 3)")
    if dirs.ndim != 2 or dirs.shape[1] != 3:
        raise ProbeError(ErrorCode.SH_INVALID, "sampling directions must have shape (D, 3)")
    masks = np.zeros((origins_array.shape[0], len(occluders)), dtype=bool)
    for index, occluder in enumerate(occluders):
        if occluder.kind == "aabb":
            masks[:, index] = _ray_hits_box(
                origins_array, dirs, occluder.lower, occluder.upper
            ).any(axis=1)
        elif occluder.kind == "sphere":
            masks[:, index] = _ray_hits_sphere(
                origins_array, dirs, occluder.center, occluder.radius_value
            ).any(axis=1)
        else:
            raise ProbeError(ErrorCode.SCENE_INVALID, f"unsupported occluder kind {occluder.kind}")
    return masks


@dataclass(frozen=True)
class SampleResult:
    radiance: np.ndarray
    visibility: np.ndarray
    visibility_mean: np.ndarray
    occluder_hits: np.ndarray


def sample_probes(
    origins: np.ndarray,
    directions: np.ndarray,
    scene: Scene,
    chunk_size: int = 4096,
    checkpoint: Callable[[int], None] | None = None,
) -> SampleResult:
    origins_array = np.asarray(origins, dtype=np.float64)
    dirs = np.asarray(directions, dtype=np.float64)
    if origins_array.ndim != 2 or origins_array.shape[1] != 3:
        raise ProbeError(ErrorCode.SCENE_INVALID, "probe origins must have shape (N, 3)")
    if not np.isfinite(origins_array).all() or not np.isfinite(dirs).all():
        raise ProbeError(ErrorCode.SH_INVALID, "sampling inputs contain non-finite values")
    if chunk_size <= 0:
        raise ProbeError(ErrorCode.SCENE_INVALID, "chunk_size must be positive")

    active_occluders = scene.active_occluders()
    sky = sky_radiance(dirs, scene)
    albedo = np.asarray(scene.material.albedo, dtype=np.float64)
    radiance_chunks: list[np.ndarray] = []
    visibility_chunks: list[np.ndarray] = []
    hit_chunks: list[np.ndarray] = []
    for chunk_index, start in enumerate(range(0, origins_array.shape[0], chunk_size)):
        if checkpoint is not None:
            checkpoint(chunk_index)
        stop = min(start + chunk_size, origins_array.shape[0])
        local_origins = origins_array[start:stop]
        visibility = np.ones((stop - start, dirs.shape[0]), dtype=np.float64)
        hits = np.zeros((stop - start, len(active_occluders)), dtype=bool)
        for occluder_index, occluder in enumerate(active_occluders):
            if occluder.kind == "aabb":
                blocked = _ray_hits_box(
                    local_origins, dirs, occluder.lower, occluder.upper
                )
            else:
                blocked = _ray_hits_sphere(
                    local_origins, dirs, occluder.center, occluder.radius_value
                )
            hits[:, occluder_index] = blocked.any(axis=1)
            visibility[blocked] = 0.0
        radiance_chunks.append(
            sky[None, :, :] * visibility[:, :, None] * albedo[None, None, :]
        )
        visibility_chunks.append(visibility)
        hit_chunks.append(hits)

    radiance = np.concatenate(radiance_chunks, axis=0) if radiance_chunks else np.empty((0, dirs.shape[0], 3))
    visibility = np.concatenate(visibility_chunks, axis=0) if visibility_chunks else np.empty((0, dirs.shape[0]))
    hits = np.concatenate(hit_chunks, axis=0) if hit_chunks else np.empty((0, len(active_occluders)), dtype=bool)
    radiance.setflags(write=False)
    visibility.setflags(write=False)
    hits.setflags(write=False)
    means = np.mean(visibility, axis=1) if visibility.size else np.empty((0,))
    means.setflags(write=False)
    return SampleResult(radiance, visibility, means, hits)

