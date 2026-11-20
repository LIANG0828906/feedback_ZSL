"""Deterministic volumetric single-pixel and chunk rendering."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Callable

import numpy as np

from .chunks import Chunk
from .config import RenderConfig
from .errors import E_CANCELLED, DustRenderError
from .geometry import Segment, segment_media
from .scene import Medium, Scene

BACKGROUND_CONSTANT = np.asarray([0.0275, 0.03, 0.039], dtype=np.float64)
BACKGROUND_VERTICAL = np.asarray([-0.0025, 0.01, 0.031], dtype=np.float64)
FIXED_SCALE = float(2**18)


@dataclass
class ChunkResult:
    chunk: Chunk
    rgb: np.ndarray
    transmittance: np.ndarray
    primary_samples: int
    shadow_samples: int
    step: float
    max_segments: int

    @property
    def samples(self) -> int:
        return self.primary_samples + self.shadow_samples


@dataclass(frozen=True)
class SampleEstimate:
    chunk_id: str
    primary: int
    shadow: int
    max_segments: int

    @property
    def total(self) -> int:
        return self.primary + self.shadow


def background_color(scene: Scene, direction: np.ndarray) -> np.ndarray:
    return scene.ambient + BACKGROUND_CONSTANT + float(direction[1]) * BACKGROUND_VERTICAL


def _quantize(value: np.ndarray | float, precision: str) -> np.ndarray | float:
    if precision == "float":
        return value
    return np.rint(np.asarray(value) * FIXED_SCALE) / FIXED_SCALE


def _aggregate_media(
    scene: Scene, active: tuple[Medium, ...], point: np.ndarray
) -> tuple[np.ndarray, float, float]:
    scattering_rgb = np.zeros(3, dtype=np.float64)
    sigma_t = 0.0
    density_sum = 0.0
    for medium in active:
        density = medium.density(scene.density_fields, point)
        if density <= 0.0:
            continue
        scattering_rgb += medium.scattering * density
        sigma_t += (medium.absorption[0] + medium.scattering[0]) * density
        density_sum += density
    return scattering_rgb, max(0.0, sigma_t), density_sum


def _phase_value(active: tuple[Medium, ...], view_dir: np.ndarray, light_dir: np.ndarray) -> float:
    if not active:
        return 1.0
    cosine = float(np.clip(np.dot(view_dir, light_dir), -1.0, 1.0))
    phase = 0.0
    weight = 0.0
    for medium in active:
        g = float(medium.phase_g)
        denominator = max(1e-8, 1.0 + g * g - 2.0 * g * cosine)
        phase += (1.0 - g * g) / (denominator**1.5)
        weight += 1.0
    return phase / max(weight, 1.0)


def _visibility(
    scene: Scene,
    point: np.ndarray,
    light_direction: np.ndarray,
    shadow_samples: int,
    shadow_distance: float,
) -> float:
    extent = shadow_distance / float(shadow_samples)
    optical_depth = 0.0
    for index in range(shadow_samples):
        distance = (index + 0.5) * extent
        sample = point + light_direction * distance
        _, sigma_t, _ = _aggregate_media(scene, tuple(scene.media), sample)
        optical_depth += sigma_t * extent
    return math.exp(-max(0.0, optical_depth))


def _iter_segment_steps(
    scene: Scene,
    config: RenderConfig,
    origin: np.ndarray,
    segment: Segment,
    step_base: float,
    primary_direction: np.ndarray,
) -> list[tuple[np.ndarray, float, float, float]]:
    """Yield (point, step, sigma_t, density) with deterministic global phase."""

    samples: list[tuple[np.ndarray, float, float, float]] = []
    step = min(
        config.max_step,
        max(config.min_step, step_base),
    )
    # The phase is anchored to the global ray parameter, never to a chunk origin.
    # Using a fraction of min_step keeps the sample count stable while still letting
    # the configured phase shift the first density lookup.
    position = segment.start + config.phase * config.min_step
    if position >= segment.end:
        position = segment.start
    epsilon = 1e-10
    while position < segment.end + epsilon:
        world_point = origin + position * primary_direction
        scattering, sigma_t, density = _aggregate_media(
            scene, segment.media, world_point
        )
        del scattering
        if density > 0 and sigma_t > 0:
            step = step_base / (1.0 + config.density_adapt_gain * density)
            step = min(config.max_step, max(config.min_step, step))
        else:
            step = min(config.max_step, max(config.min_step, step_base))
        remaining = segment.end - position
        if remaining <= epsilon:
            break
        used_step = min(step, remaining)
        samples.append((world_point, used_step, sigma_t, density))
        position += step
    return samples


def _render_pixel(
    scene: Scene,
    config: RenderConfig,
    x: int,
    y: int,
    step_base: float,
    *,
    count_only: bool,
) -> tuple[np.ndarray, float, int, int, int]:
    origin, direction = scene.camera.ray(x, y)
    segments = segment_media(
        scene.media, origin, direction, scene.camera.near, scene.camera.far
    )
    background = background_color(scene, direction)
    color = np.zeros(3, dtype=np.float64)
    transmittance = 1.0
    primary = 0
    shadow = 0
    max_segments = len(segments)
    light_direction = scene.light.direction
    phase = _phase_value(
        tuple(medium for segment in segments for medium in segment.media[:1]),
        direction,
        light_direction,
    )
    for segment in segments:
        for point, step, sigma_t, _ in _iter_segment_steps(
            scene, config, origin, segment, step_base, direction
        ):
            primary += 1
            shadow += config.shadow_samples
            if count_only:
                continue
            visibility = _visibility(
                scene,
                point,
                light_direction,
                config.shadow_samples,
                config.shadow_distance,
            )
            scattering_rgb, _, _ = _aggregate_media(scene, segment.media, point)
            if sigma_t <= 0.0:
                continue
            step_trans = math.exp(-sigma_t * step)
            source = (
                scattering_rgb
                * scene.light.color
                * scene.light.intensity
                * phase
                * visibility
                * step
            )
            if config.precision == "fixed":
                transmittance = float(_quantize(transmittance, "fixed"))
                color = np.asarray(_quantize(color, "fixed"), dtype=np.float64)
                source = np.asarray(_quantize(source, "fixed"), dtype=np.float64)
                step_trans = float(_quantize(step_trans, "fixed"))
            color += transmittance * source
            transmittance *= step_trans
            if config.precision == "fixed":
                transmittance = float(_quantize(transmittance, "fixed"))
                color = np.asarray(_quantize(color, "fixed"), dtype=np.float64)
            if transmittance < 1e-8:
                transmittance = 0.0
                break
    if not count_only:
        color += transmittance * background
        if config.precision == "fixed":
            color = np.asarray(_quantize(color, "fixed"), dtype=np.float64)
        color = np.maximum(color, 0.0)
    else:
        color = np.zeros(3, dtype=np.float64)
    return color, transmittance, primary, shadow, max_segments


def estimate_chunk(
    scene: Scene,
    config: RenderConfig,
    chunk: Chunk,
    step_base: float | None = None,
) -> SampleEstimate:
    base = config.base_step if step_base is None else step_base
    primary = 0
    shadow = 0
    max_segments = 0
    for y in range(chunk.y0, chunk.y1):
        for x in range(chunk.x0, chunk.x1):
            _, _, p, s, segments = _render_pixel(
                scene, config, x, y, base, count_only=True
            )
            primary += p
            shadow += s
            max_segments = max(max_segments, segments)
    return SampleEstimate(chunk.id, primary, shadow, max_segments)


def render_chunk(
    scene: Scene,
    config: RenderConfig,
    chunk: Chunk,
    step_base: float | None = None,
    *,
    cancel_check: Callable[[], bool] | None = None,
) -> ChunkResult:
    base = config.base_step if step_base is None else step_base
    rgb = np.zeros(chunk.content_shape, dtype=np.float32)
    trans = np.ones(chunk.transmittance_shape, dtype=np.float32)
    primary = 0
    shadow = 0
    max_segments = 0
    for local_y, y in enumerate(range(chunk.y0, chunk.y1)):
        if cancel_check is not None and cancel_check():
            raise DustRenderError(E_CANCELLED, "render cancelled cooperatively", "$render")
        for local_x, x in enumerate(range(chunk.x0, chunk.x1)):
            color, visibility, p, s, segments = _render_pixel(
                scene,
                config,
                x,
                y,
                base,
                count_only=False,
            )
            rgb[local_y, local_x] = np.asarray(color, dtype=np.float32)
            trans[local_y, local_x] = np.float32(visibility)
            primary += p
            shadow += s
            max_segments = max(max_segments, segments)
    return ChunkResult(
        chunk=chunk,
        rgb=rgb,
        transmittance=trans,
        primary_samples=primary,
        shadow_samples=shadow,
        step=float(base),
        max_segments=max_segments,
    )


def fixed_point_upper_bound(config: RenderConfig, max_primary_samples: int) -> float:
    quantum = 1.0 / FIXED_SCALE
    operations = max(1, int(max_primary_samples)) * (1 + int(config.shadow_samples))
    return quantum * operations * 8.0
