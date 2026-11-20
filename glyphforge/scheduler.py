from __future__ import annotations

from collections.abc import Callable, Iterable
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass

from .errors import ErrorCode, GlyphForgeError, RasterCancelled
from .raster import (
    CancelEvent,
    GlyphGeometry,
    RasterOptions,
    TileCoverage,
    raster_region,
    temporary_scanline_bytes,
)
from .snapshot import SnapshotStore


@dataclass(frozen=True)
class BudgetConfig:
    parallelism: int = 4
    global_memory_bytes: int = 64 * 1024 * 1024
    max_task_samples: int = 4_000_000
    max_task_memory_bytes: int = 4 * 1024 * 1024
    sampling_batch_size: int = 2048
    min_sampling_batch_size: int = 256
    degrade_sequence: tuple[str, ...] = ("shrink_batch", "reject")

    def validate(self) -> None:
        if self.parallelism < 1 or self.parallelism > 64:
            raise ValueError("parallelism must be between 1 and 64")
        if self.global_memory_bytes <= 0 or self.max_task_memory_bytes <= 0:
            raise ValueError("memory budgets must be positive")
        if self.max_task_samples <= 0:
            raise ValueError("max_task_samples must be positive")
        if self.sampling_batch_size <= 0:
            raise ValueError("sampling_batch_size must be positive")
        if self.min_sampling_batch_size <= 0:
            raise ValueError("min_sampling_batch_size must be positive")
        if self.min_sampling_batch_size > self.sampling_batch_size:
            raise ValueError("min_sampling_batch_size must not exceed sampling_batch_size")
        for action in self.degrade_sequence:
            if action not in ("shrink_batch", "reject"):
                raise ValueError(f"unsupported degrade action: {action}")


@dataclass(frozen=True)
class TileTask:
    glyph_name: str
    glyph_index: int
    row: int
    col: int
    x: int
    y: int
    width: int
    height: int
    geometry: GlyphGeometry
    sample_count: int
    coverage_buffer_bytes: int


@dataclass(frozen=True)
class TaskMeasurement:
    sample_count: int
    coverage_buffer_bytes: int
    temporary_scanline_bytes: int
    total_bytes: int
    requested_batch_size: int
    effective_batch_size: int
    degradation_attempts: tuple[int, ...] = ()

    def to_dict(self) -> dict[str, int]:
        return {
            "sample_count": self.sample_count,
            "coverage_buffer_bytes": self.coverage_buffer_bytes,
            "temporary_scanline_bytes": self.temporary_scanline_bytes,
            "total_bytes": self.total_bytes,
            "requested_batch_size": self.requested_batch_size,
            "effective_batch_size": self.effective_batch_size,
        }


@dataclass
class GlyphSubmission:
    name: str
    glyph_index: int
    tasks: list[TileTask]


@dataclass
class TaskPlan:
    task: TileTask
    submit_order: int
    measurement: TaskMeasurement | None
    accepted: bool
    status: str
    rejection: GlyphForgeError | None = None
    wave: int = -1
    wait_rounds: int = 0


@dataclass
class GlyphPlan:
    name: str
    glyph_index: int
    tasks: list[TaskPlan]
    accepted: bool
    rejection: GlyphForgeError | None = None


@dataclass
class SchedulePlan:
    glyphs: list[GlyphPlan]
    task_order: list[TaskPlan]
    waves: list[list[TaskPlan]]
    rejected_tasks: list[TaskPlan]


@dataclass(frozen=True)
class TaskOutcome:
    task: TileTask
    submit_order: int
    wave: int
    status: str
    tile: TileCoverage | None = None
    error: GlyphForgeError | None = None


@dataclass
class ExecutionResult:
    outcomes: list[TaskOutcome]
    cancelled: bool
    waves_run: int


