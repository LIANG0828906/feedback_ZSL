"""Deterministic tile rendering, precision paths and seam validation."""

from __future__ import annotations

import math
import threading
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from functools import lru_cache
from typing import Any

import numpy as np

from .binary import build_scene, validate_scene
from .config import RenderConfig, validate_config
from .errors import BudgetError, BusinessError, CancelledError, ErrorCode, SeamError
from .geometry import media_segments, scene_shadow_tmax
from .hashing import hash_arrays, hash_json, sha256_bytes
from .model import Medium, Scene, density_sample
from .planning import PixelPlan, combined_coefficients, plan_tile
from .scheduler import (
    Admission,
    ScheduleResult,
    Tile,
    partition_screen,
    schedule_tiles,
)

FIXED_SCALE = 1 << 20
TONE_MAP_EXPOSURE = 8.0


@dataclass
class TileDependencies:
    camera_hash: str
    light_hash: str
    medium_hashes: dict[int, str]
    density_chunk_hashes: dict[tuple[int, int, int, int], str]

    def to_dict(self) -> dict[str, Any]:
        return {
            "camera_hash": self.camera_hash,
            "light_hash": self.light_hash,
            "medium_hashes": {
                str(key): value for key, value in sorted(self.medium_hashes.items())
            },
            "density_chunk_hashes": {
                "|".join(str(part) for part in key): value
                for key, value in sorted(self.density_chunk_hashes.items())
            },
        }


@dataclass
class TileResult:
    tile: Tile
    radiance: np.ndarray
    scatter: np.ndarray
    transmittance: np.ndarray
    sample_counts: np.ndarray
    stage: str
    step_multiplier: float
    degradation_index: int
    sample_count: int
    content_hash: str
    dependencies: TileDependencies
    trials: list[dict[str, Any]] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "tile_id": self.tile.tile_id,
            "submit_seq": self.tile.sequence,
            "x": self.tile.x,
            "y": self.tile.y,
            "width": self.tile.width,
            "height": self.tile.height,
            "level": self.tile.level,
            "parent": self.tile.parent,
            "stage": self.stage,
            "step_multiplier": self.step_multiplier,
            "degradation_index": self.degradation_index,
            "sample_count": self.sample_count,
            "max_pixel_samples": int(self.sample_counts.max(initial=0)),
            "content_hash": self.content_hash,
            "trials": self.trials,
        }


@dataclass
class SeamCheck:
    enabled: bool
    checked_pixels: int
    max_scatter_delta: float
    max_transmittance_delta: float
    tolerance: float
    truth_stage: str
    truth_samples: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "enabled": self.enabled,
            "checked_pixels": self.checked_pixels,
            "max_scatter_delta": self.max_scatter_delta,
            "max_transmittance_delta": self.max_transmittance_delta,
            "tolerance": self.tolerance,
            "truth_stage": self.truth_stage,
            "truth_samples": self.truth_samples,
        }


@dataclass
class RenderResult:
    radiance: np.ndarray
    scatter: np.ndarray
    transmittance: np.ndarray
    image_ppm: bytes
    summary: dict[str, Any]
    trace: list[dict[str, Any]]
    tile_results: list[TileResult]
    schedule: ScheduleResult
    precision_delta: np.ndarray | None = None

    @property
    def pixel_hash(self) -> str:
        return self.summary["pixel_hash"]

    @property
    def image_hash(self) -> str:
        return self.summary["image_hash"]


@lru_cache(maxsize=None)
def _cached_medium_hash(
    medium_id: int,
    kind: int,
    density_field_id: int,
    sigma_s: bytes,
    sigma_a: bytes,
    g: float,
    density_scale: float,
    shape_a: bytes,
    shape_b: bytes,
) -> str:
    return hash_json(
        {
            "id": medium_id,
            "kind": kind,
            "density_field_id": density_field_id,
            "sigma_s": np.frombuffer(sigma_s, dtype=np.float64).tolist(),
            "sigma_a": np.frombuffer(sigma_a, dtype=np.float64).tolist(),
            "g": g,
            "density_scale": density_scale,
            "shape_a": np.frombuffer(shape_a, dtype=np.float64).tolist(),
            "shape_b": np.frombuffer(shape_b, dtype=np.float64).tolist(),
        }
    )


