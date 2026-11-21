"""Tiled rendering, budget planning, assembly, and deterministic summaries."""

from __future__ import annotations

import hashlib
import json
from concurrent.futures import FIRST_COMPLETED, Future, ThreadPoolExecutor, wait
from dataclasses import dataclass, field
from typing import Any, Callable

import numpy as np

from .config import RenderConfig
from .errors import AppError, ErrorCode
from .scene import Scene
from .volume import count_pixel_samples, precision_error_bound, trace_pixel


@dataclass(frozen=True, slots=True)
class TileSpec:
    index: int
    seq: int
    x0: int
    y0: int
    x1: int
    y1: int

    @property
    def width(self) -> int:
        return self.x1 - self.x0

    @property
    def height(self) -> int:
        return self.y1 - self.y0

    def to_dict(self) -> dict[str, int]:
        return {
            "index": self.index,
            "seq": self.seq,
            "x": self.x0,
            "y": self.y0,
            "w": self.width,
            "h": self.height,
        }


@dataclass(slots=True)
class TileResult:
    tile: TileSpec
    stage_index: int
    stage: str
    step_scale: float
    estimated_samples: int
    samples_used: int
    color: np.ndarray
    transmittance: np.ndarray
    precision: str

    def color_hash(self) -> str:
        return hashlib.sha256(
            np.ascontiguousarray(self.color, dtype="<f8").tobytes()
        ).hexdigest()

    def transmittance_hash(self) -> str:
        return hashlib.sha256(
            np.ascontiguousarray(self.transmittance, dtype="<f8").tobytes()
        ).hexdigest()

    def summary_dict(self) -> dict[str, Any]:
        payload = self.tile.to_dict()
        payload.update(
            {
                "stage_index": self.stage_index,
                "stage": self.stage,
                "step_scale": self.step_scale,
                "estimated_samples": self.estimated_samples,
                "samples_used": self.samples_used,
                "color_hash": self.color_hash(),
                "transmittance_hash": self.transmittance_hash(),
            }
        )
        return payload


@dataclass(slots=True)
class TilePlan:
    tile: TileSpec
    accepted: bool
    stage_index: int | None
    stage: str | None
    step_scale: float | None
    estimated_samples: int | None
    attempts: list[dict[str, Any]] = field(default_factory=list)


@dataclass(slots=True)
class RenderArtifact:
    scene: Scene
    config: RenderConfig
    precision: str
    tiles: list[TileResult]
    color: np.ndarray
    transmittance: np.ndarray
    summary: dict[str, Any]
    trace: list[dict[str, Any]]

    @property
    def color_bytes(self) -> bytes:
        return np.ascontiguousarray(self.color, dtype="<f8").tobytes()

    @property
    def transmittance_bytes(self) -> bytes:
        return np.ascontiguousarray(self.transmittance, dtype="<f8").tobytes()


def make_tiles(scene: Scene, config: RenderConfig) -> list[TileSpec]:
    tiles: list[TileSpec] = []
    index = 0
    for y0 in range(0, scene.height, config.tile_height):
        for x0 in range(0, scene.width, config.tile_width):
            tiles.append(
                TileSpec(
                    index=index,
                    seq=index,
                    x0=x0,
                    y0=y0,
                    x1=min(x0 + config.tile_width, scene.width),
                    y1=min(y0 + config.tile_height, scene.height),
                )
            )
            index += 1
    return tiles


def estimate_tile_samples(
    scene: Scene,
    tile: TileSpec,
    config: RenderConfig,
    step_scale: float,
) -> int:
    total = 0
    for y in range(tile.y0, tile.y1):
        for x in range(tile.x0, tile.x1):
            total += count_pixel_samples(
                scene, x, y, config, step_scale
            )
    return total