def measure_task(
    task: TileTask,
    budget: BudgetConfig,
) -> tuple[TaskMeasurement, GlyphForgeError | None]:
    budget.validate()
    batch = budget.sampling_batch_size
    attempts: list[int] = []
    coverage_bytes = task.width * task.height * 4
    sample_count = task.sample_count
    degradation_done = False
    for action in budget.degrade_sequence:
        if action == "shrink_batch":
            while batch > budget.min_sampling_batch_size:
                temp_bytes = temporary_scanline_bytes(batch)
                total_bytes = coverage_bytes + temp_bytes
                if (
                    total_bytes <= budget.max_task_memory_bytes
                    and sample_count <= budget.max_task_samples
                ):
                    break
                attempts.append(batch)
                batch = max(budget.min_sampling_batch_size, batch // 2)
            temp_bytes = temporary_scanline_bytes(batch)
            total_bytes = coverage_bytes + temp_bytes
            if (
                total_bytes <= budget.max_task_memory_bytes
                and sample_count <= budget.max_task_samples
            ):
                degradation_done = True
                break
        elif action == "reject":
            break
    temp_bytes = temporary_scanline_bytes(batch)
    total_bytes = coverage_bytes + temp_bytes
    measurement = TaskMeasurement(
        sample_count=sample_count,
        coverage_buffer_bytes=coverage_bytes,
        temporary_scanline_bytes=temp_bytes,
        total_bytes=total_bytes,
        requested_batch_size=budget.sampling_batch_size,
        effective_batch_size=batch,
        degradation_attempts=tuple(attempts),
    )
    if (
        sample_count <= budget.max_task_samples
        and total_bytes <= budget.max_task_memory_bytes
    ):
        return measurement, None
    exceeded: list[str] = []
    if sample_count > budget.max_task_samples:
        exceeded.append("samples")
    if total_bytes > budget.max_task_memory_bytes:
        exceeded.append("memory")
    if "reject" not in budget.degrade_sequence and degradation_done:
        return measurement, None
    error = GlyphForgeError(
        ErrorCode.E_BUDGET_EXCEEDED,
        "tile task exceeds its declared budget and was not started",
        {
            "glyph": task.glyph_name,
            "tile": {"row": task.row, "col": task.col},
            "measurement": measurement.to_dict(),
            "limits": {
                "max_task_samples": budget.max_task_samples,
                "max_task_memory_bytes": budget.max_task_memory_bytes,
            },
            "exceeded": exceeded,
            "degrade_sequence": list(budget.degrade_sequence),
        },
    )
    return measurement, error


class DeterministicScheduler:
    def __init__(self, budget: BudgetConfig):
        budget.validate()
        self.budget = budget

    def plan(
        self,
        submissions: Iterable[GlyphSubmission],
    ) -> SchedulePlan:
        submissions_list = list(submissions)
        seen_glyphs: set[str] = set()
        for submission in submissions_list:
            if submission.name in seen_glyphs:
                raise GlyphForgeError(
                    ErrorCode.E_BUSY,
                    "the same glyph was submitted more than once",
                    {"glyph": submission.name},
                )
            seen_glyphs.add(submission.name)

        glyph_plans: list[GlyphPlan] = []
        accepted_tasks: list[TaskPlan] = []
        rejected_tasks: list[TaskPlan] = []
        submit_order = 0
        for submission in sorted(submissions_list, key=lambda item: item.glyph_index):
            planned_for_glyph: list[TaskPlan] = []
            rejection: GlyphForgeError | None = None
            for task in sorted(submission.tasks, key=lambda item: (item.row, item.col)):
                measurement, error = measure_task(task, self.budget)
                if error is not None and rejection is None:
                    rejection = error
                if rejection is None and measurement.total_bytes > self.budget.global_memory_bytes:
                    rejection = GlyphForgeError(
                        ErrorCode.E_BUDGET_EXCEEDED,
                        "tile task cannot fit within the global memory budget",
                        {
                            "glyph": task.glyph_name,
                            "tile": {"row": task.row, "col": task.col},
                            "measurement": measurement.to_dict(),
                            "global_memory_bytes": self.budget.global_memory_bytes,
                        },
                    )
                planned_for_glyph.append(
                    TaskPlan(
                        task=task,
                        submit_order=submit_order,
                        measurement=measurement,
                        accepted=error is None,
                        status="pending" if error is None else "rejected",
                        rejection=error,
                    )
                )
                submit_order += 1
            if rejection is None:
                for task_plan in planned_for_glyph:
                    task_plan.accepted = True
                    task_plan.status = "pending"
                    accepted_tasks.append(task_plan)
            else:
                for task_plan in planned_for_glyph:
                    task_plan.accepted = False
                    task_plan.status = "rejected_glyph_isolated"
                    if task_plan.rejection is None:
                        task_plan.rejection = rejection
                    rejected_tasks.append(task_plan)
            glyph_plans.append(
                GlyphPlan(
                    name=submission.name,
                    glyph_index=submission.glyph_index,
                    tasks=planned_for_glyph,
                    accepted=rejection is None,
                    rejection=rejection,
                )
            )

        waves: list[list[TaskPlan]] = []
        current_wave: list[TaskPlan] = []
        current_bytes = 0
        for task_plan in accepted_tasks:
            total = task_plan.measurement.total_bytes if task_plan.measurement else 0
            cannot_add = (
                len(current_wave) >= self.budget.parallelism
                or (current_wave and current_bytes + total > self.budget.global_memory_bytes)
            )
            if cannot_add:
                waves.append(current_wave)
                current_wave = []
                current_bytes = 0
            current_wave.append(task_plan)
            current_bytes += total
        if current_wave:
            waves.append(current_wave)
        for wave_index, wave in enumerate(waves):
            for task_plan in wave:
                task_plan.wave = wave_index
                task_plan.wait_rounds = wave_index
                task_plan.status = "scheduled"
        return SchedulePlan(
            glyphs=glyph_plans,
            task_order=accepted_tasks,
            waves=waves,
            rejected_tasks=rejected_tasks,
        )

    def execute(
        self,
        plan: SchedulePlan,
        options: RasterOptions,
        *,
        snapshot_store: SnapshotStore | None = None,
        cancel_event: CancelEvent | None = None,
        on_tile: Callable[[TaskOutcome], None] | None = None,
    ) -> ExecutionResult:
        outcomes: list[TaskOutcome] = []
        cancelled = False
        waves_run = 0

        def run_task(task_plan: TaskPlan) -> TaskOutcome:
            task = task_plan.task
            if cancel_event is not None and cancel_event.is_set():
                return TaskOutcome(
                    task=task,
                    submit_order=task_plan.submit_order,
                    wave=task_plan.wave,
                    status="cancelled",
                )
            batch_size = (
                task_plan.measurement.effective_batch_size
                if task_plan.measurement is not None
                else self.budget.sampling_batch_size
            )
            try:
                tile = raster_region(
                    task.geometry,
                    row=task.row,
                    col=task.col,
                    x0=task.x,
                    y0=task.y,
                    width=task.width,
                    height=task.height,
                    options=options,
                    batch_size=batch_size,
                    cancel_event=cancel_event,
                )
            except RasterCancelled:
                return TaskOutcome(
                    task=task,
                    submit_order=task_plan.submit_order,
                    wave=task_plan.wave,
                    status="cancelled",
                )
            except GlyphForgeError as exc:
                return TaskOutcome(
                    task=task,
                    submit_order=task_plan.submit_order,
                    wave=task_plan.wave,
                    status="failed",
                    error=exc,
                )
            except Exception as exc:  # noqa: BLE001 - isolate any tile failure
                return TaskOutcome(
                    task=task,
                    submit_order=task_plan.submit_order,
                    wave=task_plan.wave,
                    status="failed",
                    error=GlyphForgeError(
                        ErrorCode.E_COVERAGE_MISMATCH,
                        "unexpected rasterization failure",
                        {
                            "glyph": task.glyph_name,
                            "tile": {"row": task.row, "col": task.col},
                            "error": repr(exc),
                        },
                    ),
                )
            if snapshot_store is not None:
                snapshot_store.add_tile(tile)
            return TaskOutcome(
                task=task,
                submit_order=task_plan.submit_order,
                wave=task_plan.wave,
                status="computed",
                tile=tile,
            )

        with ThreadPoolExecutor(
            max_workers=self.budget.parallelism,
            thread_name_prefix="glyphforge-tile",
        ) as executor:
            for wave in plan.waves:
                if cancel_event is not None and cancel_event.is_set():
                    cancelled = True
                    break
                wave_outcomes: list[TaskOutcome] = []
                futures = [executor.submit(run_task, task_plan) for task_plan in wave]
                for future in as_completed(futures):
                    wave_outcomes.append(future.result())
                wave_outcomes.sort(key=lambda outcome: outcome.submit_order)
                outcomes.extend(wave_outcomes)
                waves_run += 1
                if on_tile is not None:
                    for outcome in wave_outcomes:
                        on_tile(outcome)
                if any(outcome.status in ("cancelled",) for outcome in wave_outcomes):
                    cancelled = True
                    break
                if cancel_event is not None and cancel_event.is_set():
                    cancelled = True
                    break
        outcomes.sort(key=lambda outcome: outcome.submit_order)
        return ExecutionResult(
            outcomes=outcomes,
            cancelled=cancelled,
            waves_run=waves_run,
        )
