"""Deterministic ray traversal and volumetric scattering."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterator

import numpy as np

from .config import RenderConfig
from .scene import Scene


_EPS = 1e-10


@dataclass(frozen=True, slots=True)
class Segment:
    t0: float
    t1: float
    active: tuple[int, ...]


@dataclass(frozen=True, slots=True)
class Sample:
    t: float
    ds: float
    extinction: float
    scattering: float
    phase_g: float


@dataclass(frozen=True, slots=True)
class PixelTrace:
    color: np.ndarray
    transmittance: float
    view_samples: int
    samples_used: int


def boundary_segments(
    scene: Scene,
    origin: np.ndarray,
    direction: np.ndarray,
    max_distance: float,
) -> tuple[Segment, ...]:
    events: list[tuple[float, int, int]] = []
    for medium_index, medium in enumerate(scene.media):
        interval = medium.intersect_interval(origin, direction, max_distance)
        if interval is None:
            continue
        enter, exit_ = interval
        events.append((enter, -1, medium_index))
        events.append((exit_, 1, medium_index))
    if not events:
        return ()
    events.sort(key=lambda event: (event[0], event[1], event[2]))
    active: set[int] = set()
    cursor = events[0][0]
    segments: list[Segment] = []
    for event_t, delta, medium_index in events:
        if event_t > cursor + _EPS and active:
            segments.append(
                Segment(cursor, event_t, tuple(sorted(active)))
            )
        cursor = max(cursor, event_t)
        if delta < 0:
            active.add(medium_index)
        else:
            active.discard(medium_index)
    return tuple(segments)


def sample_phase(pixel_x: int, pixel_y: int, segment_index: int) -> float:
    raw = (
        pixel_x * 0.7548776662466927
        + pixel_y * 0.5698402909980532
        + segment_index * 0.3819660112501051
        + 0.5
    )
    return raw - math.floor(raw)


def coefficients(
    scene: Scene, active: tuple[int, ...], point: np.ndarray
) -> tuple[float, float, float, float]:
    extinction = 0.0
    scattering = 0.0
    weighted_g = 0.0
    density_sum = 0.0
    for medium_index in active:
        medium = scene.media[medium_index]
        density = medium.sample_density(point)
        if density <= 0.0:
            continue
        density_sum += density
        extinction += medium.sigma_t * density
        scattering += medium.sigma_s * density
        weighted_g += medium.phase_g * density
    phase_g = weighted_g / density_sum if density_sum > 0.0 else 0.0
    return density_sum, extinction, scattering, phase_g


def _adaptive_scale(extinction: float) -> float:
    factor = 0.5 + 1.0 / (0.5 + extinction * 8.0)
    factor = max(0.5, min(2.0, factor))
    return round(factor * 4.0) / 4.0


def iter_samples(
    scene: Scene,
    origin: np.ndarray,
    direction: np.ndarray,
    segments: tuple[Segment, ...],
    pixel_x: int,
    pixel_y: int,
    base_step: float,
    step_scale: float,
) -> Iterator[Sample]:
    nominal_step = max(base_step * step_scale, 1e-6)
    for segment_index, segment in enumerate(segments):
        length = segment.t1 - segment.t0
        if length <= _EPS:
            continue
        phase = sample_phase(pixel_x, pixel_y, segment_index)
        t = segment.t0 + min(length, nominal_step * phase)
        while t < segment.t1 - _EPS:
            point = origin + direction * t
            _, extinction, scattering, phase_g = coefficients(
                scene, segment.active, point
            )
            adaptive = _adaptive_scale(extinction)
            ds = min(segment.t1 - t, nominal_step * adaptive)
            if ds <= _EPS:
                break
            yield Sample(
                t=t,
                ds=ds,
                extinction=extinction,
                scattering=scattering,
                phase_g=phase_g,
            )
            t += ds


def _shadow_visibility(
    scene: Scene,
    point: np.ndarray,
    config: RenderConfig,
    step_scale: float,
) -> float:
    if config.shadow_samples == 0:
        return 1.0
    light_direction = scene.light.direction
    step = config.shadow_distance / config.shadow_samples
    optical_depth = 0.0
    for index in range(config.shadow_samples):
        sample_point = point + light_direction * (step * (index + 0.5))
        for medium in scene.media:
            if medium.contains(sample_point):
                density = medium.sample_density(sample_point)
                if density > 0.0:
                    optical_depth += medium.sigma_t * density * step
    # The scale is recorded for traceability. A larger step scale coarsens
    # the view ray, while shadow visibility remains a fixed-budget estimator.
    _ = step_scale
    return math.exp(-optical_depth)


def henyey_greenstein(cos_theta: float, phase_g: float) -> float:
    denominator = 1.0 + phase_g * phase_g - 2.0 * phase_g * cos_theta
    denominator = max(denominator, 1e-6)
    return (1.0 - phase_g * phase_g) / (
        4.0 * math.pi * denominator ** 1.5
    )


def trace_pixel(
    scene: Scene,
    pixel_x: int,
    pixel_y: int,
    config: RenderConfig,
    step_scale: float,
    precision: str,
) -> PixelTrace:
    origin = scene.camera.position
    direction = scene.camera.ray_direction(pixel_x, pixel_y)
    segments = boundary_segments(
        scene, origin, direction, config.max_distance
    )
    light_direction = scene.light.direction
    cosine = float(np.dot(direction, light_direction))
    radiance = scene.light.radiance

    if precision == "float64":
        transmittance = 1.0
        color = np.zeros(3, dtype=np.float64)
        view_samples = 0
        for sample in iter_samples(
            scene,
            origin,
            direction,
            segments,
            pixel_x,
            pixel_y,
            config.base_step,
            step_scale,
        ):
            view_samples += 1
            visibility = _shadow_visibility(
                scene, origin + direction * sample.t, config, step_scale
            )
            attenuation = math.exp(-sample.extinction * sample.ds)
            if sample.extinction > 0.0:
                phase = henyey_greenstein(cosine, sample.phase_g)
                albedo = sample.scattering / sample.extinction
                source = radiance * phase * visibility * albedo
                color += transmittance * (1.0 - attenuation) * source
            transmittance *= attenuation
        color += transmittance * scene.background
        used = view_samples * (1 + config.shadow_samples)
        return PixelTrace(
            color=color,
            transmittance=transmittance,
            view_samples=view_samples,
            samples_used=used,
        )

    if precision != "fixed":
        raise ValueError(f"unsupported precision path: {precision}")

    one = config.fixed_scale
    transmittance_fixed = one
    color_fixed = [0, 0, 0]
    view_samples = 0
    for sample in iter_samples(
        scene,
        origin,
        direction,
        segments,
        pixel_x,
        pixel_y,
        config.base_step,
        step_scale,
    ):
        view_samples += 1
        visibility = _shadow_visibility(
            scene, origin + direction * sample.t, config, step_scale
        )
        attenuation = math.exp(-sample.extinction * sample.ds)
        attenuation_fixed = max(
            0, min(one, int(round(attenuation * one)))
        )
        if sample.extinction > 0.0:
            phase = henyey_greenstein(cosine, sample.phase_g)
            albedo = sample.scattering / sample.extinction
            source = radiance * phase * visibility * albedo
            source_fixed = [
                max(0, int(round(float(channel) * one))) for channel in source
            ]
            delta_fixed = one - attenuation_fixed
            denominator = one * one
            for channel in range(3):
                color_fixed[channel] += (
                    transmittance_fixed
                    * delta_fixed
                    * source_fixed[channel]
                ) // denominator
        transmittance_fixed = (
            transmittance_fixed * attenuation_fixed + one // 2
        ) // one
    for channel in range(3):
        background_fixed = max(
            0, int(round(float(scene.background[channel]) * one))
        )
        color_fixed[channel] += (
            transmittance_fixed * background_fixed
        ) // one
    used = view_samples * (1 + config.shadow_samples)
    return PixelTrace(
        color=np.asarray(color_fixed, dtype=np.float64) / one,
        transmittance=transmittance_fixed / one,
        view_samples=view_samples,
        samples_used=used,
    )


def count_pixel_samples(
    scene: Scene,
    pixel_x: int,
    pixel_y: int,
    config: RenderConfig,
    step_scale: float,
) -> int:
    origin = scene.camera.position
    direction = scene.camera.ray_direction(pixel_x, pixel_y)
    segments = boundary_segments(
        scene, origin, direction, config.max_distance
    )
    view_samples = sum(
        1
        for _ in iter_samples(
            scene,
            origin,
            direction,
            segments,
            pixel_x,
            pixel_y,
            config.base_step,
            step_scale,
        )
    )
    return view_samples * (1 + config.shadow_samples)


def precision_error_bound(
    scene: Scene,
    samples_used: int,
    fixed_scale: int,
) -> float:
    """Conservative per-pixel bound for fixed-point accumulation."""
    light_energy = float(np.max(scene.light.radiance))
    background_energy = float(np.max(scene.background))
    phase_peak = 1.0 / (4.0 * math.pi * (1.0 - 0.95) ** 1.5)
    amplitude = max(1.0, light_energy * phase_peak + background_energy)
    operations = max(1, samples_used + 4)
    return 8.0 * operations * amplitude / fixed_scale