def plan_tiles(
    scene: Scene,
    tiles: list[TileSpec],
    config: RenderConfig,
    trace: list[dict[str, Any]],
) -> tuple[list[TilePlan], list[TileSpec]]:
    plans: list[TilePlan] = []
    rejected: list[TileSpec] = []
    for tile in sorted(tiles, key=lambda item: item.seq):
        attempts: list[dict[str, Any]] = []
        accepted_plan: TilePlan | None = None
        for stage_index, stage in enumerate(config.degradation_sequence):
            estimated = estimate_tile_samples(
                scene, tile, config, stage.step_scale
            )
            attempt = {
                "event": "budget_attempt",
                "tile_index": tile.index,
                "seq": tile.seq,
                "stage_index": stage_index,
                "stage": stage.name,
                "step_scale": stage.step_scale,
                "estimated_samples": estimated,
                "sample_budget": config.sample_budget,
                "accepted": estimated <= config.sample_budget,
            }
            attempts.append(attempt)
            trace.append(attempt)
            if estimated <= config.sample_budget:
                accepted_plan = TilePlan(
                    tile=tile,
                    accepted=True,
                    stage_index=stage_index,
                    stage=stage.name,
                    step_scale=stage.step_scale,
                    estimated_samples=estimated,
                    attempts=attempts,
                )
                break
        if accepted_plan is None:
            trace.append(
                {
                    "event": "wait_queue",
                    "tile_index": tile.index,
                    "seq": tile.seq,
                    "reason": "all declared degradation stages exceed budget",
                    "sample_budget": config.sample_budget,
                }
            )
            trace.append(
                {
                    "event": "reject_budget",
                    "tile_index": tile.index,
                    "seq": tile.seq,
                    "sample_budget": config.sample_budget,
                }
            )
            rejected.append(tile)
            plans.append(
                TilePlan(
                    tile=tile,
                    accepted=False,
                    stage_index=None,
                    stage=None,
                    step_scale=None,
                    estimated_samples=attempts[-1]["estimated_samples"],
                    attempts=attempts,
                )
            )
        else:
            trace.append(
                {
                    "event": "accept",
                    "tile_index": tile.index,
                    "seq": tile.seq,
                    "stage_index": accepted_plan.stage_index,
                    "stage": accepted_plan.stage,
                    "step_scale": accepted_plan.step_scale,
                    "estimated_samples": accepted_plan.estimated_samples,
                }
            )
            plans.append(accepted_plan)
    return plans, rejected


def render_tile(
    scene: Scene,
    tile: TileSpec,
    stage_index: int,
    config: RenderConfig,
    precision: str,
) -> TileResult:
    stage = config.stage(stage_index)
    color = np.zeros((tile.height, tile.width, 3), dtype=np.float64)
    transmittance = np.zeros((tile.height, tile.width), dtype=np.float64)
    used = 0
    view_samples = 0
    for local_y, y in enumerate(range(tile.y0, tile.y1)):
        for local_x, x in enumerate(range(tile.x0, tile.x1)):
            pixel = trace_pixel(
                scene,
                x,
                y,
                config,
                stage.step_scale,
                precision,
            )
            color[local_y, local_x] = pixel.color
            transmittance[local_y, local_x] = pixel.transmittance
            used += pixel.samples_used
            view_samples += pixel.view_samples
    _ = view_samples
    return TileResult(
        tile=tile,
        stage_index=stage_index,
        stage=stage.name,
        step_scale=stage.step_scale,
        estimated_samples=0,
        samples_used=used,
        color=color,
        transmittance=transmittance,
        precision=precision,
    )


def _assemble(
    scene: Scene,
    tiles: list[TileResult],
) -> tuple[np.ndarray, np.ndarray]:
    color = np.zeros((scene.height, scene.width, 3), dtype=np.float64)
    transmittance = np.zeros((scene.height, scene.width), dtype=np.float64)
    for result in tiles:
        tile = result.tile
        color[tile.y0 : tile.y1, tile.x0 : tile.x1] = result.color
        transmittance[tile.y0 : tile.y1, tile.x0 : tile.x1] = (
            result.transmittance
        )
    return color, transmittance


