from __future__ import annotations

import math
import threading
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .atlas import (
    AtlasGlyph,
    AtlasResult,
    build_atlas,
    write_atlas_png,
    write_coverage_pgm,
)
from .config import WorkbenchConfig
from .errors import ErrorCode, GlyphForgeError, RunCancelled
from .model import Font, Glyph
from .raster import (
    CoverageSummary,
    GlyphGeometry,
    TileCoverage,
    compose_tiles,
    coverage_content_hash,
    prepare_geometry,
    raster_region,
    sample_count_for_rect,
    temporary_scanline_bytes,
)
from .scheduler import (
    DeterministicScheduler,
    GlyphSubmission,
    TaskMeasurement,
    TaskOutcome,
    TileTask,
)
from .snapshot import SnapshotData, SnapshotStore, load_snapshot
from .util import (
    atomic_write_bytes,
    atomic_write_json,
    canonical_json,
)


@dataclass(frozen=True)
class QuantizationComparison:
    previous_step: float
    current_step: float
    previous_abs_error: float
    current_abs_error: float
    abs_error_delta: float
    previous_coverage_sum: float
    current_coverage_sum: float
    coverage_sum_delta: float

    def to_dict(self) -> dict[str, float]:
        return {
            "previous_step": self.previous_step,
            "current_step": self.current_step,
            "previous_abs_error": self.previous_abs_error,
            "current_abs_error": self.current_abs_error,
            "abs_error_delta": self.abs_error_delta,
            "previous_coverage_sum": self.previous_coverage_sum,
            "current_coverage_sum": self.current_coverage_sum,
            "coverage_sum_delta": self.coverage_sum_delta,
        }


@dataclass
class GlyphRunResult:
    name: str
    glyph_index: int
    unicode: tuple[int, ...]
    status: str
    advance: float
    font_bbox: tuple[float, float, float, float]
    pixel_bbox: tuple[int, int, int, int]
    coverage: np.ndarray | None
    summary: CoverageSummary | None
    coverage_hash: str | None
    truth_hash: str | None
    tile_count: int
    reused_tiles: int
    recomputed_tiles: int
    error: GlyphForgeError | None
    comparison: QuantizationComparison | None
    tile_events: list[dict[str, Any]] = field(default_factory=list)

    @property
    def successful(self) -> bool:
        return self.status == "ok"

    def to_dict(self) -> dict[str, Any]:
        return {
            "glyph": self.name,
            "index": self.glyph_index,
            "unicode": list(self.unicode),
            "status": self.status,
            "advance": self.advance,
            "bbox": list(self.font_bbox),
            "pixel_bbox": list(self.pixel_bbox),
            "coverage_hash": self.coverage_hash,
            "truth_hash": self.truth_hash,
            "tile_count": self.tile_count,
            "reused_tiles": self.reused_tiles,
            "recomputed_tiles": self.recomputed_tiles,
            "quantization": self.summary.to_dict() if self.summary else None,
            "comparison": self.comparison.to_dict() if self.comparison else None,
            "error": self.error.to_dict() if self.error else None,
        }