def medium_hash(medium: Medium) -> str:
    return _cached_medium_hash(
        medium.id,
        medium.kind,
        medium.density_field_id,
        np.ascontiguousarray(medium.sigma_s).tobytes(),
        np.ascontiguousarray(medium.sigma_a).tobytes(),
        float(medium.g),
        float(medium.density_scale),
        np.ascontiguousarray(medium.shape_a).tobytes(),
        np.ascontiguousarray(medium.shape_b).tobytes(),
    )


def _scene_content_hashes(scene: Scene) -> tuple[str, str, dict[int, str]]:
    camera_hash = hash_arrays(
        [
            ("intrinsics", scene.camera.intrinsics),
            ("world_to_camera", scene.camera.world_to_camera),
        ]
    )
    light_hash = hash_arrays(
        [("direction", scene.light.direction), ("radiance", scene.light.radiance)]
    )
    medium_hashes = {medium.id: medium_hash(medium) for medium in scene.media}
    return camera_hash, light_hash, medium_hashes


def _hg_phase(cos_theta: float, g: float) -> float:
    denom = 1.0 + g * g - 2.0 * g * cos_theta
    if denom <= 1e-12:
        denom = 1e-12
    return (1.0 - g * g) / (4.0 * math.pi * denom * math.sqrt(denom))


def _shadow_visibility(
    scene: Scene,
    point: np.ndarray,
    light_direction: np.ndarray,
    shadow_samples: int,
    scene_bounds: tuple[np.ndarray, np.ndarray] | None,
    media_by_id: dict[int, Medium],
) -> tuple[
    np.ndarray,
    set[tuple[int, int, int, int]],
    set[int],
]:
    """Estimate shadow transmittance and return touched media/chunks."""

    if shadow_samples <= 0:
        return np.ones(3, dtype=np.float64), set(), set()
    tmax = scene_shadow_tmax(scene, point, light_direction, scene_bounds)
    if tmax <= 0.0:
        return np.ones(3, dtype=np.float64), set(), set()
    segments = media_segments(scene, point, light_direction)
    optical = np.zeros(3, dtype=np.float64)
    touched: set[tuple[int, int, int, int]] = set()
    touched_media: set[int] = set()
    dt = tmax / float(shadow_samples)
    for index in range(shadow_samples):
        t = (index + 0.5) * dt
        active: tuple[int, ...] = ()
        for segment in segments:
            if segment.t0 <= t <= segment.t1:
                active = segment.medium_ids
                break
        if not active:
            continue
        touched_media.update(active)
        sample_point = point + t * light_direction
        _density, sigma_s, sigma_a, chunks = combined_coefficients(
            scene, sample_point, active, media_by_id
        )
        touched.update(chunks)
        optical += (sigma_s + sigma_a) * dt
    return np.exp(-optical), touched, touched_media


