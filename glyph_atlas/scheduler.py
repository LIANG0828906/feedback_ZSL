"""Deterministic tiled scheduling, resource budgets, and disk recovery."""

from __future__ import annotations

import hashlib
import re
import threading
from concurrent.futures import Future, ThreadPoolExecutor, wait, FIRST_COMPLETED
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterable

import numpy as np

from .config import TargetConfig, WorkConfig
from .errors import (
    E_BUDGET_EXCEEDED,
    E_BUSY,
    E_CANCELLED,
    E_CONFIG_INVALID,
    E_TILE_MISMATCH,
    fail,
)
from .font import FontSpec, ResolvedGlyph
from .raster import (
    GlyphRenderPlan,
    PixelBox,
    TileRaster,
    compare_coverage,
    coverage_sha256,
    estimate_tile_memory,
    make_render_plan,
    rasterize_tile,
    rasterize_whole,
    tile_mismatch_coord,
)
from .snapshot import (
    SnapshotStore,
    SnapshotTile,
    ensure_writable_directory,
    make_snapshot_tile,
    snapshot_coverage_hash,
)


@dataclass
class GlyphRasterResult:
    glyph_name: str
    unicode: tuple[int, ...]
    plan: GlyphRenderPlan
    coverage: np.ndarray
    coverage_hash: str
    quantization_summary: dict[str, Any]
    sample_points: int
    memory_bytes: int
    tiles: tuple[SnapshotTile, ...]
    snapshot_path: Path
    snapshot_store: SnapshotStore
    trace: tuple[dict[str, Any], ...]
    resumed: bool
    whole_reference: dict[str, Any]

    def metrics_record(self) -> dict[str, Any]:
        return {
            "glyph": self.glyph_name,
            "unicode": list(self.unicode),
            "advance_px": _round(self.plan.advance_px),
            "origin_x": self.plan.origin_x,
            "origin_top": self.plan.origin_top,
            "width": self.plan.width,
            "height": self.plan.height,
            "bbox": [_round(value) for value in self.plan.bbox],
            "fill_rule": self.plan.fill_rule,
            "coverage_sha256": self.coverage_hash,
            "quantization": self.quantization_summary,
        }


@dataclass
class SchedulerReport:
    target: TargetConfig
    glyphs: tuple[GlyphRasterResult, ...]
    trace: tuple[dict[str, Any], ...]
    parallelism: int

    def glyph_map(self) -> dict[str, GlyphRasterResult]:
        return {glyph.glyph_name: glyph for glyph in self.glyphs}


@dataclass
class _BudgetFailure:
    index: int
    coord: tuple[int, int]
    sample_points: int
    memory_bytes: int
    used_sampling_points: int
    used_memory_bytes: int
    reserved_sampling_points: int
    reserved_memory_bytes: int
    limit_name: str
    limit_value: int


@dataclass
class _GlyphSchedule:
    glyph: ResolvedGlyph
    plan: GlyphRenderPlan
    store: SnapshotStore
    tiles: list[SnapshotTile] = field(default_factory=list)
    sample_points: int = 0
    memory_bytes: int = 0
    resumed: bool = False
    trace: list[dict[str, Any]] = field(default_factory=list)
    budget_failure: _BudgetFailure | None = None