@dataclass
class PipelineResult:
    font: Font
    config: WorkbenchConfig
    text: str
    glyphs: list[GlyphRunResult]
    atlas: AtlasResult
    trace_events: list[dict[str, Any]]
    snapshot_completed_tiles: int
    snapshot_chain_hash: str | None
    snapshot_path: str | None
    output_dir: str | None = None

    @property
    def errors(self) -> list[GlyphForgeError]:
        return [glyph.error for glyph in self.glyphs if glyph.error is not None]

    @property
    def successful_glyph_count(self) -> int:
        return sum(1 for glyph in self.glyphs if glyph.successful)

    @property
    def reuse_count(self) -> int:
        return sum(glyph.reused_tiles for glyph in self.glyphs)

    @property
    def recompute_count(self) -> int:
        return sum(glyph.recomputed_tiles for glyph in self.glyphs)

    def summary_dict(self) -> dict[str, Any]:
        return {
            "format": "glyphforge.workbench-result/1",
            "font": {
                "name": Path(self.font.path).name,
                "format": self.font.format,
                "source_hash": self.font.source_hash,
                "units_per_em": self.font.units_per_em,
            },
            "text": self.text,
            "config": self.config.to_dict(),
            "glyphs": [glyph.to_dict() for glyph in self.glyphs],
            "atlas": {
                "width": self.atlas.width,
                "height": self.atlas.height,
                "alpha_hash": self.atlas.alpha_hash,
                "metrics": [metric.to_dict() for metric in self.atlas.metrics],
            },
            "counts": {
                "glyphs": len(self.glyphs),
                "successful_glyphs": self.successful_glyph_count,
                "reused_tiles": self.reuse_count,
                "recomputed_tiles": self.recompute_count,
                "snapshot_completed_tiles": self.snapshot_completed_tiles,
            },
            "errors": [error.to_dict() for error in self.errors],
            "snapshot": {
                "file": Path(self.snapshot_path).name if self.snapshot_path else None,
                "chain_hash": self.snapshot_chain_hash,
            },
            "trace_file": "trace.jsonl" if self.output_dir else None,
        }

    def trace_text(self) -> str:
        return "".join(
            canonical_json(event) + "\n" for event in self.trace_events
        )

    def write_outputs(self, output_dir: str | Path) -> None:
        target = Path(output_dir)
        target.mkdir(parents=True, exist_ok=True)
        self.output_dir = str(target)
        coverage_dir = target / "coverage"
        for glyph in self.glyphs:
            if glyph.coverage is not None and glyph.successful:
                write_coverage_pgm(
                    coverage_dir / f"{_safe_filename(glyph.name)}.pgm",
                    glyph.coverage,
                )
        write_atlas_png(target / "atlas.png", self.atlas)
        metrics = {
            "format": "glyphforge.atlas-metrics/1",
            "atlas": {"width": self.atlas.width, "height": self.atlas.height},
            "glyphs": [metric.to_dict() for metric in self.atlas.metrics],
        }
        atomic_write_json(target / "metrics.json", metrics)
        atomic_write_json(target / "report.json", self.summary_dict())
        atomic_write_bytes(target / "trace.jsonl", self.trace_text().encode("utf-8"))


@dataclass
class SceneGlyph:
    glyph: Glyph
    index: int
    geometry: GlyphGeometry
    tiles: list[tuple[int, int, int, int, int, int]]


def _safe_filename(name: str) -> str:
    safe = "".join(
        character if character.isalnum() or character in "-_." else "_"
        for character in name
    )
    return safe or "glyph"


def select_glyphs(font: Font, text: str) -> list[Glyph]:
    selected: list[Glyph] = []
    seen: set[str] = set()
    for position, character in enumerate(text):
        glyph = font.glyph_for_codepoint(ord(character))
        if glyph is None:
            raise GlyphForgeError(
                ErrorCode.E_GLYPH_INVALID,
                "text contains a codepoint with no Unicode mapping",
                {
                    "glyph": None,
                    "contour_index": None,
                    "character": character,
                    "codepoint": ord(character),
                    "text_position": position,
                },
            )
        if glyph.name not in seen:
            seen.add(glyph.name)
            selected.append(glyph)
    return selected


def _build_scene(font: Font, text: str, config: WorkbenchConfig) -> list[SceneGlyph]:
    options = config.raster_options(font.units_per_em)
    scene: list[SceneGlyph] = []
    for index, glyph in enumerate(select_glyphs(font, text)):
        geometry = prepare_geometry(glyph, options)
        scene.append(
            SceneGlyph(
                glyph=glyph,
                index=index,
                geometry=geometry,
                tiles=geometry.tile_rects(config.tile_size),
            )
        )
    return scene


def _task_for_tile(
    scene_glyph: SceneGlyph,
    tile: tuple[int, int, int, int, int, int],
    supersample: int,
) -> TileTask:
    row, col, x, y, width, height = tile
    return TileTask(
        glyph_name=scene_glyph.glyph.name,
        glyph_index=scene_glyph.index,
        row=row,
        col=col,
        x=x,
        y=y,
        width=width,
        height=height,
        geometry=scene_glyph.geometry,
        sample_count=sample_count_for_rect(width, height, supersample),
        coverage_buffer_bytes=width * height * 4,
    )


