from __future__ import annotations

import threading
from collections.abc import Callable
from concurrent.futures import FIRST_COMPLETED, Future, ThreadPoolExecutor, wait
from dataclasses import dataclass
from typing import Any

from .config import Config
from .errors import E_BUSY, E_MEMORY_BUDGET, AppError
from .trace import TraceLogger


@dataclass(frozen=True)
class ScheduledTask:
    task_id: str
    asset_id: str
    submit_seq: int
    memory_bytes: int
    work_fn: Callable[[], Any]
    parameters_version: str


class TaskScheduler:
    """Memory-budgeted, stable-order asset task scheduler.

    One logical archival task is accepted per asset. Segment-level parallelism is
    performed inside the task with the same configured worker ceiling.
    """

    def __init__(self, config: Config, trace: TraceLogger | None = None):
        self.config = config
        self.trace = trace
        self._next_seq = 0
        self._pending: list[ScheduledTask] = []
        self._asset_states: dict[str, str] = {}
        self._lock = threading.RLock()
        self._used_memory = 0
        self._active_count = 0
        self._memory_wait_assets: set[str] = set()
        self._results: dict[str, Any] = {}
        self._errors: dict[str, AppError] = {}

    def _record(self, event: str, **fields: Any) -> None:
        if self.trace is not None:
            self.trace.record(event, **fields)

    def submit(
        self,
        asset_id: str,
        work_fn: Callable[[], Any],
        memory_bytes: int,
        *,
        task_id: str | None = None,
        parameters_version: str = "v3",
    ) -> int:
        with self._lock:
            if asset_id in self._asset_states:
                raise AppError(
                    E_BUSY,
                    f"asset {asset_id!r} already has a queued or running task",
                    field="asset_id",
                    details={"asset_id": asset_id, "state": self._asset_states[asset_id]},
                )
            if memory_bytes <= 0:
                raise AppError(
                    E_MEMORY_BUDGET,
                    "task memory requirement must be positive",
                    field="memory_bytes",
                )
            if memory_bytes > self.config.per_task_memory_bytes:
                raise AppError(
                    E_MEMORY_BUDGET,
                    "task memory requirement exceeds the configured per-task budget",
                    field="memory_bytes",
                    details={
                        "required": memory_bytes,
                        "per_task_memory_bytes": self.config.per_task_memory_bytes,
                    },
                )
            if memory_bytes > self.config.global_memory_bytes:
                raise AppError(
                    E_MEMORY_BUDGET,
                    "task memory requirement exceeds the configured global budget",
                    field="memory_bytes",
                    details={
                        "required": memory_bytes,
                        "global_memory_bytes": self.config.global_memory_bytes,
                    },
                )
            seq = self._next_seq
            self._next_seq += 1
            task = ScheduledTask(
                task_id=task_id or f"task-{seq:06d}",
                asset_id=asset_id,
                submit_seq=seq,
                memory_bytes=memory_bytes,
                work_fn=work_fn,
                parameters_version=parameters_version,
            )
            self._pending.append(task)
            self._asset_states[asset_id] = "queued"
            self._record(
                "task_submit",
                asset_id=asset_id,
                task_id=task.task_id,
                submit_seq=seq,
                memory_bytes=memory_bytes,
                parameters_version=parameters_version,
                state="queued",
            )
            return seq

    def run_all(self) -> dict[str, Any]:
        futures: dict[Future[Any], ScheduledTask] = {}
        with ThreadPoolExecutor(
            max_workers=self.config.max_parallel,
            thread_name_prefix="loudness-asset",
        ) as executor:
            while True:
                with self._lock:
                    for task in sorted(self._pending, key=lambda item: item.submit_seq):
                        if self._active_count >= self.config.max_parallel:
                            break
                        free_memory = self.config.global_memory_bytes - self._used_memory
                        if task.memory_bytes > free_memory:
                            if task.asset_id not in self._memory_wait_assets:
                                self._memory_wait_assets.add(task.asset_id)
                                self._record(
                                    "task_wait_memory",
                                    asset_id=task.asset_id,
                                    task_id=task.task_id,
                                    submit_seq=task.submit_seq,
                                    required_memory_bytes=task.memory_bytes,
                                    used_memory_bytes=self._used_memory,
                                    global_memory_bytes=self.config.global_memory_bytes,
                                    state="waiting",
                                )
                            continue
                        self._pending.remove(task)
                        self._memory_wait_assets.discard(task.asset_id)
                        self._used_memory += task.memory_bytes
                        self._active_count += 1
                        self._asset_states[task.asset_id] = "running"
                        self._record(
                            "task_start",
                            asset_id=task.asset_id,
                            task_id=task.task_id,
                            submit_seq=task.submit_seq,
                            memory_bytes=task.memory_bytes,
                            used_memory_bytes=self._used_memory,
                            active_tasks=self._active_count,
                            state="running",
                        )
                        futures[executor.submit(task.work_fn)] = task

                if not futures and not self._pending:
                    break
                if not futures:
                    raise AppError(
                        E_MEMORY_BUDGET,
                        "no queued task can fit the configured global memory budget",
                        field="global_memory_bytes",
                    )
                done, _ = wait(futures.keys(), return_when=FIRST_COMPLETED)
                for future in done:
                    task = futures.pop(future)
                    try:
                        result = future.result()
                        self._results[task.task_id] = result
                        self._record(
                            "task_complete",
                            asset_id=task.asset_id,
                            task_id=task.task_id,
                            submit_seq=task.submit_seq,
                            state="complete",
                        )
                    except AppError as exc:
                        self._errors[task.task_id] = exc
                        self._record(
                            "task_failed",
                            asset_id=task.asset_id,
                            task_id=task.task_id,
                            submit_seq=task.submit_seq,
                            code=exc.code,
                            field=exc.field,
                            state="failed",
                        )
                    except Exception as exc:  # noqa: BLE001 - defensive worker boundary
                        wrapped = AppError(
                            "E_TASK_FAILED",
                            f"unexpected task failure: {exc}",
                            details={"type": type(exc).__name__},
                        )
                        self._errors[task.task_id] = wrapped
                        self._record(
                            "task_failed",
                            asset_id=task.asset_id,
                            task_id=task.task_id,
                            submit_seq=task.submit_seq,
                            code=wrapped.code,
                            state="failed",
                        )
                    with self._lock:
                        self._used_memory -= task.memory_bytes
                        self._active_count -= 1
                        self._asset_states.pop(task.asset_id, None)
        return dict(self._results)

    @property
    def errors(self) -> dict[str, AppError]:
        return dict(self._errors)

    @property
    def pending_count(self) -> int:
        with self._lock:
            return len(self._pending)