def _canonical_digest(
    color: np.ndarray, transmittance: np.ndarray
) -> str:
    digest = hashlib.sha256()
    digest.update(np.ascontiguousarray(color, dtype="<f8").tobytes())
    digest.update(np.ascontiguousarray(transmittance, dtype="<f8").tobytes())
    return digest.hexdigest()


def _base_trace(
    scene: Scene, config: RenderConfig, precision: str
) -> list[dict[str, Any]]:
    return [
        {
            "event": "schedule_start",
            "scene_id": scene.scene_id,
            "scene_fingerprint": scene.fingerprint,
            "config_digest": config.digest(),
            "precision": precision,
            "max_workers": config.max_workers,
            "sample_budget": config.sample_budget,
        }
    ]


def render_frame(
    scene: Scene,
    config: RenderConfig,
    precision: str,
    completed: dict[int, TileResult] | None = None,
    selected_tile_indices: set[int] | None = None,
    checkpoint: Callable[[dict[int, TileResult], list[dict[str, Any]]], None]
    | None = None,
    stop_after: int | None = None,
) -> RenderArtifact:
    if precision not in {"float64", "fixed"}:
        raise AppError(
            ErrorCode.E_USAGE,
            f"unsupported precision path: {precision}",
            field="precision",
        )
    trace = _base_trace(scene, config, precision)
    tiles = make_tiles(scene, config)
    completed = dict(completed or {})
    completed_results: dict[int, TileResult] = {}
    for index, result in completed.items():
        if index in {tile.index for tile in tiles}:
            completed_results[index] = result
            trace.append(
                {
                    "event": "resume_skip",
                    "tile_index": index,
                    "seq": result.tile.seq,
                    "color_hash": result.color_hash(),
                }
            )
    remaining_tiles = [
        tile
        for tile in tiles
        if tile.index not in completed_results
        and (
            selected_tile_indices is None
            or tile.index in selected_tile_indices
        )
    ]
    if selected_tile_indices is not None:
        unknown = sorted(
            selected_tile_indices - {tile.index for tile in tiles}
        )
        if unknown:
            raise AppError(
                ErrorCode.E_USAGE,
                "selected tile indices are outside the chunk table",
                details={"unknown_tile_indices": unknown},
            )
    plans, rejected = plan_tiles(scene, remaining_tiles, config, trace)
    rejected_details = [
        {
            "tile_index": tile.index,
            "seq": tile.seq,
            "x": tile.x0,
            "y": tile.y0,
            "w": tile.width,
            "h": tile.height,
        }
        for tile in rejected
    ]
    if rejected:
        raise AppError(
            ErrorCode.E_BUDGET_EXCEEDED,
            "one or more tiles exceed every declared sampling budget stage",
            field="sample_budget",
            details={
                "rejected_tiles": rejected_details,
                "trace": trace,
                "output_written": False,
            },
        )

    accepted_plans = [plan for plan in plans if plan.accepted]
    accepted_plans.sort(key=lambda plan: plan.tile.seq)
    for plan in accepted_plans:
        trace.append(
            {
                "event": "submit",
                "tile_index": plan.tile.index,
                "seq": plan.tile.seq,
                "stage_index": plan.stage_index,
                "stage": plan.stage,
                "step_scale": plan.step_scale,
            }
        )

    results_by_index = dict(completed_results)
    total_tiles = len(tiles)
    if accepted_plans:
        executor = ThreadPoolExecutor(
            max_workers=config.max_workers,
            thread_name_prefix="volrender",
        )
        futures: dict[Future[TileResult], TilePlan] = {}
        try:
            for plan in accepted_plans:
                future = executor.submit(
                    render_tile,
                    scene,
                    plan.tile,
                    plan.stage_index,
                    config,
                    precision,
                )
                futures[future] = plan
            pending = set(futures)
            processed = 0
            while pending:
                done, pending = wait(pending, return_when=FIRST_COMPLETED)
                for future in sorted(
                    done, key=lambda item: futures[item].tile.seq
                ):
                    plan = futures[future]
                    try:
                        result = future.result()
                    except Exception as exc:
                        trace.append(
                            {
                                "event": "tile_failed",
                                "tile_index": plan.tile.index,
                                "seq": plan.tile.seq,
                                "error": type(exc).__name__,
                                "message": str(exc),
                            }
                        )
                        raise
                    if result.samples_used != plan.estimated_samples:
                        raise AppError(
                            ErrorCode.E_BUDGET_EXCEEDED,
                            "preflight sample count diverged from render usage",
                            field="sample_budget",
                            details={
                                "tile_index": plan.tile.index,
                                "estimated_samples": plan.estimated_samples,
                                "actual_samples": result.samples_used,
                            },
                        )
                    result.estimated_samples = int(plan.estimated_samples or 0)
                    results_by_index[result.tile.index] = result
                    trace.append(
                        {
                            "event": "complete",
                            "tile_index": result.tile.index,
                            "seq": result.tile.seq,
                            "stage_index": result.stage_index,
                            "stage": result.stage,
                            "step_scale": result.step_scale,
                            "samples_used": result.samples_used,
                            "color_hash": result.color_hash(),
                        }
                    )
                    processed += 1
                    if checkpoint is not None:
                        checkpoint(dict(results_by_index), trace)
                    if (
                        stop_after is not None
                        and len(results_by_index) >= stop_after
                    ):
                        for queued in pending:
                            queued.cancel()
                        raise SimulatedTermination(
                            f"simulated termination after "
                            f"{len(results_by_index)} tiles"
                        )
        finally:
            executor.shutdown(wait=True, cancel_futures=True)

    if len(results_by_index) != total_tiles:
        missing = sorted(
            set(tile.index for tile in tiles) - set(results_by_index)
        )
        raise AppError(
            ErrorCode.E_RECOVERY_INVALID,
            "render finished with missing tiles",
            details={"missing_tile_indices": missing},
        )
    ordered = [
        results_by_index[tile.index]
        for tile in sorted(tiles, key=lambda item: item.seq)
    ]
    color, transmittance = _assemble(scene, ordered)
    summary = build_render_summary(
        scene, config, precision, ordered, color, transmittance, trace
    )
    return RenderArtifact(
        scene=scene,
        config=config,
        precision=precision,
        tiles=ordered,
        color=color,
        transmittance=transmittance,
        summary=summary,
        trace=trace,
    )