def _render_tile(
    scene: Scene,
    admission: Admission,
    config: RenderConfig,
    cancel_event: threading.Event | None,
) -> TileResult:
    tile = admission.tile
    plan = admission.plan
    height = tile.height
    width = tile.width
    scatter = np.zeros((height, width, 3), dtype=np.float64)
    transmittance = np.ones((height, width, 3), dtype=np.float64)
    sample_counts = np.zeros((height, width), dtype=np.int64)
    touched_chunks: set[tuple[int, int, int, int]] = set()
    medium_ids: set[int] = set()
    light_direction = scene.light.unit_direction
    light_rgb = scene.light.radiance
    precision_fixed = config.precision == "fixed"
    sample_counter = 0
    scene_bounds = scene.scene_bounds
    media_by_id = scene.media_by_id()

    for local_y in range(height):
        if cancel_event is not None and cancel_event.is_set():
            raise CancelledError()
        for local_x in range(width):
            pixel_plan: PixelPlan = plan.rows[local_y][local_x]
            if cancel_event is not None and cancel_event.is_set():
                raise CancelledError()
            origin, direction = scene.camera.ray_for_pixel(
                tile.x + local_x, tile.y + local_y
            )
            medium_ids.update(pixel_plan.medium_ids)
            touched_chunks.update(pixel_plan.touched_chunks)
            if precision_fixed:
                trans_q = np.full(3, FIXED_SCALE, dtype=np.int64)
                radiance_q = np.zeros(3, dtype=np.int64)
                for sample in pixel_plan.samples:
                    point = origin + sample.t * direction
                    visibility, shadow_chunks, shadow_media = _shadow_visibility(
                        scene,
                        point,
                        light_direction,
                        pixel_plan.shadow_samples,
                        scene_bounds,
                        media_by_id,
                    )
                    touched_chunks.update(shadow_chunks)
                    medium_ids.update(shadow_media)
                    sigma_t = sample.sigma_s + sample.sigma_a
                    cos_theta = float(np.dot(direction, light_direction))
                    phase = _hg_phase(cos_theta, media_by_id[pixel_plan.medium_ids[0]].g) if pixel_plan.medium_ids else 1.0 / (4.0 * math.pi)
                    for channel in range(3):
                        current_t = trans_q[channel] / float(FIXED_SCALE)
                        term = (
                            current_t
                            * sample.sigma_s[channel]
                            * phase
                            * light_rgb[channel]
                            * visibility[channel]
                            * sample.dt
                        )
                        radiance_q[channel] += int(round(term * FIXED_SCALE))
                        attenuation = math.exp(-float(sigma_t[channel]) * sample.dt)
                        trans_q[channel] = max(
                            0,
                            min(
                                FIXED_SCALE,
                                int(round(trans_q[channel] * attenuation)),
                            ),
                        )
                    sample_counter += 1
                    if sample_counter % 256 == 0 and cancel_event is not None and cancel_event.is_set():
                        raise CancelledError()
                scatter[local_y, local_x] = radiance_q.astype(np.float64) / FIXED_SCALE
                transmittance[local_y, local_x] = trans_q.astype(np.float64) / FIXED_SCALE
            else:
                trans = np.ones(3, dtype=np.float64)
                radiance = np.zeros(3, dtype=np.float64)
                for sample in pixel_plan.samples:
                    point = origin + sample.t * direction
                    visibility, shadow_chunks, shadow_media = _shadow_visibility(
                        scene,
                        point,
                        light_direction,
                        pixel_plan.shadow_samples,
                        scene_bounds,
                        media_by_id,
                    )
                    touched_chunks.update(shadow_chunks)
                    medium_ids.update(shadow_media)
                    sigma_t = sample.sigma_s + sample.sigma_a
                    cos_theta = float(np.dot(direction, light_direction))
                    if pixel_plan.medium_ids:
                        # Geometry is strictly nested; the first active medium
                        # defines the deterministic preview phase function.
                        g = media_by_id[pixel_plan.medium_ids[0]].g
                        phase = _hg_phase(cos_theta, g)
                    else:
                        phase = 1.0 / (4.0 * math.pi)
                    radiance += (
                        trans
                        * sample.sigma_s
                        * phase
                        * light_rgb
                        * visibility
                        * sample.dt
                    )
                    trans *= np.exp(-sigma_t * sample.dt)
                    sample_counter += 1
                    if sample_counter % 256 == 0 and cancel_event is not None and cancel_event.is_set():
                        raise CancelledError()
                scatter[local_y, local_x] = radiance
                transmittance[local_y, local_x] = trans
            sample_counts[local_y, local_x] = pixel_plan.total_count

    camera_hash, light_hash, all_medium_hashes = _scene_content_hashes(scene)
    medium_hashes = {
        medium_id: all_medium_hashes[medium_id] for medium_id in sorted(medium_ids)
    }
    chunk_hashes = {
        key: hash_arrays(
            [
                (
                    "chunk",
                    scene.density_fields[key[0]].chunks[(key[1], key[2], key[3])],
                )
            ]
        )
        for key in sorted(touched_chunks)
    }
    dependencies = TileDependencies(
        camera_hash=camera_hash,
        light_hash=light_hash,
        medium_hashes=medium_hashes,
        density_chunk_hashes=chunk_hashes,
    )
    content_hash = hash_arrays(
        [
            ("radiance", scatter),
            ("transmittance", transmittance),
            ("sample_counts", sample_counts),
        ]
    )
    return TileResult(
        tile=tile,
        radiance=scatter,
        scatter=scatter.copy(),
        transmittance=transmittance,
        sample_counts=sample_counts,
        stage=admission.stage.name,
        step_multiplier=admission.stage.step_multiplier,
        degradation_index=admission.degradation_index,
        sample_count=admission.sample_count,
        content_hash=content_hash,
        dependencies=dependencies,
        trials=[trial.to_dict() for trial in admission.trials],
    )


