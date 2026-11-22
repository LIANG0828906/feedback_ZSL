"""Deterministic per-pixel sampling plans and budget preflight."""

from __future__ import annotations

import threading
from dataclasses import dataclass

import numpy as np

from .config import DegradeStage, RenderConfig
from .errors import CancelledError
from .geometry import media_segments, sample_phase
from .model import DensityField, Medium, Scene, density_sample

ADAPTIVE_GAIN = 3.0


class PlanBudgetExceeded(Exception):
    def __init__(self, measured_samples: int, budget: int) -> None:
        self.measured_samples = measured_samples
        self.budget = budget
        super().__init__(f"plan exceeded budget after {measured_samples} samples")


@dataclass(frozen=True)
class SamplePoint:
    t: float
    dt: float
    density_total: float
    sigma_s: np.ndarray
    sigma_a: np.ndarray


@dataclass
class PixelPlan:
    samples: list[SamplePoint]
    phase: float
    medium_ids: tuple[int, ...]
    shadow_samples: int
    primary_count: int
    total_count: int
    touched_chunks: set[tuple[int, int, int, int]]


@dataclass
class TilePlan:
    rows: list[list[PixelPlan]]
    primary_samples: int
    shadow_samples: int
    total_samples: int
    max_pixel_samples: int


def _combined_coefficients(
    scene: Scene,
    media_by_id: dict[int, Medium],
    point: np.ndarray,
    active_ids: tuple[int, ...],
) -> tuple[float, np.ndarray, np.ndarray, set[tuple[int, int, int, int]]]:
    sigma_s = np.zeros(3, dtype=np.float64)
    sigma_a = np.zeros(3, dtype=np.float64)
    density_total = 0.0
    touched: set[tuple[int, int, int, int]] = set()
    for medium_id in active_ids:
        medium = media_by_id[medium_id]
        field: DensityField = scene.density_fields[medium.density_field_id]
        lo, hi = medium.bounds
        density, chunks = density_sample(field, point, lo, hi)
        density *= medium.density_scale
        if density != 0.0:
            sigma_s += medium.sigma_s * density
            sigma_a += medium.sigma_a * density
        density_total += density
        touched.update((field.id, *key) for key in chunks)
    return density_total, sigma_s, sigma_a, touched


def combined_coefficients(
    scene: Scene,
    point: np.ndarray,
    active_ids: tuple[int, ...],
    media_by_id: dict[int, Medium] | None = None,
) -> tuple[float, np.ndarray, np.ndarray, set[tuple[int, int, int, int]]]:
    return _combined_coefficients(
        scene,
        scene.media_by_id() if media_by_id is None else media_by_id,
        point,
        active_ids,
    )


def plan_pixel(
    scene: Scene,
    media_by_id: dict[int, Medium],
    pixel_x: int,
    pixel_y: int,
    stage: DegradeStage,
    config: RenderConfig,
    budget_remaining: int,
    cancel_event: threading.Event | None,
) -> PixelPlan:
    origin, direction = scene.camera.ray_for_pixel(pixel_x, pixel_y)
    segments = media_segments(scene, origin, direction)
    phase = sample_phase(pixel_x, pixel_y)
    base_step = config.base_step * stage.step_multiplier
    shadow_count = stage.shadow_samples
    samples: list[SamplePoint] = []
    medium_ids: set[int] = set()
    touched: set[tuple[int, int, int, int]] = set()
    primary_count = 0
    shadow_samples = 0
    budget_measured = 0

    for segment in segments:
        medium_ids.update(segment.medium_ids)
        position = segment.t0
        step = min(base_step, segment.t1 - position)
        if step <= 0.0:
            continue
        while position < segment.t1 - 1e-15:
            if cancel_event is not None and cancel_event.is_set():
                raise CancelledError()
            step = min(step, segment.t1 - position)
            if step <= 0.0:
                break
            sample_t = position + phase * step
            sample_t = min(max(sample_t, position), segment.t1)
            point = origin + sample_t * direction
            density, sigma_s, sigma_a, chunks = _combined_coefficients(
                scene, media_by_id, point, segment.medium_ids
            )
            touched.update(chunks)
            samples.append(SamplePoint(sample_t, step, density, sigma_s, sigma_a))
            primary_count += 1
            shadow_samples += shadow_count
            budget_measured += 1 + shadow_count
            if budget_measured > budget_remaining:
                raise PlanBudgetExceeded(budget_measured, config.sample_budget)
            position += step
            next_step = base_step / (1.0 + ADAPTIVE_GAIN * max(density, 0.0))
            step = max(config.min_step, next_step)

    total = primary_count + shadow_samples
    if total != budget_measured:
        raise AssertionError("sampling plan budget accounting mismatch")
    return PixelPlan(
        samples=samples,
        phase=phase,
        medium_ids=tuple(sorted(medium_ids)),
        shadow_samples=shadow_count,
        primary_count=primary_count,
        total_count=total,
        touched_chunks=touched,
    )


def plan_tile(
    scene: Scene,
    tile_x: int,
    tile_y: int,
    tile_width: int,
    tile_height: int,
    stage: DegradeStage,
    config: RenderConfig,
    budget: int | None = None,
    cancel_event: threading.Event | None = None,
) -> TilePlan:
    budget_left = config.sample_budget if budget is None else int(budget)
    primary_total = 0
    shadow_total = 0
    max_pixel = 0
    rows: list[list[PixelPlan]] = []
    media_by_id = scene.media_by_id()
    for local_y in range(tile_height):
        row: list[PixelPlan] = []
        for local_x in range(tile_width):
            if cancel_event is not None and cancel_event.is_set():
                raise CancelledError()
            plan = plan_pixel(
                scene,
                media_by_id,
                tile_x + local_x,
                tile_y + local_y,
                stage,
                config,
                budget_left - primary_total - shadow_total,
                cancel_event,
            )
            row.append(plan)
            primary_total += plan.primary_count
            shadow_total += plan.shadow_samples
            max_pixel = max(max_pixel, plan.total_count)
        rows.append(row)
    return TilePlan(
        rows=rows,
        primary_samples=primary_total,
        shadow_samples=shadow_total,
        total_samples=primary_total + shadow_total,
        max_pixel_samples=max_pixel,
    )