def build_render_summary(
    scene: Scene,
    config: RenderConfig,
    precision: str,
    results: list[TileResult],
    color: np.ndarray,
    transmittance: np.ndarray,
    trace: list[dict[str, Any]],
) -> dict[str, Any]:
    total_samples = sum(result.samples_used for result in results)
    total_budget = config.sample_budget * len(results)
    degraded = [
        result.tile.index
        for result in results
        if result.stage_index > 0
    ]
    summary: dict[str, Any] = {
        "summary_version": 1,
        "scene_id": scene.scene_id,
        "scene_fingerprint": scene.fingerprint,
        "density_version": scene.density_version,
        "config_digest": config.digest(),
        "precision": precision,
        "image": {"width": scene.width, "height": scene.height},
        "tile_size": {
            "width": config.tile_width,
            "height": config.tile_height,
        },
        "parallelism": {"max_workers": config.max_workers},
        "budget": {
            "per_tile": config.sample_budget,
            "total": total_budget,
            "used": total_samples,
            "utilization": (
                total_samples / total_budget if total_budget else 0.0
            ),
        },
        "degradation_sequence": [
            {"name": stage.name, "step_scale": stage.step_scale}
            for stage in config.degradation_sequence
        ],
        "degraded_tile_indices": degraded,
        "tiles": [result.summary_dict() for result in results],
        "buffer_digest": _canonical_digest(color, transmittance),
    }
    return summary