class RasterScheduler:
    """A fixed-concurrency scheduler with deterministic commit ordering.

    Submission is accepted once per glyph. Tiles are executed by a fixed-size
    thread pool, but completed results are committed only in row-major tile
    coordinate order. A commitment updates both the in-memory coverage and the
    hash-chained disk snapshot atomically.
    """

    def __init__(
        self,
        font: FontSpec,
        config: WorkConfig,
        target: TargetConfig,
        resume: bool = False,
        cancel_event: threading.Event | None = None,
        progress: Callable[[dict[str, Any]], None] | None = None,
    ) -> None:
        self.font = font
        self.config = config
        self.target = target
        self.resume = resume
        self.cancel_event = cancel_event or threading.Event()
        self.progress = progress
        self._configured_parallelism = config.parallelism
        self._submitted: set[str] = set()
        self._queue: list[ResolvedGlyph] = []
        self._trace: list[dict[str, Any]] = []
        self._trace_sequence = 0
        self._snapshot_directory = ensure_writable_directory(config.snapshot_dir)
        ensure_writable_directory(config.temp_dir)

    @property
    def parallelism(self) -> int:
        return self._configured_parallelism

    def submit_glyph(self, glyph_reference: str | int) -> int:
        glyph_spec = self.font.glyph(glyph_reference)
        if glyph_spec.name in self._submitted:
            fail(
                E_BUSY,
                "Glyph is already submitted",
                glyph=glyph_spec.name,
                target=self.target.name,
            )
        resolved = self.font.resolve(glyph_spec.name, fill_rule=self.config.fill_rule)
        self._submitted.add(glyph_spec.name)
        self._queue.append(resolved)
        self._record(
            "glyph_submit",
            glyph=resolved.name,
            unicode=list(resolved.unicode),
            submit_index=len(self._queue) - 1,
        )
        return len(self._queue) - 1

    def set_parallelism(self, value: int) -> None:
        if value != self._configured_parallelism:
            fail(
                E_CONFIG_INVALID,
                "Parallelism is fixed by configuration and cannot be changed at runtime",
                configured=self._configured_parallelism,
                requested=value,
            )

    def cancel(self) -> None:
        self.cancel_event.set()

    def run(self) -> SchedulerReport:
        if not self._queue:
            fail(E_CONFIG_INVALID, "No glyphs were submitted to the scheduler")
        results: list[GlyphRasterResult] = []
        for glyph in self._queue:
            result = self._run_glyph(glyph)
            results.append(result)
        ordered_trace = self.trace_records()
        for result in results:
            result.trace = tuple(
                record
                for record in ordered_trace
                if record.get("glyph") == result.glyph_name
            )
        return SchedulerReport(
            target=self.target,
            glyphs=tuple(results),
            trace=tuple(ordered_trace),
            parallelism=self._configured_parallelism,
        )

    def trace_records(self) -> list[dict[str, Any]]:
        """Return a stable event ordering independent of worker completion."""

        glyph_order = {glyph.name: index for index, glyph in enumerate(self._queue)}
        event_rank = {
            "glyph_submit": 0,
            "glyph_start": 1,
            "glyph_resume": 1,
            "tile_submit": 2,
            "tile_commit": 3,
            "tile_budget_exceeded": 4,
            "glyph_done": 5,
            "run_cancelled": 6,
        }

        def key(record: dict[str, Any]) -> tuple[Any, ...]:
            glyph = str(record.get("glyph", ""))
            task = record.get("task")
            coord = record.get("coord", [0, 0])
            return (
                glyph_order.get(glyph, 1_000_000),
                event_rank.get(str(record.get("event")), 99),
                int(task) if isinstance(task, int) else -1,
                int(coord[0]) if isinstance(coord, list) and len(coord) == 2 else 0,
                int(coord[1]) if isinstance(coord, list) and len(coord) == 2 else 0,
            )

        ordered = sorted(self._trace, key=key)
        for sequence, record in enumerate(ordered):
            record["seq"] = sequence
        return ordered

    def _run_glyph(self, glyph: ResolvedGlyph) -> GlyphRasterResult:
        trace_start = len(self._trace)
        plan = make_render_plan(
            glyph,
            pixel_size=self.target.pixel_size,
            units_per_em=self.font.units_per_em,
            subpixel_x=self.target.subpixel_x,
            subpixel_y=self.target.subpixel_y,
            fill_rule=self.config.fill_rule,
        )
        snapshot_path = self._snapshot_path(glyph)
        store = SnapshotStore(
            path=snapshot_path,
            plan=plan,
            sampling=self.config.sampling,
            tile_width=self.config.tile_width,
            tile_height=self.config.tile_height,
            budget=self.config.budget.as_dict(),
            font_sha256=self.config_font_hash(),
            temp_dir=self.config.temp_dir,
        )
        schedule = _GlyphSchedule(glyph=glyph, plan=plan, store=store)
        loaded = False
        if self.resume and snapshot_path.exists():
            state = store.load()
            schedule.tiles = list(state.tiles)
            schedule.sample_points = sum(tile.sample_points for tile in schedule.tiles)
            schedule.memory_bytes = sum(tile.memory_bytes for tile in schedule.tiles)
            schedule.resumed = True
            loaded = True
            self._record(
                "glyph_resume",
                glyph=glyph.name,
                snapshot=snapshot_path.name,
                completed_indices=list(state.completed_indices),
                head_hash=state.head_hash,
            )
        else:
            store.write([])
            self._record(
                "glyph_start",
                glyph=glyph.name,
                snapshot=snapshot_path.name,
                completed_indices=[],
            )

        try:
            self._schedule_tiles(schedule)
        except BaseException as exc:
            if isinstance(exc, Exception):
                store.write(schedule.tiles)
            raise

        coverage = self._assemble_coverage(schedule)
        assembled_hash = coverage_sha256(coverage)
        reference_coverage, reference_tile = rasterize_whole(plan, self.config.sampling)
        consistent, comparison = compare_coverage(
            coverage,
            reference_coverage,
            self.config.coverage_tolerance,
        )
        if not consistent:
            coordinate = comparison.get("coordinate", [0, 0])
            pixel_x, pixel_y = int(coordinate[0]), int(coordinate[1])
            fail(
                E_TILE_MISMATCH,
                "Tiled coverage differs from whole-glyph reference",
                glyph=glyph.name,
                coord=[
                    pixel_x // self.config.tile_width,
                    pixel_y // self.config.tile_height,
                ],
                pixel=[pixel_x, pixel_y],
                comparison=comparison,
                snapshot=str(snapshot_path),
            )

        quantization = {
            "step": self.config.sampling.quantization_step,
            "pixel_count": plan.width * plan.height,
            "raw_coverage_sum": _round(sum(tile.raw_sum for tile in schedule.tiles)),
            "quantized_coverage_sum": _round(sum(tile.quantized_sum for tile in schedule.tiles)),
            "absolute_error": _round(sum(tile.quantization_error for tile in schedule.tiles)),
            "max_absolute_error": _round(
                max((tile.max_quantization_error for tile in schedule.tiles), default=0.0)
            ),
        }
        quantization["mean_absolute_error"] = _round(
            quantization["absolute_error"] / max(1, quantization["pixel_count"])
        )
        self._record(
            "glyph_done",
            glyph=glyph.name,
            coverage_sha256=assembled_hash,
            quantization=quantization,
            tile_count=len(schedule.tiles),
            resumed=loaded,
            whole_max_difference=comparison.get("max_difference", 0),
        )
        return GlyphRasterResult(
            glyph_name=glyph.name,
            unicode=glyph.unicode,
            plan=plan,
            coverage=coverage,
            coverage_hash=assembled_hash,
            quantization_summary=quantization,
            sample_points=schedule.sample_points,
            memory_bytes=schedule.memory_bytes,
            tiles=tuple(schedule.tiles),
            snapshot_path=snapshot_path,
            snapshot_store=store,
            trace=tuple(self._trace[trace_start:]),
            resumed=schedule.resumed,
            whole_reference={
                "content_hash": reference_tile.content_hash,
                "max_difference": comparison.get("max_difference", 0),
                "total_difference": comparison.get("total_difference", 0),
                "tolerance": self.config.coverage_tolerance,
            },
        )

    def _schedule_tiles(self, schedule: _GlyphSchedule) -> None:
        tile_plan = schedule.plan.tile_boxes(self.config.tile_width, self.config.tile_height)
        if len(schedule.tiles) > len(tile_plan):
            fail(E_CONFIG_INVALID, "Snapshot contains too many tiles", glyph=schedule.glyph.name)
        next_submit = len(schedule.tiles)
        next_commit = len(schedule.tiles)
        previous_hash = schedule.tiles[-1].chain_hash if schedule.tiles else _genesis()
        ready: dict[int, TileRaster] = {}
        pending: dict[int, Future[TileRaster]] = {}
        reserved_samples = 0
        reserved_memory = 0
        stopped_for_budget = False
        cancelled = False

        with ThreadPoolExecutor(
            max_workers=self._configured_parallelism,
            thread_name_prefix="glyph-tile",
        ) as executor:
            while next_commit < len(tile_plan):
                if self.cancel_event.is_set():
                    cancelled = True
                    break

                # Commit every contiguous completed tile before admitting new
                # work. This keeps budget accounting and the on-disk snapshot
                # prefix consistent at every observable point.
                while next_commit in ready:
                    raster = ready.pop(next_commit)
                    snapshot_tile = make_snapshot_tile(next_commit, raster, previous_hash)
                    schedule.tiles.append(snapshot_tile)
                    schedule.sample_points += raster.sample_points
                    schedule.memory_bytes += raster.memory_bytes
                    reserved_samples -= raster.sample_points
                    reserved_memory -= raster.memory_bytes
                    previous_hash = snapshot_tile.chain_hash
                    schedule.store.write(schedule.tiles)
                    self._record(
                        "tile_commit",
                        glyph=schedule.glyph.name,
                        task=next_commit,
                        coord=list(snapshot_tile.coord),
                        status="committed",
                        sample_points=raster.sample_points,
                        memory_bytes=raster.memory_bytes,
                        used_sampling_points=schedule.sample_points,
                        used_memory_bytes=schedule.memory_bytes,
                        content_hash=snapshot_tile.content_hash,
                        chain_hash=snapshot_tile.chain_hash,
                    )
                    self._emit_progress(schedule, next_commit, len(tile_plan))
                    next_commit += 1

                if next_commit >= len(tile_plan):
                    break

                while (
                    not stopped_for_budget
                    and len(pending) < self._configured_parallelism
                    and next_submit < len(tile_plan)
                ):
                    index = next_submit
                    coord, box = tile_plan[index]
                    samples = box.area * self.config.sampling.samples_x * self.config.sampling.samples_y
                    memory = estimate_tile_memory(box, self.config.sampling)
                    failure = self._budget_failure(
                        index=index,
                        coord=coord,
                        samples=samples,
                        memory=memory,
                        schedule=schedule,
                        reserved_samples=reserved_samples,
                        reserved_memory=reserved_memory,
                    )
                    if failure is not None:
                        schedule.budget_failure = failure
                        stopped_for_budget = True
                        self._record(
                            "tile_budget_exceeded",
                            glyph=schedule.glyph.name,
                            task=index,
                            coord=list(coord),
                            sample_points=samples,
                            memory_bytes=memory,
                            used_sampling_points=failure.used_sampling_points,
                            used_memory_bytes=failure.used_memory_bytes,
                            used={
                                "sampling_points": failure.used_sampling_points,
                                "memory_bytes": failure.used_memory_bytes,
                            },
                            reserved_sampling_points=failure.reserved_sampling_points,
                            reserved_memory_bytes=failure.reserved_memory_bytes,
                            limit_name=failure.limit_name,
                            limit_value=failure.limit_value,
                        )
                        break
                    future = executor.submit(rasterize_tile, schedule.plan, box, self.config.sampling, coord)
                    pending[index] = future
                    reserved_samples += samples
                    reserved_memory += memory
                    self._record(
                        "tile_submit",
                        glyph=schedule.glyph.name,
                        task=index,
                        coord=list(coord),
                        sample_points=samples,
                        memory_bytes=memory,
                        used_sampling_points=schedule.sample_points + reserved_samples,
                        used_memory_bytes=schedule.memory_bytes + reserved_memory,
                    )
                    next_submit += 1

                if not pending:
                    if stopped_for_budget and schedule.budget_failure is not None:
                        break
                    fail(
                        E_CONFIG_INVALID,
                        "Scheduler has no runnable task but tiles remain",
                        glyph=schedule.glyph.name,
                        next_commit=next_commit,
                    )

                completed, _ = wait(pending.values(), return_when=FIRST_COMPLETED)
                for future in completed:
                    index = next(
                        task_index
                        for task_index, task_future in pending.items()
                        if task_future is future
                    )
                    del pending[index]
                    ready[index] = future.result()

        if cancelled:
            schedule.store.write(schedule.tiles)
            self._record(
                "run_cancelled",
                glyph=schedule.glyph.name,
                completed_indices=[tile.index for tile in schedule.tiles],
                snapshot=schedule.store.path.name,
            )
            fail(
                E_CANCELLED,
                "Rasterization was cancelled",
                glyph=schedule.glyph.name,
                completed_indices=[tile.index for tile in schedule.tiles],
                snapshot=str(schedule.store.path),
            )
        if stopped_for_budget and schedule.budget_failure is not None:
            failure = schedule.budget_failure
            fail(
                E_BUDGET_EXCEEDED,
                "Tile resource budget would be exceeded",
                glyph=schedule.glyph.name,
                task=failure.index,
                coord=list(failure.coord),
                sample_points=failure.sample_points,
                memory_bytes=failure.memory_bytes,
                used_sampling_points=failure.used_sampling_points,
                used_memory_bytes=failure.used_memory_bytes,
                used={
                    "sampling_points": failure.used_sampling_points,
                    "memory_bytes": failure.used_memory_bytes,
                },
                reserved_sampling_points=failure.reserved_sampling_points,
                reserved_memory_bytes=failure.reserved_memory_bytes,
                limit_name=failure.limit_name,
                limit_value=failure.limit_value,
                completed_indices=[tile.index for tile in schedule.tiles],
                snapshot=str(schedule.store.path),
            )

    def _budget_failure(
        self,
        index: int,
        coord: tuple[int, int],
        samples: int,
        memory: int,
        schedule: _GlyphSchedule,
        reserved_samples: int,
        reserved_memory: int,
    ) -> _BudgetFailure | None:
        budget = self.config.budget
        checks = (
            (
                "max_task_sampling_points",
                budget.max_task_sampling_points,
                samples,
            ),
            (
                "max_task_memory_bytes",
                budget.max_task_memory_bytes,
                memory,
            ),
            (
                "max_total_sampling_points",
                budget.max_total_sampling_points,
                schedule.sample_points + reserved_samples + samples,
            ),
            (
                "max_total_memory_bytes",
                budget.max_total_memory_bytes,
                schedule.memory_bytes + reserved_memory + memory,
            ),
        )
        for limit_name, limit, usage in checks:
            if limit is not None and usage > limit:
                return _BudgetFailure(
                    index=index,
                    coord=coord,
                    sample_points=samples,
                    memory_bytes=memory,
                    used_sampling_points=schedule.sample_points,
                    used_memory_bytes=schedule.memory_bytes,
                    reserved_sampling_points=reserved_samples,
                    reserved_memory_bytes=reserved_memory,
                    limit_name=limit_name,
                    limit_value=limit,
                )
        return None

    def _assemble_coverage(self, schedule: _GlyphSchedule) -> np.ndarray:
        coverage = np.zeros((schedule.plan.height, schedule.plan.width), dtype=np.uint8)
        for tile in schedule.tiles:
            box = tile.box
            coverage[box.y : box.y + box.height, box.x : box.x + box.width] = tile.coverage
        return coverage

    def _snapshot_path(self, glyph: ResolvedGlyph) -> Path:
        safe_target = re.sub(r"[^A-Za-z0-9_.-]+", "_", self.target.name)
        safe_glyph = re.sub(r"[^A-Za-z0-9_.-]+", "_", glyph.name)
        return self._snapshot_directory / f"{safe_target}.{glyph.index:04d}.{safe_glyph}.snapshot.json"

    def config_font_hash(self) -> str:
        # The hash chain is rooted in both the parsed font and the effective
        # global fill rule. The render signature separately pins geometry.
        base = f"{self.font.canonical_hash()}:{self.config.fill_rule}".encode("utf-8")
        return hashlib.sha256(base).hexdigest()

    def _record(self, event: str, **fields: Any) -> None:
        record = {
            "seq": self._trace_sequence,
            "event": event,
            "target": self.target.name,
        }
        record.update(fields)
        self._trace_sequence += 1
        self._trace.append(record)

    def _emit_progress(self, schedule: _GlyphSchedule, committed: int, total: int) -> None:
        if self.progress is None:
            return
        self.progress(
            {
                "event": "tile_commit",
                "glyph": schedule.glyph.name,
                "committed": committed + 1,
                "total": total,
                "coord": list(schedule.tiles[-1].coord),
                "width": schedule.plan.width,
                "height": schedule.plan.height,
                "tile_width": self.config.tile_width,
                "tile_height": self.config.tile_height,
                "sample_points": schedule.sample_points,
                "memory_bytes": schedule.memory_bytes,
                "coverage": schedule.tiles[-1].coverage,
            }
        )


def _genesis() -> str:
    from .snapshot import GENESIS_HASH

    return GENESIS_HASH


def _round(value: float) -> float:
    return float(round(value, 9))