def _tone_map_ppm(radiance: np.ndarray) -> bytes:
    mapped = np.clip(radiance * TONE_MAP_EXPOSURE, 0.0, None)
    mapped = mapped / (1.0 + mapped)
    mapped = np.power(mapped, 1.0 / 2.2)
    pixels = np.clip(np.rint(mapped * 255.0), 0, 255).astype(np.uint8)
    height, width, _ = pixels.shape
    header = f"P6\n{width} {height}\n255\n".encode("ascii")
    return header + pixels.tobytes(order="C")


def _stitch(
    scene: Scene, tile_results: list[TileResult]
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    scatter = np.zeros((scene.height, scene.width, 3), dtype=np.float64)
    trans = np.ones((scene.height, scene.width, 3), dtype=np.float64)
    sample_counts = np.zeros((scene.height, scene.width), dtype=np.int64)
    for result in sorted(tile_results, key=lambda item: item.tile.sequence):
        tile = result.tile
        scatter[tile.y : tile.y + tile.height, tile.x : tile.x + tile.width] = (
            result.scatter
        )
        trans[tile.y : tile.y + tile.height, tile.x : tile.x + tile.width] = (
            result.transmittance
        )
        sample_counts[
            tile.y : tile.y + tile.height, tile.x : tile.x + tile.width
        ] = result.sample_counts
    return scatter, trans, sample_counts


def _neighbour_pair(
    tiles_by_id: dict[str, Tile], tile: Tile, pixel_x: int, pixel_y: int
) -> tuple[str, str]:
    pairs: list[tuple[str, str]] = []
    if pixel_x == tile.x and tile.x > 0:
        pairs.append((tile.tile_id, f"@{tile.x - 1},{pixel_y}"))
    elif pixel_x == tile.x + tile.width - 1 and tile.x + tile.width < 1 << 30:
        pairs.append((tile.tile_id, f"@{tile.x + tile.width},{pixel_y}"))
    if pixel_y == tile.y and tile.y > 0:
        pairs.append((tile.tile_id, f"@{pixel_x},{tile.y - 1}"))
    elif pixel_y == tile.y + tile.height - 1:
        pairs.append((tile.tile_id, f"@{pixel_x},{tile.y + tile.height}"))
    neighbour = pairs[0][1] if pairs else "@neighbor"
    for candidate in tiles_by_id.values():
        if (
            tile.x + tile.width == candidate.x
            and candidate.y <= pixel_y < candidate.y + candidate.height
        ) or (
            candidate.x + candidate.width == tile.x
            and candidate.y <= pixel_y < candidate.y + candidate.height
        ):
            neighbour = candidate.tile_id
            break
        if (
            tile.y + tile.height == candidate.y
            and candidate.x <= pixel_x < candidate.x + candidate.width
        ) or (
            candidate.y + candidate.height == tile.y
            and candidate.x <= pixel_x < candidate.x + candidate.width
        ):
            neighbour = candidate.tile_id
            break
    return tile.tile_id, neighbour


def _check_seams(
    scene: Scene,
    tile_results: list[TileResult],
    truth: TileResult,
    config: RenderConfig,
) -> SeamCheck:
    if len(tile_results) <= 1 or not config.seam.enabled:
        return SeamCheck(
            enabled=False,
            checked_pixels=0,
            max_scatter_delta=0.0,
            max_transmittance_delta=0.0,
            tolerance=config.seam.tolerance,
            truth_stage=config.seam.truth_stage,
            truth_samples=truth.sample_count,
        )
    tiles_by_id = {result.tile.tile_id: result.tile for result in tile_results}
    checked = 0
    max_scatter = 0.0
    max_trans = 0.0
    for result in sorted(tile_results, key=lambda item: item.tile.sequence):
        tile = result.tile
        for local_y in range(tile.height):
            for local_x in range(tile.width):
                gx = tile.x + local_x
                gy = tile.y + local_y
                boundary = (
                    (gx == tile.x and tile.x > 0)
                    or (gx == tile.x + tile.width - 1 and gx < scene.width - 1)
                    or (gy == tile.y and tile.y > 0)
                    or (gy == tile.y + tile.height - 1 and gy < scene.height - 1)
                )
                if not boundary:
                    continue
                checked += 1
                scatter_delta = float(
                    np.max(np.abs(result.scatter[local_y, local_x] - truth.scatter[gy, gx]))
                )
                trans_delta = float(
                    np.max(
                        np.abs(
                            result.transmittance[local_y, local_x]
                            - truth.transmittance[gy, gx]
                        )
                    )
                )
                max_scatter = max(max_scatter, scatter_delta)
                max_trans = max(max_trans, trans_delta)
                if scatter_delta > config.seam.tolerance or trans_delta > config.seam.tolerance:
                    pair = _neighbour_pair(tiles_by_id, tile, gx, gy)
                    metric = (
                        "scatter"
                        if scatter_delta >= trans_delta
                        else "transmittance"
                    )
                    raise SeamError(
                        "tile boundary differs from whole-screen truth",
                        {
                            "tile_pair": list(pair),
                            "pixel": [gx, gy],
                            "metric": metric,
                            "scatter_delta": scatter_delta,
                            "transmittance_delta": trans_delta,
                            "tolerance": config.seam.tolerance,
                            "stage": result.stage,
                        },
                    )
    return SeamCheck(
        enabled=True,
        checked_pixels=checked,
        max_scatter_delta=max_scatter,
        max_transmittance_delta=max_trans,
        tolerance=config.seam.tolerance,
        truth_stage=config.seam.truth_stage,
        truth_samples=truth.sample_count,
    )


def _render_truth(
    scene: Scene,
    config: RenderConfig,
    cancel_event: threading.Event | None,
) -> TileResult:
    stage = config.stage(config.seam.truth_stage)
    if stage is None:
        raise BusinessError(
            ErrorCode.CONFIG_INVALID,
            "seam truth stage does not exist",
            field="seam.truth_stage",
        )
    try:
        plan = plan_tile(
            scene,
            0,
            0,
            scene.width,
            scene.height,
            stage,
            config,
            budget=config.seam.truth_budget,
            cancel_event=cancel_event,
        )
    except Exception as exc:
        from .planning import PlanBudgetExceeded

        if isinstance(exc, PlanBudgetExceeded):
            raise BudgetError(
                "whole-screen seam truth exceeds its declared diagnostic budget",
                {
                    "scope": "seam_truth",
                    "budget": config.seam.truth_budget,
                    "measured_samples": exc.measured_samples,
                },
            ) from exc
        raise
    tile = Tile(0, "truth", 0, 0, scene.width, scene.height)
    admission = Admission(tile, stage, plan, plan.total_samples, [])
    return _render_tile(scene, admission, config, cancel_event)


def _execute_admissions(
    scene: Scene,
    admissions: list[Admission],
    config: RenderConfig,
    cancel_event: threading.Event | None,
) -> list[TileResult]:
    results: list[TileResult] = []
    if not admissions:
        return results
    workers = min(config.max_parallelism, len(admissions))
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="volrender") as pool:
        futures = {
            pool.submit(_render_tile, scene, admission, config, cancel_event): admission
            for admission in admissions
        }
        try:
            for future in as_completed(futures):
                results.append(future.result())
        except Exception:
            if cancel_event is not None:
                cancel_event.set()
            for future in futures:
                future.cancel()
            raise
    results.sort(key=lambda item: item.tile.sequence)
    return results