def summary_bytes(summary: dict[str, Any]) -> bytes:
    return (
        json.dumps(
            summary,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=False,
            allow_nan=False,
        )
        + "\n"
    ).encode("utf-8")


def render_reference(
    scene: Scene,
    config: RenderConfig,
    stage_by_pixel: np.ndarray,
    precision: str,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Render the whole screen in one call for seam truth comparison."""
    color = np.zeros((scene.height, scene.width, 3), dtype=np.float64)
    transmittance = np.zeros((scene.height, scene.width), dtype=np.float64)
    samples = np.zeros((scene.height, scene.width), dtype=np.int64)
    for y in range(scene.height):
        for x in range(scene.width):
            stage_index = int(stage_by_pixel[y, x])
            step_scale = config.stage(stage_index).step_scale
            pixel = trace_pixel(
                scene, x, y, config, step_scale, precision
            )
            color[y, x] = pixel.color
            transmittance[y, x] = pixel.transmittance
            samples[y, x] = pixel.samples_used
    return color, transmittance, samples


def compare_precision(
    scene: Scene,
    reference: RenderArtifact,
    fixed: RenderArtifact,
) -> tuple[dict[str, Any], np.ndarray, np.ndarray, np.ndarray]:
    absolute = np.abs(reference.color - fixed.color)
    transmittance_absolute = np.abs(
        reference.transmittance - fixed.transmittance
    )
    sample_counts = np.zeros_like(absolute[:, :, 0], dtype=np.int64)
    stage_by_tile = {
        result.tile.index: result.stage_index
        for result in reference.tiles
    }
    for result in reference.tiles:
        tile = result.tile
        step_scale = reference.config.stage(
            stage_by_tile[tile.index]
        ).step_scale
        for y in range(tile.y0, tile.y1):
            for x in range(tile.x0, tile.x1):
                sample_counts[y, x] = count_pixel_samples(
                    scene,
                    x,
                    y,
                    reference.config,
                    step_scale,
                )
    bound = np.empty_like(absolute[:, :, 0], dtype=np.float64)
    for y in range(scene.height):
        for x in range(scene.width):
            bound[y, x] = precision_error_bound(
                scene,
                int(sample_counts[y, x]),
                reference.config.fixed_scale,
            )
    within = np.all(absolute <= bound[:, :, None], axis=2)
    within &= transmittance_absolute <= bound
    nonzero = absolute[absolute > 0.0]
    summary: dict[str, Any] = {
        "precision_reference": reference.precision,
        "precision_compared": fixed.precision,
        "fixed_scale": reference.config.fixed_scale,
        "pixel_count": int(absolute.shape[0] * absolute.shape[1]),
        "max_abs_error": float(np.max(absolute)),
        "mean_abs_error": float(np.mean(absolute)),
        "rmse": float(np.sqrt(np.mean(absolute * absolute))),
        "max_transmittance_abs_error": float(
            np.max(transmittance_absolute)
        ),
        "mean_transmittance_abs_error": float(
            np.mean(transmittance_absolute)
        ),
        "p95_abs_error": (
            float(np.percentile(nonzero, 95)) if nonzero.size else 0.0
        ),
        "max_upper_bound": float(np.max(bound)),
        "mean_upper_bound": float(np.mean(bound)),
        "pixels_within_bound": int(np.count_nonzero(within)),
        "all_within_bound": bool(np.all(within)),
        "per_channel_max_abs": [
            float(np.max(absolute[:, :, channel])) for channel in range(3)
        ],
    }
    return summary, absolute, transmittance_absolute, bound


class SimulatedTermination(RuntimeError):
    """Raised by recovery tests to emulate a forced process stop."""