def _snapshot_run_config(
    font: Font,
    config: WorkbenchConfig,
    scene: list[SceneGlyph],
) -> dict[str, Any]:
    tile_table = []
    for scene_glyph in scene:
        for row, col, x, y, width, height in scene_glyph.tiles:
            tile_table.append(
                {
                    "glyph": scene_glyph.glyph.name,
                    "row": row,
                    "col": col,
                    "x": x,
                    "y": y,
                    "width": width,
                    "height": height,
                }
            )
    return {
        "font_hash": font.source_hash,
        "glyph_order": [scene_glyph.glyph.name for scene_glyph in scene],
        "tile_size": config.tile_size,
        "curve_tolerance": config.curve_tolerance,
        "raster": {
            "pixel_size": config.pixel_size,
            "subpixel_offset": [config.subpixel_x, config.subpixel_y],
            "supersample": config.supersample,
            "quant_step": config.quant_step,
            "fill_rule": config.fill_rule,
        },
        "budget": {
            "parallelism": config.budget.parallelism,
            "global_memory_bytes": config.budget.global_memory_bytes,
            "max_task_samples": config.budget.max_task_samples,
            "max_task_memory_bytes": config.budget.max_task_memory_bytes,
            "sampling_batch_size": config.budget.sampling_batch_size,
            "min_sampling_batch_size": config.budget.min_sampling_batch_size,
            "degrade_sequence": list(config.budget.degrade_sequence),
        },
        "coverage_tolerance": config.coverage_tolerance,
        "tile_table": tile_table,
    }


def _reused_tiles(
    snapshot: SnapshotData | None,
    scene: list[SceneGlyph],
    run_config: dict[str, Any],
) -> dict[str, TileCoverage]:
    if snapshot is None:
        return {}
    if snapshot.run_config != run_config:
        raise GlyphForgeError(
            ErrorCode.E_SNAPSHOT_MISMATCH,
            "snapshot run configuration does not match the requested workbench run",
            {"snapshot": Path(snapshot.path).name},
        )
    expected: dict[str, tuple[int, int, int, int, int]] = {}
    for scene_glyph in scene:
        for row, col, x, y, width, height in scene_glyph.tiles:
            expected[f"{scene_glyph.glyph.name}:{row}:{col}"] = (
                x,
                y,
                width,
                height,
                config_quant_step(run_config),
            )
    for key, tile in snapshot.tiles.items():
        descriptor = expected.get(key)
        if descriptor is None:
            raise GlyphForgeError(
                ErrorCode.E_SNAPSHOT_MISMATCH,
                "snapshot contains a tile outside the current tile table",
                {"snapshot_tile": key},
            )
        x, y, width, height, quant_step = descriptor
        if (
            tile.x != x
            or tile.y != y
            or tile.width != width
            or tile.height != height
            or tile.summary.quant_step != quant_step
        ):
            raise GlyphForgeError(
                ErrorCode.E_SNAPSHOT_MISMATCH,
                "snapshot tile descriptor does not match the current tile table",
                {"snapshot_tile": key},
            )
    return dict(snapshot.tiles)


def config_quant_step(run_config: Mapping[str, Any]) -> float:
    return float(run_config["raster"]["quant_step"])


def _measurement_for_reused(tile: TileCoverage) -> TaskMeasurement:
    return TaskMeasurement(
        sample_count=tile.summary.samples,
        coverage_buffer_bytes=tile.width * tile.height * 4,
        temporary_scanline_bytes=temporary_scanline_bytes(tile.batch_size),
        total_bytes=(
            tile.width * tile.height * 4
            + temporary_scanline_bytes(tile.batch_size)
        ),
        requested_batch_size=tile.batch_size,
        effective_batch_size=tile.batch_size,
    )


def _comparison(
    previous: Mapping[str, Any] | None,
    current: CoverageSummary,
) -> QuantizationComparison | None:
    if previous is None:
        return None
    try:
        previous_step = float(previous.get("quant_step"))
        previous_error = float(previous.get("quantization_abs_error"))
        previous_sum = float(previous.get("raw_coverage_sum"))
    except (TypeError, ValueError):
        return None
    if math.isclose(previous_step, current.quant_step, rel_tol=0.0, abs_tol=0.0):
        return None
    return QuantizationComparison(
        previous_step=previous_step,
        current_step=current.quant_step,
        previous_abs_error=previous_error,
        current_abs_error=current.quantization_abs_error,
        abs_error_delta=current.quantization_abs_error - previous_error,
        previous_coverage_sum=previous_sum,
        current_coverage_sum=current.raw_coverage_sum,
        coverage_sum_delta=current.raw_coverage_sum - previous_sum,
    )