def _precision_statistics(
    float_radiance: np.ndarray, fixed_radiance: np.ndarray, max_samples: int
) -> dict[str, Any]:
    delta = np.abs(float_radiance - fixed_radiance)
    flat = delta.reshape(-1)
    quantum = 1.0 / FIXED_SCALE
    analytic = (max_samples + 2) * quantum * 3.0
    upper = max(float(np.max(flat, initial=0.0)), analytic)
    return {
        "mode_a": "float",
        "mode_b": "fixed",
        "fixed_scale": FIXED_SCALE,
        "max_abs": float(np.max(flat, initial=0.0)),
        "mean_abs": float(np.mean(flat)),
        "rmse": float(np.sqrt(np.mean(np.square(flat)))),
        "p95_abs": float(np.percentile(flat, 95)),
        "error_upper_bound": float(upper),
        "per_channel_max_abs": [
            float(np.max(delta[..., channel], initial=0.0)) for channel in range(3)
        ],
    }


def _core_summary(
    scene: Scene,
    config: RenderConfig,
    tile_results: list[TileResult],
    schedule: ScheduleResult,
    scatter: np.ndarray,
    transmittance: np.ndarray,
    sample_counts: np.ndarray,
    seam_check: SeamCheck,
    image_bytes: bytes,
    extra: dict[str, Any] | None = None,
) -> dict[str, Any]:
    image_hash = sha256_bytes(image_bytes)
    pixel_hash = hash_arrays(
        [
            ("radiance", scatter),
            ("transmittance", transmittance),
            ("sample_counts", sample_counts),
        ]
    )
    stages: dict[str, int] = {}
    for result in tile_results:
        stages[result.stage] = stages.get(result.stage, 0) + 1
    tile_records = [
        result.to_dict()
        for result in sorted(tile_results, key=lambda item: item.tile.sequence)
    ]
    summary: dict[str, Any] = {
        "schema": "volrender.summary.v1",
        "scene": {
            "width": scene.width,
            "height": scene.height,
            "binary_hash": sha256_bytes(build_scene(scene)),
        },
        "config": config.to_dict(),
        "config_hash": config.content_hash,
        "precision": config.precision,
        "parallelism": config.max_parallelism,
        "budget": {
            "per_tile": config.sample_budget,
            "total_consumed": int(sample_counts.sum()),
            "max_pixel_samples": int(sample_counts.max(initial=0)),
        },
        "sampling": {
            "base_step": config.base_step,
            "min_step": config.min_step,
            "adaptive_gain": 3.0,
            "phase": "global_pixel_coordinate",
        },
        "error_summary": {
            "mode": config.precision,
            "quantization_step": (
                1.0 / FIXED_SCALE if config.precision == "fixed" else 0.0
            ),
            "error_upper_bound": (
                (int(sample_counts.max(initial=0)) + 2) / FIXED_SCALE * 3.0
                if config.precision == "fixed"
                else 0.0
            ),
            "comparison_available": False,
        },
        "hierarchy": {
            "root": {
                "level": 1,
                "width": scene.width,
                "height": scene.height,
                "tile_size": config.tile_size,
            },
            "leaves": [
                {
                    "tile_id": result.tile.tile_id,
                    "level": result.tile.level,
                    "parent": result.tile.parent,
                }
                for result in sorted(tile_results, key=lambda item: item.tile.sequence)
            ],
        },
        "degradation": {
            "counts_by_stage": dict(sorted(stages.items())),
            "declared_sequence": [
                stage.to_dict() for stage in config.degrade_sequence
            ],
        },
        "tiles": tile_records,
        "seam_check": seam_check.to_dict(),
        "pixel_hash": pixel_hash,
        "image_hash": image_hash,
        "render_hash": hash_arrays(
            [
                ("radiance", scatter),
                ("transmittance", transmittance),
                ("sample_counts", sample_counts),
            ]
        ),
    }
    if extra:
        summary.update(extra)
    summary["summary_hash"] = hash_json(summary)
    return summary


