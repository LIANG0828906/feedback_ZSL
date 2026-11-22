"""Segment state machine and bounded parallel scheduling primitives."""

from __future__ import annotations

import threading
import time
from contextlib import contextmanager
from dataclasses import dataclass
from enum import Enum
from typing import Any, Callable, Iterable, Iterator, Sequence

from .errors import WorkbenchError, require


class SegmentState(str, Enum):
    PENDING = "pending"
    PROCESSING = "processing"
    COMMITTED = "committed"
    FAILED = "failed"


LEGAL_TRANSITIONS = {
    SegmentState.PENDING: {SegmentState.PROCESSING},
    SegmentState.PROCESSING: {SegmentState.COMMITTED, SegmentState.FAILED},
    SegmentState.FAILED: {SegmentState.PROCESSING},
    SegmentState.COMMITTED: set(),
}


@dataclass(frozen=True)
class SegmentSpec:
    index: int
    row: int
    col: int
    y0: int
    x0: int
    y1: int
    x1: int
    grid_rows: int
    grid_cols: int

    @property
    def height(self) -> int:
        return self.y1 - self.y0

    @property
    def width(self) -> int:
        return self.x1 - self.x0

    @property
    def pixel_count(self) -> int:
        return self.height * self.width

    @property
    def bbox(self) -> tuple[int, int, int, int]:
        return self.y0, self.x0, self.y1, self.x1

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "row": self.row,
            "col": self.col,
            "bbox": list(self.bbox),
            "pixel_count": self.pixel_count,
        }


def build_segments(height: int, width: int, segment_size: tuple[int, int]) -> list[SegmentSpec]:
    seg_h, seg_w = int(segment_size[0]), int(segment_size[1])
    require(height > 0 and width > 0 and seg_h > 0 and seg_w > 0, "E_PARA", "Invalid scene or segment size", field="segment_size")
    rows = (height + seg_h - 1) // seg_h
    cols = (width + seg_w - 1) // seg_w
    segments: list[SegmentSpec] = []
    index = 0
    for row in range(rows):
        for col in range(cols):
            y0 = row * seg_h
            x0 = col * seg_w
            y1 = min(height, y0 + seg_h)
            x1 = min(width, x0 + seg_w)
            segments.append(SegmentSpec(index, row, col, y0, x0, y1, x1, rows, cols))
            index += 1
    return segments


def segment_for_pixel(segments: Sequence[SegmentSpec], y: int, x: int) -> SegmentSpec | None:
    for segment in segments:
        if segment.y0 <= y < segment.y1 and segment.x0 <= x < segment.x1:
            return segment
    return None


@dataclass
class SegmentRecord:
    spec: SegmentSpec
    state: SegmentState = SegmentState.PENDING
    metadata_snapshot: dict[str, Any] | None = None
    params_snapshot: dict[str, Any] | None = None
    metadata_version: str | None = None
    params_version: str | None = None
    algorithm_version: str | None = None
    error: dict[str, Any] | None = None


class SegmentStateMachine:
    """Four-state machine with immutable committed snapshots."""

    def __init__(self, segments: Sequence[SegmentSpec]):
        self.records = [SegmentRecord(spec=segment) for segment in segments]

    @property
    def committed_count(self) -> int:
        return sum(record.state == SegmentState.COMMITTED for record in self.records)

    @property
    def total_count(self) -> int:
        return len(self.records)

    @property
    def progress(self) -> str:
        return f"{self.committed_count}/{self.total_count}"

    def transition(self, index: int, target: SegmentState) -> SegmentRecord:
        record = self._record(index)
        current = record.state
        if target not in LEGAL_TRANSITIONS[current]:
            raise WorkbenchError(
                "E_STATE",
                f"Illegal segment state transition: {current.value} -> {target.value}",
                field=f"segments[{index}].state",
                details={"segment_index": index, "current_state": current.value, "target_state": target.value},
            )
        record.state = target
        return record

    def begin(self, index: int) -> SegmentRecord:
        return self.transition(index, SegmentState.PROCESSING)

    def mark_failed(self, index: int, error: dict[str, Any]) -> SegmentRecord:
        record = self.transition(index, SegmentState.FAILED)
        record.error = dict(error)
        return record

    def mark_committed(
        self,
        index: int,
        *,
        metadata_snapshot: dict[str, Any],
        params_snapshot: dict[str, Any],
        metadata_version: str,
        params_version: str,
        algorithm_version: str,
    ) -> SegmentRecord:
        record = self.transition(index, SegmentState.COMMITTED)
        record.metadata_snapshot = dict(metadata_snapshot)
        record.params_snapshot = dict(params_snapshot)
        record.metadata_version = metadata_version
        record.params_version = params_version
        record.algorithm_version = algorithm_version
        record.error = None
        return record

    def restore_committed(
        self,
        index: int,
        *,
        metadata_snapshot: dict[str, Any] | None = None,
        params_snapshot: dict[str, Any] | None = None,
        metadata_version: str | None = None,
        params_version: str | None = None,
        algorithm_version: str | None = None,
    ) -> SegmentRecord:
        record = self._record(index)
        if record.state != SegmentState.PENDING:
            raise WorkbenchError(
                "E_STATE",
                "Checkpoint restore requires a pending segment",
                field=f"segments[{index}].state",
                details={"segment_index": index, "current_state": record.state.value},
            )
        record.state = SegmentState.COMMITTED
        record.metadata_snapshot = metadata_snapshot
        record.params_snapshot = params_snapshot
        record.metadata_version = metadata_version
        record.params_version = params_version
        record.algorithm_version = algorithm_version
        return record

    def snapshot(self) -> list[dict[str, Any]]:
        return [
            {
                "index": record.spec.index,
                "state": record.state.value,
                "error": record.error,
                "metadata_version": record.metadata_version,
                "params_version": record.params_version,
                "algorithm_version": record.algorithm_version,
            }
            for record in self.records
        ]

    def _record(self, index: int) -> SegmentRecord:
        require(
            isinstance(index, int) and 0 <= index < len(self.records),
            "E_SEG",
            "Segment index is out of range",
            field="segment_index",
            index=index,
            total=len(self.records),
        )
        return self.records[index]