def _coverage_mismatch(
    glyph_name: str,
    geometry: GlyphGeometry,
    tiled: np.ndarray,
    truth: np.ndarray,
    tile_size: int,
    tolerance: int,
) -> GlyphForgeError | None:
    if tiled.shape != truth.shape:
        return GlyphForgeError(
            ErrorCode.E_COVERAGE_MISMATCH,
            "tiled coverage shape differs from the whole-glyph truth image",
            {
                "glyph": glyph_name,
                "tiled_shape": list(tiled.shape),
                "truth_shape": list(truth.shape),
                "over_tolerance_pixels": int(max(tiled.size, truth.size)),
            },
        )
    if not tiled.size:
        return None
    difference = np.abs(tiled.astype(np.int16) - truth.astype(np.int16))
    bad = difference > tolerance
    count = int(bad.sum())
    if count == 0:
        return None
    first_local = np.argwhere(bad)[0]
    local_row, local_col = int(first_local[0]), int(first_local[1])
    x_boundaries = [
        geometry.x0 + index * tile_size
        for index in range((geometry.width + tile_size - 1) // tile_size + 1)
    ]
    y_boundaries = [
        geometry.y0 + index * tile_size
        for index in range((geometry.height + tile_size - 1) // tile_size + 1)
    ]
    global_x = geometry.x0 + local_col
    global_y = geometry.y0 + local_row
    seam_x = min(x_boundaries, key=lambda value: abs(value - global_x))
    seam_y = min(y_boundaries, key=lambda value: abs(value - global_y))
    return GlyphForgeError(
        ErrorCode.E_COVERAGE_MISMATCH,
        "tiled coverage exceeds the declared tolerance against whole-glyph truth",
        {
            "glyph": glyph_name,
            "tile": {
                "row": local_row // tile_size,
                "col": local_col // tile_size,
            },
            "seam": {"x": seam_x, "y": seam_y},
            "first_pixel": {"x": global_x, "y": global_y},
            "over_tolerance_pixels": count,
            "max_delta": int(difference.max()),
            "tolerance": tolerance,
        },
    )


def run_pipeline(
    font: Font,
    text: str,
    config: WorkbenchConfig,
    *,
    output_dir: str | Path | None = None,
    snapshot_path: str | Path | None = None,
    resume: bool = False,
    cancel_event: threading.Event | None = None,
    previous_summaries: Mapping[str, Mapping[str, Any]] | None = None,
    progress: Callable[[dict[str, Any]], None] | None = None,
) -> PipelineResult:
    config.validate()
    options = config.raster_options(font.units_per_em)
    scene = _build_scene(font, text, config)
    run_config = _snapshot_run_config(font, config, scene)

    snapshot: SnapshotData | None = None
    snapshot_store: SnapshotStore | None = None
    snapshot_file = Path(snapshot_path) if snapshot_path else None
    if snapshot_file is not None:
        if resume:
            if not snapshot_file.exists():
                raise GlyphForgeError(
                    ErrorCode.E_SNAPSHOT_MISMATCH,
                    "resume requested but the snapshot file does not exist",
                    {"snapshot": snapshot_file.name},
                )
            snapshot = load_snapshot(snapshot_file)
        reused = _reused_tiles(snapshot, scene, run_config)
        snapshot_store = SnapshotStore(
            snapshot_file,
            run_config,
            initial_tiles=reused,
        )
    else:
        reused = {}

    if progress is not None:
        progress(
            {
                "event": "run_start",
                "glyphs": [scene_glyph.glyph.name for scene_glyph in scene],
                "parallelism": config.budget.parallelism,
                "snapshot": snapshot_file.name if snapshot_file else None,
            }
        )
    if cancel_event is not None and cancel_event.is_set():
        raise RunCancelled(str(snapshot_file) if snapshot_file else None)

    submissions: list[GlyphSubmission] = []
    all_tasks: dict[str, TileTask] = {}
    reused_by_glyph: dict[str, int] = {scene_glyph.glyph.name: 0 for scene_glyph in scene}
    for scene_glyph in scene:
        missing: list[TileTask] = []
        for tile in scene_glyph.tiles:
            task = _task_for_tile(scene_glyph, tile, config.supersample)
            key = f"{task.glyph_name}:{task.row}:{task.col}"
            all_tasks[key] = task
            if key in reused:
                reused_by_glyph[task.glyph_name] += 1
            else:
                missing.append(task)
        if missing:
            submissions.append(
                GlyphSubmission(
                    name=scene_glyph.glyph.name,
                    glyph_index=scene_glyph.index,
                    tasks=missing,
                )
            )

    scheduler = DeterministicScheduler(config.budget)
    schedule_plan = scheduler.plan(submissions)
    glyph_plan_by_name = {plan.name: plan for plan in schedule_plan.glyphs}

    if progress is not None:
        for glyph_plan in schedule_plan.glyphs:
            progress(
                {
                    "event": "glyph_plan",
                    "glyph": glyph_plan.name,
                    "accepted": glyph_plan.accepted,
                    "reused_tiles": reused_by_glyph.get(glyph_plan.name, 0),
                    "tiles_to_compute": len(glyph_plan.tasks),
                }
            )

    def on_tile(outcome: TaskOutcome) -> None:
        if progress is not None:
            progress(
                {
                    "event": "tile",
                    "glyph": outcome.task.glyph_name,
                    "tile": {"row": outcome.task.row, "col": outcome.task.col},
                    "status": outcome.status,
                    "snapshot_completed_tiles": (
                        snapshot_store.completed_count
                        if snapshot_store is not None
                        else 0
                    ),
                }
            )

    execution = scheduler.execute(
        schedule_plan,
        options,
        snapshot_store=snapshot_store,
        cancel_event=cancel_event,
        on_tile=on_tile,
    )
    if execution.cancelled:
        raise RunCancelled(str(snapshot_file) if snapshot_file else None)

    outcomes_by_key = {
        f"{outcome.task.glyph_name}:{outcome.task.row}:{outcome.task.col}": outcome
        for outcome in execution.outcomes
    }
    task_plan_by_key = {
        f"{task_plan.task.glyph_name}:{task_plan.task.row}:{task_plan.task.col}": task_plan
        for glyph_plan in schedule_plan.glyphs
        for task_plan in glyph_plan.tasks
    }

    glyph_results: list[GlyphRunResult] = []
    for scene_glyph in scene:
        glyph = scene_glyph.glyph
        glyph_plan = glyph_plan_by_name.get(glyph.name)
        tiles_for_glyph: list[TileCoverage] = []
        outcomes_for_glyph: list[TaskOutcome] = []
        tile_events: list[dict[str, Any]] = []
        glyph_error: GlyphForgeError | None = None
        if glyph_plan is not None and not glyph_plan.accepted:
            glyph_error = glyph_plan.rejection
        for tile in sorted(scene_glyph.tiles, key=lambda item: (item[0], item[1])):
            row, col, *_ = tile
            key = f"{glyph.name}:{row}:{col}"
            task = all_tasks[key]
            if key in reused:
                reused_tile = reused[key]
                tiles_for_glyph.append(reused_tile)
                tile_events.append(
                    {
                        "event": "tile",
                        "glyph": glyph.name,
                        "tile": {"row": row, "col": col},
                        "status": "reused",
                        "measurement": _measurement_for_reused(reused_tile).to_dict(),
                        "content_hash": reused_tile.content_hash,
                    }
                )
                continue
            task_plan = task_plan_by_key.get(key)
            outcome = outcomes_by_key.get(key)
            if task_plan is not None and not task_plan.accepted:
                if glyph_error is None:
                    glyph_error = task_plan.rejection
                tile_events.append(
                    {
                        "event": "tile",
                        "glyph": glyph.name,
                        "tile": {"row": row, "col": col},
                        "status": task_plan.status,
                        "measurement": (
                            task_plan.measurement.to_dict()
                            if task_plan.measurement
                            else None
                        ),
                        "error": (
                            task_plan.rejection.to_dict()
                            if task_plan.rejection
                            else None
                        ),
                    }
                )
                continue
            if outcome is None:
                if glyph_error is None:
                    glyph_error = GlyphForgeError(
                        ErrorCode.E_COVERAGE_MISMATCH,
                        "tile was not completed",
                        {
                            "glyph": glyph.name,
                            "tile": {"row": row, "col": col},
                            "contour_index": None,
                        },
                    )
                tile_events.append(
                    {
                        "event": "tile",
                        "glyph": glyph.name,
                        "tile": {"row": row, "col": col},
                        "status": "missing",
                    }
                )
                continue
            outcomes_for_glyph.append(outcome)
            if outcome.status != "computed" or outcome.tile is None:
                if glyph_error is None and outcome.error is not None:
                    glyph_error = outcome.error
                tile_events.append(
                    {
                        "event": "tile",
                        "glyph": glyph.name,
                        "tile": {"row": row, "col": col},
                        "status": outcome.status,
                        "wave": outcome.wave,
                        "measurement": (
                            task_plan.measurement.to_dict()
                            if task_plan is not None and task_plan.measurement
                            else None
                        ),
                        "error": outcome.error.to_dict() if outcome.error else None,
                    }
                )
                continue
            tiles_for_glyph.append(outcome.tile)
            tile_events.append(
                {
                    "event": "tile",
                    "glyph": glyph.name,
                    "tile": {"row": row, "col": col},
                    "status": "computed",
                    "wave": outcome.wave,
                    "wait_rounds": (
                        task_plan.wait_rounds if task_plan is not None else 0
                    ),
                    "wait_reason": (
                        "global_memory_or_parallelism"
                        if task_plan is not None and task_plan.wait_rounds > 0
                        else None
                    ),
                    "submission_order": outcome.submit_order,
                    "measurement": (
                        task_plan.measurement.to_dict()
                        if task_plan is not None and task_plan.measurement
                        else None
                    ),
                    "content_hash": outcome.tile.content_hash,
                }
            )

        coverage: np.ndarray | None = None
        summary: CoverageSummary | None = None
        coverage_hash: str | None = None
        truth_hash: str | None = None
        comparison: QuantizationComparison | None = None
        status = "ok" if glyph_error is None else _status_from_error(glyph_error)
        if glyph_error is None:
            try:
                coverage, summary = compose_tiles(
                    tiles_for_glyph,
                    geometry=scene_glyph.geometry,
                    strict=True,
                    quant_step=config.quant_step,
                )
            except ValueError as exc:
                glyph_error = GlyphForgeError(
                    ErrorCode.E_COVERAGE_MISMATCH,
                    "tile composition failed",
                    {
                        "glyph": glyph.name,
                        "tile": {"row": 0, "col": 0},
                        "contour_index": None,
                        "error": str(exc),
                    },
                )
                status = "coverage_mismatch"
        if glyph_error is None:
            truth = raster_region(
                scene_glyph.geometry,
                row=0,
                col=0,
                x0=scene_glyph.geometry.x0,
                y0=scene_glyph.geometry.y0,
                width=scene_glyph.geometry.width,
                height=scene_glyph.geometry.height,
                options=options,
                batch_size=config.budget.sampling_batch_size,
            )
            truth_hash = truth.content_hash
            mismatch = _coverage_mismatch(
                glyph.name,
                scene_glyph.geometry,
                coverage,
                truth.coverage,
                config.tile_size,
                config.coverage_tolerance,
            )
            if mismatch is not None:
                glyph_error = mismatch
                status = "coverage_mismatch"
            else:
                coverage_hash = coverage_content_hash(coverage)
                previous = (
                    previous_summaries.get(glyph.name)
                    if previous_summaries is not None
                    else None
                )
                comparison = _comparison(previous, summary)
        if glyph_error is not None:
            coverage = None
            summary = None
            coverage_hash = None

        recomputed = sum(
            1
            for event in tile_events
            if event.get("status") in ("computed", "failed")
        )
        glyph_results.append(
            GlyphRunResult(
                name=glyph.name,
                glyph_index=scene_glyph.index,
                unicode=glyph.unicode,
                status=status,
                advance=glyph.advance * config.pixel_size / font.units_per_em,
                font_bbox=glyph.bbox,
                pixel_bbox=(
                    scene_glyph.geometry.x0,
                    scene_glyph.geometry.y0,
                    scene_glyph.geometry.x0 + scene_glyph.geometry.width,
                    scene_glyph.geometry.y0 + scene_glyph.geometry.height,
                ),
                coverage=coverage,
                summary=summary,
                coverage_hash=coverage_hash,
                truth_hash=truth_hash,
                tile_count=len(scene_glyph.tiles),
                reused_tiles=reused_by_glyph.get(glyph.name, 0),
                recomputed_tiles=recomputed,
                error=glyph_error,
                comparison=comparison,
                tile_events=tile_events,
            )
        )

    successful = [
        result
        for result in glyph_results
        if result.successful and result.coverage is not None
    ]
    atlas_glyphs = [
        AtlasGlyph(
            name=result.name,
            unicode=result.unicode,
            coverage=result.coverage,
            advance=result.advance,
            font_bbox=result.font_bbox,
            pixel_bbox=result.pixel_bbox,
            coverage_hash=result.coverage_hash or "",
        )
        for result in successful
    ]
    atlas = build_atlas(atlas_glyphs, config.atlas)
    snapshot_completed = snapshot_store.completed_count if snapshot_store else 0
    snapshot_chain = snapshot_store.chain_hash if snapshot_store else None
    trace_events = _trace_events(
        font,
        text,
        config,
        scene,
        glyph_results,
        reused_by_glyph,
        snapshot_completed,
        snapshot_chain,
    )
    result = PipelineResult(
        font=font,
        config=config,
        text=text,
        glyphs=glyph_results,
        atlas=atlas,
        trace_events=trace_events,
        snapshot_completed_tiles=snapshot_completed,
        snapshot_chain_hash=snapshot_chain,
        snapshot_path=str(snapshot_file) if snapshot_file else None,
    )
    if output_dir is not None:
        result.write_outputs(output_dir)
    if progress is not None:
        progress(
            {
                "event": "run_complete",
                "glyphs": len(glyph_results),
                "successful_glyphs": result.successful_glyph_count,
                "reused_tiles": result.reuse_count,
                "recomputed_tiles": result.recompute_count,
            }
        )
    return result


def _status_from_error(error: GlyphForgeError) -> str:
    if error.code == ErrorCode.E_BUDGET_EXCEEDED:
        return "budget_rejected"
    if error.code == ErrorCode.E_COVERAGE_MISMATCH:
        return "coverage_mismatch"
    if error.code == ErrorCode.E_GLYPH_INVALID:
        return "glyph_invalid"
    return error.code.value.lower()


def _trace_events(
    font: Font,
    text: str,
    config: WorkbenchConfig,
    scene: list[SceneGlyph],
    glyph_results: list[GlyphRunResult],
    reused_by_glyph: dict[str, int],
    snapshot_completed: int,
    snapshot_chain: str | None,
) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = [
        {
            "event": "run_start",
            "format": "glyphforge.trace/1",
            "text": text,
            "font_hash": font.source_hash,
            "glyph_order": [item.glyph.name for item in scene],
            "quant_step": config.quant_step,
            "fill_rule": config.fill_rule,
            "tile_size": config.tile_size,
            "parallelism": config.budget.parallelism,
            "global_memory_bytes": config.budget.global_memory_bytes,
            "max_task_samples": config.budget.max_task_samples,
            "max_task_memory_bytes": config.budget.max_task_memory_bytes,
            "degrade_sequence": list(config.budget.degrade_sequence),
        }
    ]
    for scene_glyph, glyph_result in zip(scene, glyph_results):
        events.append(
            {
                "event": "glyph_plan",
                "glyph": scene_glyph.glyph.name,
                "tiles": len(scene_glyph.tiles),
                "reused_tiles": reused_by_glyph.get(scene_glyph.glyph.name, 0),
                "recomputed_tiles": glyph_result.recomputed_tiles,
            }
        )
        events.extend(glyph_result.tile_events)
        events.append(
            {
                "event": "glyph_complete",
                "glyph": scene_glyph.glyph.name,
                "status": glyph_result.status,
                "coverage_hash": glyph_result.coverage_hash,
                "truth_hash": glyph_result.truth_hash,
                "reused_tiles": glyph_result.reused_tiles,
                "recomputed_tiles": glyph_result.recomputed_tiles,
                "symbols": glyph_result.to_dict(),
            }
        )
    events.append(
        {
            "event": "run_complete",
            "snapshot_completed_tiles": snapshot_completed,
            "snapshot_chain_hash": snapshot_chain,
        }
    )
    return events
