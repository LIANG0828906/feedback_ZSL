from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .camera import CameraPath
from .scene import Occluder, SpatialLimits


@dataclass
class SpatialRender:
    stereo: np.ndarray
    ild_db: np.ndarray
    itd_us: np.ndarray
    azimuth_rad: np.ndarray
    elevation_rad: np.ndarray


def _slew_limit(values: np.ndarray, max_step: float) -> np.ndarray:
    if max_step <= 0:
        return np.full_like(values, values[0] if values.size else 0.0)
    if values.size < 2:
        return values.copy()
    differences = np.diff(values)
    differences = np.clip(differences, -max_step, max_step)
    return np.concatenate(
        [values[:1], values[0] + np.cumsum(differences, dtype=np.float64)]
    )


def spatial_parameters(
    times: np.ndarray,
    source_position: np.ndarray,
    camera: CameraPath,
    limits: SpatialLimits,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    camera_positions, camera_basis = camera.frames_at(times)
    relative = source_position[None, :] - camera_positions
    forwards = camera_basis[:, 0:3]
    rights = camera_basis[:, 3:6]
    ups = camera_basis[:, 6:9]

    front_component = np.einsum("ij,ij->i", relative, forwards)
    right_component = np.einsum("ij,ij->i", relative, rights)
    up_component = np.einsum("ij,ij->i", relative, ups)
    horizontal = np.hypot(front_component, right_component)
    azimuth = np.arctan2(right_component, front_component)
    elevation = np.arctan2(up_component, horizontal)
    distance = np.maximum(np.linalg.norm(relative, axis=1), 1e-6)

    itd_scale = (
        2.0
        * limits.head_radius_m
        / limits.speed_of_sound_m_s
        * 1_000_000.0
    )
    raw_itd = itd_scale * np.sin(azimuth) * np.cos(elevation)
    raw_ild = (
        limits.ild_strength_db
        * np.sin(azimuth)
        * np.cos(elevation)
    )
    raw_itd = np.clip(raw_itd, -limits.max_itd_us, limits.max_itd_us)
    raw_ild = np.clip(raw_ild, -limits.max_ild_db, limits.max_ild_db)

    if times.size > 1:
        sample_rate = 1.0 / max(float(times[1] - times[0]), 1e-12)
        itd_step = limits.max_itd_rate_us_per_s / sample_rate
        ild_step = limits.max_ild_rate_db_per_s / sample_rate
    else:
        itd_step = limits.max_itd_us
        ild_step = limits.max_ild_db
    itd = _slew_limit(raw_itd, itd_step)
    ild = _slew_limit(raw_ild, ild_step)
    return azimuth, elevation, ild, itd, distance


def render_spatial_event(
    mono: np.ndarray,
    times: np.ndarray,
    source_position: np.ndarray,
    camera: CameraPath,
    limits: SpatialLimits,
    sample_rate: int,
) -> SpatialRender:
    if mono.ndim != 1:
        raise ValueError("spatial event input must be mono")
    azimuth, elevation, ild, itd, distance = spatial_parameters(
        times, source_position, camera, limits
    )

    attenuation = 1.0 / (1.0 + 0.12 * distance)
    left_gain = attenuation * np.power(10.0, -ild / 40.0)
    right_gain = attenuation * np.power(10.0, ild / 40.0)

    base_delay_s = 0.5 * limits.max_itd_us * 1e-6 + 1e-4
    left_delay = (base_delay_s + 0.5 * itd * 1e-6) * sample_rate
    right_delay = (base_delay_s - 0.5 * itd * 1e-6) * sample_rate
    index = np.arange(mono.shape[0], dtype=np.float64)
    source_grid = np.arange(mono.shape[0], dtype=np.float64)
    left = np.interp(index - left_delay, source_grid, mono, left=0.0, right=0.0)
    right = np.interp(index - right_delay, source_grid, mono, left=0.0, right=0.0)
    stereo = np.column_stack([left * left_gain, right * right_gain])
    return SpatialRender(
        stereo=stereo,
        ild_db=ild,
        itd_us=itd,
        azimuth_rad=azimuth,
        elevation_rad=elevation,
    )


def _ray_box_intersection(
    origin: np.ndarray, target: np.ndarray, minimum: np.ndarray, maximum: np.ndarray
) -> bool:
    direction = target - origin
    t_min = 0.0
    t_max = 1.0
    for axis in range(3):
        component = direction[axis]
        if abs(component) < 1e-12:
            if origin[axis] < minimum[axis] or origin[axis] > maximum[axis]:
                return False
            continue
        t1 = (minimum[axis] - origin[axis]) / component
        t2 = (maximum[axis] - origin[axis]) / component
        if t1 > t2:
            t1, t2 = t2, t1
        t_min = max(t_min, t1)
        t_max = min(t_max, t2)
        if t_min > t_max:
            return False
    return t_max > 1e-9 and t_min < 1.0 - 1e-9


def occluder_layers(
    camera_position: np.ndarray,
    source_position: np.ndarray,
    occluders: list[Occluder],
) -> int:
    return sum(
        1
        for occluder in occluders
        if _ray_box_intersection(
            camera_position,
            source_position,
            occluder.minimum,
            occluder.maximum,
        )
    )