def _render_impl(
    scene: Scene,
    config: RenderConfig,
    validate_seams: bool,
    cancel_event: threading.Event | None,
) -> RenderResult:
    tiles = partition_screen(scene.width, scene.height, config.tile_size)
    schedule = schedule_tiles(scene, tiles, config, cancel_event)
    if schedule.waiting:
        raise BudgetError(
            "one or more tiles exceed every declared degradation stage",
            {
                "budget": config.sample_budget,
                "waiting": [task.to_dict() for task in schedule.waiting],
                "decision_count": len(schedule.decisions),
            },
        )
    results = _execute_admissions(scene, schedule.admitted, config, cancel_event)
    scatter, transmittance, sample_counts = _stitch(scene, results)
    truth: TileResult | None = None
    if validate_seams and len(results) > 1 and config.seam.enabled:
        truth = _render_truth(scene, config, cancel_event)
        seam_check = _check_seams(scene, results, truth, config)
    else:
        seam_check = SeamCheck(
            enabled=False,
            checked_pixels=0,
            max_scatter_delta=0.0,
            max_transmittance_delta=0.0,
            tolerance=config.seam.tolerance,
            truth_stage=config.seam.truth_stage,
            truth_samples=truth.sample_count if truth is not None else 0,
        )
    image_bytes = _tone_map_ppm(scatter)
    summary = _core_summary(
        scene,
        config,
        results,
        schedule,
        scatter,
        transmittance,
        sample_counts,
        seam_check,
        image_bytes,
    )
    trace: list[dict[str, Any]] = []
    admissions_by_seq = {item.tile.sequence: item for item in schedule.admitted}
    for decision in schedule.decisions:
        trace.append(decision)
    for result in results:
        admission = admissions_by_seq[result.tile.sequence]
        trace.append(
            {
                "event": "tile_committed",
                "tile_id": result.tile.tile_id,
                "submit_seq": result.tile.sequence,
                "bounds": [
                    result.tile.x,
                    result.tile.y,
                    result.tile.width,
                    result.tile.height,
                ],
                "stage": result.stage,
                "degradation_index": result.degradation_index,
                "step_multiplier": admission.stage.step_multiplier,
                "base_step": config.base_step,
                "effective_base_step": config.base_step * admission.stage.step_multiplier,
                "budget": config.sample_budget,
                "sample_count": result.sample_count,
                "max_pixel_samples": int(result.sample_counts.max(initial=0)),
                "content_hash": result.content_hash,
                "dependencies": result.dependencies.to_dict(),
            }
        )
    trace.sort(key=lambda record: (record["submit_seq"], 0 if record["event"] == "schedule" else 1))
    return RenderResult(
        radiance=scatter,
        scatter=scatter,
        transmittance=transmittance,
        image_ppm=image_bytes,
        summary=summary,
        trace=trace,
        tile_results=results,
        schedule=schedule,
    )


