"""Bounded parallel replay helpers and serialized shared snapshot writes."""

from __future__ import annotations

import os
import threading
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass
from typing import Any, Callable, Generic, Iterable, TypeVar

from .compare import compare_results
from .errors import ParameterError, StudioError
from .models import BindingTable, DeviceSpec, SampleBundle
from .pipeline import Pipeline, PipelineRun
from .snapshot import SnapshotStore, SnapshotWriteReport

T = TypeVar("T")


class BufferOverflowError(StudioError):
    default_code = "E_CONCURRENCY"


class BoundedEventBuffer(Generic[T]):
    """A hard-capacity event buffer used by replay workers."""

    def __init__(self, capacity: int) -> None:
        if capacity <= 0:
            raise ParameterError("event buffer capacity must be positive", field="capacity")
        self.capacity = capacity
        self._items: list[T] = []
        self.max_observed = 0

    def push(self, item: T) -> None:
        if len(self._items) >= self.capacity:
            raise BufferOverflowError(
                f"per-thread event buffer exceeded capacity {self.capacity}",
                field="event_buffer",
                context={"capacity": self.capacity},
            )
        self._items.append(item)
        self.max_observed = max(self.max_observed, len(self._items))

    def extend(self, items: Iterable[T]) -> None:
        for item in items:
            self.push(item)

    def drain(self) -> tuple[T, ...]:
        drained = tuple(self._items)
        self._items.clear()
        return drained

    def __len__(self) -> int:
        return len(self._items)


@dataclass(frozen=True)
class ParallelReplayReport:
    hashes: tuple[str, ...]
    all_equal: bool
    run_count: int
    max_parallel: int
    event_buffer_limit: int
    first_diff: dict[str, Any] | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "hashes": list(self.hashes),
            "all_equal": self.all_equal,
            "run_count": self.run_count,
            "max_parallel": self.max_parallel,
            "event_buffer_limit": self.event_buffer_limit,
            "first_diff": self.first_diff,
        }


class ReplayCoordinator:
    def __init__(
        self,
        *,
        max_parallel: int = 2,
        event_buffer_limit: int = 256,
    ) -> None:
        if not 1 <= max_parallel <= 4:
            raise ParameterError(
                "max_parallel must be within [1, 4]",
                field="max_parallel",
            )
        if not 1 <= event_buffer_limit <= 4096:
            raise ParameterError(
                "event_buffer_limit must be within [1, 4096]",
                field="event_buffer_limit",
            )
        self.max_parallel = max_parallel
        self.event_buffer_limit = event_buffer_limit
        self._executor = ThreadPoolExecutor(
            max_workers=max_parallel,
            thread_name_prefix="padweave-replay",
        )
        self._lock = threading.Lock()
        self._closed = False

    def submit(self, function: Callable[..., T], *args: Any, **kwargs: Any) -> Future[T]:
        with self._lock:
            if self._closed:
                raise RuntimeError("ReplayCoordinator is closed")
            return self._executor.submit(function, *args, **kwargs)

    def replay_once(self, bundle: SampleBundle) -> PipelineRun:
        pipeline = Pipeline(
            bundle,
            max_event_buffer=self.event_buffer_limit,
        )
        return pipeline.recompute()

    def parallel_determinism_check(
        self,
        bundle: SampleBundle,
        *,
        runs: int = 3,
    ) -> ParallelReplayReport:
        if runs < 2:
            raise ParameterError("runs must be at least 2", field="runs")
        futures = [self.submit(self.replay_once, bundle) for _ in range(runs)]
        # gather is intentionally independent of completion order
        results = [future.result() for future in futures]
        hashes = tuple(run.result.result_hash for run in results)
        all_equal = len(set(hashes)) == 1
        first_diff = None
        if not all_equal:
            reference = results[0].result
            for index, run in enumerate(results[1:], start=1):
                report = compare_results(run.result, reference)
                if not report.equal:
                    first_diff = {"run_index": index, **report.to_dict()}
                    break
        return ParallelReplayReport(
            hashes=hashes,
            all_equal=all_equal,
            run_count=runs,
            max_parallel=self.max_parallel,
            event_buffer_limit=self.event_buffer_limit,
            first_diff=first_diff,
        )

    def incremental_replay(
        self,
        pipeline: Pipeline,
        *,
        device: DeviceSpec | None = None,
        bindings: BindingTable | None = None,
        force_node_ids: Iterable[str] = (),
    ) -> PipelineRun:
        return pipeline.update(
            device=device,
            bindings=bindings,
            force_node_ids=force_node_ids,
        )

    def close(self) -> None:
        with self._lock:
            if self._closed:
                return
            self._closed = True
            self._executor.shutdown(wait=True, cancel_futures=False)

    def __enter__(self) -> "ReplayCoordinator":
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        self.close()


class SerializedSnapshotWriter:
    """Serializes shared snapshot writes; SnapshotStore adds cross-process locking."""

    def __init__(self, store: SnapshotStore) -> None:
        self.store = store
        self._lock = threading.Lock()
        self.write_count = 0

    def write(self, pipeline: Pipeline) -> SnapshotWriteReport:
        with self._lock:
            report = self.store.write(pipeline)
            self.write_count += 1
            return report


def available_parallelism() -> int:
    return max(1, min(2, os.cpu_count() or 1))


__all__ = [
    "BoundedEventBuffer",
    "BufferOverflowError",
    "ParallelReplayReport",
    "ReplayCoordinator",
    "SerializedSnapshotWriter",
    "available_parallelism",
]