class PixelBudget:
    """A blocking, per-worker pixel-buffer budget.

    The total capacity is ``max_workers * max_pixels_per_thread``. No single
    reservation may exceed the per-thread limit, so processing either waits or
    fails configuration validation; frames are never dropped.
    """

    def __init__(self, max_workers: int, max_pixels_per_thread: int):
        require(max_workers >= 1, "E_PARA", "max_workers must be positive", field="max_workers")
        require(max_pixels_per_thread >= 1, "E_PARA", "max_pixels_per_thread must be positive", field="max_pixels_per_thread")
        self.max_workers = int(max_workers)
        self.per_thread = int(max_pixels_per_thread)
        self.capacity = self.max_workers * self.per_thread
        self._condition = threading.Condition()
        self._used = 0
        self.peak = 0
        self.blocked_reservations = 0

    @property
    def used(self) -> int:
        with self._condition:
            return self._used

    def acquire(self, pixels: int) -> None:
        pixels = int(pixels)
        require(
            pixels <= self.per_thread,
            "E_PARA",
            "Segment buffer requirement exceeds max_pixels_per_thread",
            field="max_pixels_per_thread",
            required=pixels,
            limit=self.per_thread,
        )
        with self._condition:
            while self._used + pixels > self.capacity:
                self.blocked_reservations += 1
                self._condition.wait()
            self._used += pixels
            self.peak = max(self.peak, self._used)

    def release(self, pixels: int) -> None:
        with self._condition:
            self._used -= int(pixels)
            if self._used < 0:
                raise RuntimeError("Pixel budget released more capacity than reserved")
            self._condition.notify_all()

    @contextmanager
    def reserve(self, pixels: int) -> Iterator[None]:
        self.acquire(pixels)
        try:
            yield
        finally:
            self.release(pixels)

    def stats(self) -> dict[str, Any]:
        with self._condition:
            return {
                "max_workers": self.max_workers,
                "max_pixels_per_thread": self.per_thread,
                "capacity_pixels": self.capacity,
                "peak_reserved_pixels": self.peak,
                "blocked_reservations": self.blocked_reservations,
            }


@dataclass
class OrderedTask:
    key: int
    function: Callable[[], Any]
    pixel_cost: int


def run_ordered_bounded(
    tasks: Iterable[OrderedTask],
    *,
    max_workers: int,
    pixel_budget: PixelBudget,
    on_result: Callable[[OrderedTask, Any], None],
    on_error: Callable[[OrderedTask, BaseException], None],
) -> dict[str, Any]:
    """Run tasks concurrently while committing callbacks in deterministic order."""

    from concurrent.futures import ThreadPoolExecutor

    task_list = list(tasks)
    if max_workers <= 1:
        for task in task_list:
            pixel_budget.acquire(task.pixel_cost)
            try:
                result = task.function()
            except BaseException as exc:
                on_error(task, exc)
            else:
                on_result(task, result)
            finally:
                pixel_budget.release(task.pixel_cost)
        return {"mode": "serial", "budget": pixel_budget.stats()}

    def run_one(task: OrderedTask) -> Any:
        return task.function()

    pending: list[tuple[OrderedTask, Any]] = []
    task_index = 0
    started = time.perf_counter()
    with ThreadPoolExecutor(max_workers=max_workers, thread_name_prefix="mshdr-seg") as executor:
        while task_index < len(task_list) or pending:
            while task_index < len(task_list) and len(pending) < max_workers:
                task = task_list[task_index]
                # Reserve before submission so the producer itself blocks when
                # the declared pixel buffer is exhausted.
                pixel_budget.acquire(task.pixel_cost)
                try:
                    future = executor.submit(run_one, task)
                except BaseException:
                    pixel_budget.release(task.pixel_cost)
                    raise
                pending.append((task, future))
                task_index += 1
            task, future = pending.pop(0)
            try:
                result = future.result()
            except BaseException as exc:
                on_error(task, exc)
            else:
                on_result(task, result)
            finally:
                # The matching producer reservation is released after its
                # ordered result has been consumed.
                pixel_budget.release(task.pixel_cost)
    return {
        "mode": "parallel",
        "max_workers": max_workers,
        "elapsed_seconds": time.perf_counter() - started,
        "budget": pixel_budget.stats(),
    }