def render_scene(
    scene: Scene,
    config: RenderConfig,
    validate_seams: bool = True,
    compare_precision: bool = False,
    cancel_event: threading.Event | None = None,
) -> RenderResult:
    validate_scene(scene)
    validate_config(config)
    result = _render_impl(scene, config, validate_seams, cancel_event)
    if compare_precision:
        other_precision = "fixed" if config.precision == "float" else "float"
        other_config = config.with_precision(other_precision)
        cancel_event = cancel_event or threading.Event()
        other = _render_impl(scene, other_config, False, cancel_event)
        if config.precision == "float":
            float_result, fixed_result = result, other
        else:
            float_result, fixed_result = other, result
        stats = _precision_statistics(
            float_result.scatter,
            fixed_result.scatter,
            max(int(result.summary["budget"]["max_pixel_samples"]), int(other.summary["budget"]["max_pixel_samples"])),
        )
        result.precision_delta = np.abs(float_result.scatter - fixed_result.scatter)
        result.summary["precision_comparison"] = stats
        result.summary["error_summary"]["comparison_available"] = True
        result.summary["error_summary"]["max_observed_abs"] = stats["max_abs"]
        result.summary["precision_comparison_summary_hashes"] = {
            "float": float_result.summary["summary_hash"],
            "fixed": fixed_result.summary["summary_hash"],
        }
        result.summary["summary_hash"] = ""
        result.summary["summary_hash"] = hash_json(
            {key: value for key, value in result.summary.items() if key != "summary_hash"}
        )
    return result


__all__ = [
    "FIXED_SCALE",
    "RenderResult",
    "SeamCheck",
    "TileDependencies",
    "TileResult",
    "medium_hash",
    "render_scene",
]
