from __future__ import annotations

import heapq
import json
import os
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Iterable

from .errors import BusinessError, E_BUDGET_EXCEEDED, E_CANCELLED
from .mesh import Mesh
from .topology import Topology, build_topology


# Explicit deterministic accounting contract. Python container overhead and
# allocator behavior vary by interpreter, so every count is translated through
# a documented byte coefficient rather than using process RSS.
VERTEX_BYTES = 3 * 8
FACE_BYTES = 3 * 8
EDGE_TABLE_BYTES_PER_EDGE = 88
ADJACENCY_BYTES_PER_DIRECTED_EDGE = 8
ADJACENCY_BYTES_PER_VERTEX = 8
TEMP_BYTES_PER_VERTEX = 64
TEMP_BYTES_PER_FACE = 112
TEMP_BYTES_PER_EDGE = 48


@dataclass(frozen=True)
class MemoryUsage:
    vertex_count: int
    face_count: int
    edge_count: int
    vertex_bytes: int
    face_bytes: int
    edge_table_bytes: int
    adjacency_bytes: int
    temp_buffer_bytes: int
    total_bytes: int

    def to_dict(self) -> dict[str, int]:
        return {
            "vertex_count": self.vertex_count,
            "face_count": self.face_count,
            "edge_count": self.edge_count,
            "vertex_bytes": self.vertex_bytes,
            "face_bytes": self.face_bytes,
            "edge_table_bytes": self.edge_table_bytes,
            "adjacency_bytes": self.adjacency_bytes,
            "temp_buffer_bytes": self.temp_buffer_bytes,
            "total_bytes": self.total_bytes,
        }


def estimate_memory(
    vertex_count: int, face_count: int, edge_count: int
) -> MemoryUsage:
    vertex_bytes = int(vertex_count) * VERTEX_BYTES
    face_bytes = int(face_count) * FACE_BYTES
    edge_table_bytes = int(edge_count) * EDGE_TABLE_BYTES_PER_EDGE
    adjacency_bytes = (
        int(face_count) * 3 * ADJACENCY_BYTES_PER_DIRECTED_EDGE
        + int(vertex_count) * ADJACENCY_BYTES_PER_VERTEX
    )
    temp_buffer_bytes = (
        int(vertex_count) * TEMP_BYTES_PER_VERTEX
        + int(face_count) * TEMP_BYTES_PER_FACE
        + int(edge_count) * TEMP_BYTES_PER_EDGE
    )
    return MemoryUsage(
        vertex_count=int(vertex_count),
        face_count=int(face_count),
        edge_count=int(edge_count),
        vertex_bytes=vertex_bytes,
        face_bytes=face_bytes,
        edge_table_bytes=edge_table_bytes,
        adjacency_bytes=adjacency_bytes,
        temp_buffer_bytes=temp_buffer_bytes,
        total_bytes=(
            vertex_bytes
            + face_bytes
            + edge_table_bytes
            + adjacency_bytes
            + temp_buffer_bytes
        ),
    )


def measure_mesh(mesh: Mesh, topology: Topology | None = None) -> MemoryUsage:
    topology = topology or build_topology(mesh)
    return estimate_memory(mesh.vertex_count, mesh.face_count, topology.edge_count)


@dataclass(frozen=True)
class BlockTask:
    block_id: str
    submit_seq: int
    usage: MemoryUsage
    payload: Any = None


@dataclass
class TaskResult:
    block_id: str
    submit_seq: int
    payload: Any = None
    error: BusinessError | None = None
    started: bool = False
    committed: bool = False
    extra: dict[str, Any] = field(default_factory=dict)


class TraceWriter:
    """Thread-safe JSONL event log with a stable event sequence."""

    def __init__(
        self,
        path: str | os.PathLike[str],
        *,
        run_id: str,
        echo: bool = False,
    ) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.run_id = run_id
        self.echo = echo
        self._lock = threading.Lock()
        self._seq = 0
        # Start each run with a clean trace; recovery appends to the new run's
        # trace while block commit records remain immutable on disk.
        self.path.write_text("", encoding="utf-8")

    def emit(self, event: str, **fields: Any) -> dict[str, Any]:
        with self._lock:
            self._seq += 1
            record = {
                "seq": self._seq,
                "ts": datetime.now(timezone.utc).isoformat(timespec="milliseconds"),
                "run_id": self.run_id,
                "event": event,
                **fields,
            }
            line = json.dumps(record, sort_keys=True, separators=(",", ":"))
            with self.path.open("a", encoding="utf-8") as stream:
                stream.write(line + "\n")
                stream.flush()
                os.fsync(stream.fileno())
        if self.echo:
            print(line, flush=True)
        return record


Worker = Callable[[BlockTask, threading.Event], Any]


class BudgetScheduler:
    """Parallel scheduler with stable submission order and wakeful budgeting."""

    def __init__(
        self,
        *,
        global_budget_bytes: int,
        task_budget_bytes: int,
        parallelism: int,
        trace: TraceWriter,
    ) -> None:
        if global_budget_bytes <= 0 or task_budget_bytes <= 0:
            raise ValueError("budgets must be positive")
        if parallelism < 1:
            raise ValueError("parallelism must be at least one")
        if global_budget_bytes < task_budget_bytes:
            # This is valid, but no individual task can use more than global.
            pass
        self.global_budget_bytes = int(global_budget_bytes)
        self.task_budget_bytes = int(task_budget_bytes)
        self.parallelism = int(parallelism)
        self.trace = trace
        self._condition = threading.Condition()
        self._used_bytes = 0
        self._active_blocks: set[str] = set()
        self._max_used_bytes = 0
        self._max_active = 0
        self._next_start_rank = 0

    def _acquire(
        self,
        task: BlockTask,
        start_rank: int,
        cancel_event: threading.Event,
    ) -> bool:
        waited = False
        with self._condition:
            while start_rank != self._next_start_rank:
                if cancel_event.is_set():
                    return False
                self._condition.wait(timeout=0.25)
            while self._used_bytes + task.usage.total_bytes > self.global_budget_bytes:
                if cancel_event.is_set():
                    return False
                if not waited:
                    self.trace.emit(
                        "budget_wait",
                        block_id=task.block_id,
                        submit_seq=task.submit_seq,
                        requested_bytes=task.usage.total_bytes,
                        used_bytes=self._used_bytes,
                        global_budget_bytes=self.global_budget_bytes,
                    )
                    waited = True
                self._condition.wait(timeout=0.25)
            if cancel_event.is_set():
                return False
            self._used_bytes += task.usage.total_bytes
            self._max_used_bytes = max(self._max_used_bytes, self._used_bytes)
            self._active_blocks.add(task.block_id)
            self._max_active = max(self._max_active, len(self._active_blocks))
            self._next_start_rank += 1
            self.trace.emit(
                "budget_grant",
                block_id=task.block_id,
                submit_seq=task.submit_seq,
                granted_bytes=task.usage.total_bytes,
                used_bytes=self._used_bytes,
                global_budget_bytes=self.global_budget_bytes,
                waited=waited,
            )
            self._condition.notify_all()
            return True

    def _release(self, task: BlockTask) -> int:
        with self._condition:
            self._used_bytes = max(0, self._used_bytes - task.usage.total_bytes)
            self._active_blocks.discard(task.block_id)
            used = self._used_bytes
            self._condition.notify_all()
        self.trace.emit(
            "budget_release",
            block_id=task.block_id,
            submit_seq=task.submit_seq,
            released_bytes=task.usage.total_bytes,
            used_bytes=used,
        )
        return used

    def execute(
        self,
        tasks: Iterable[BlockTask],
        worker: Worker,
        *,
        cancel_event: threading.Event | None = None,
    ) -> list[TaskResult]:
        cancel_event = cancel_event or threading.Event()
        ordered = sorted(tasks, key=lambda task: (task.submit_seq, task.block_id))
        if len({task.block_id for task in ordered}) != len(ordered):
            raise ValueError("block ids must be unique")

        results: dict[str, TaskResult] = {
            task.block_id: TaskResult(task.block_id, task.submit_seq) for task in ordered
        }
        admitted: list[BlockTask] = []
        effective_limit = min(self.global_budget_bytes, self.task_budget_bytes)
        for task in ordered:
            if task.usage.total_bytes > self.task_budget_bytes:
                error = BusinessError(
                    E_BUDGET_EXCEEDED,
                    "block exceeds its per-task memory budget and was not started",
                    location={"block_id": task.block_id, "submit_seq": task.submit_seq},
                    context={
                        "required_bytes": task.usage.total_bytes,
                        "task_budget_bytes": self.task_budget_bytes,
                        "usage": task.usage.to_dict(),
                    },
                )
                results[task.block_id].error = error
                self.trace.emit(
                    "budget_reject",
                    block_id=task.block_id,
                    submit_seq=task.submit_seq,
                    required_bytes=task.usage.total_bytes,
                    task_budget_bytes=self.task_budget_bytes,
                    reason="per_task",
                )
            elif task.usage.total_bytes > effective_limit:
                error = BusinessError(
                    E_BUDGET_EXCEEDED,
                    "block cannot fit inside the global memory budget",
                    location={"block_id": task.block_id, "submit_seq": task.submit_seq},
                    context={
                        "required_bytes": task.usage.total_bytes,
                        "global_budget_bytes": self.global_budget_bytes,
                        "usage": task.usage.to_dict(),
                    },
                )
                results[task.block_id].error = error
                self.trace.emit(
                    "budget_reject",
                    block_id=task.block_id,
                    submit_seq=task.submit_seq,
                    required_bytes=task.usage.total_bytes,
                    global_budget_bytes=self.global_budget_bytes,
                    reason="global",
                )
            else:
                admitted.append(task)

        self.trace.emit(
            "scheduler_start",
            parallelism=self.parallelism,
            global_budget_bytes=self.global_budget_bytes,
            task_budget_bytes=self.task_budget_bytes,
            submitted_count=len(ordered),
            admitted_count=len(admitted),
            submit_order=[task.block_id for task in ordered],
        )
        if not admitted:
            self.trace.emit("scheduler_finish", started_count=0, max_used_bytes=0)
            return [results[task.block_id] for task in ordered]

        heap: list[tuple[int, str, BlockTask]] = [
            (task.submit_seq, task.block_id, task) for task in admitted
        ]
        heapq.heapify(heap)
        start_ranks = {
            task.block_id: rank for rank, task in enumerate(admitted)
        }
        with self._condition:
            self._next_start_rank = 0
        heap_lock = threading.Lock()

        def take_task() -> BlockTask | None:
            with heap_lock:
                if not heap or cancel_event.is_set():
                    return None
                return heapq.heappop(heap)[2]

        def run_worker() -> None:
            while True:
                if cancel_event.is_set():
                    return
                task = take_task()
                if task is None:
                    return
                result = results[task.block_id]
                if not self._acquire(task, start_ranks[task.block_id], cancel_event):
                    result.error = BusinessError(
                        E_CANCELLED,
                        "block was cancelled before resource acquisition",
                        location={"block_id": task.block_id},
                    )
                    self.trace.emit(
                        "task_cancel", block_id=task.block_id, phase="budget_wait"
                    )
                    return
                result.started = True
                self.trace.emit(
                    "task_start",
                    block_id=task.block_id,
                    submit_seq=task.submit_seq,
                    used_bytes=self._used_bytes,
                )
                try:
                    if cancel_event.is_set():
                        raise BusinessError(
                            E_CANCELLED,
                            "block was cancelled at a cooperative checkpoint",
                            location={"block_id": task.block_id},
                        )
                    result.payload = worker(task, cancel_event)
                    self.trace.emit(
                        "task_finish", block_id=task.block_id, submit_seq=task.submit_seq
                    )
                except BusinessError as exc:
                    result.error = exc
                    self.trace.emit(
                        "task_error",
                        block_id=task.block_id,
                        submit_seq=task.submit_seq,
                        code=exc.code,
                        subcode=exc.subcode,
                        message=exc.message,
                    )
                except Exception as exc:  # pragma: no cover - defensive boundary
                    result.error = BusinessError(
                        "E_INTERNAL",
                        f"unexpected block failure: {type(exc).__name__}: {exc}",
                        location={"block_id": task.block_id},
                    )
                    self.trace.emit(
                        "task_error",
                        block_id=task.block_id,
                        submit_seq=task.submit_seq,
                        code="E_INTERNAL",
                        message=str(exc),
                    )
                finally:
                    self._release(task)

        threads = [
            threading.Thread(target=run_worker, name=f"mesh-repair-{index}", daemon=True)
            for index in range(self.parallelism)
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            while thread.is_alive():
                thread.join(timeout=0.1)

        # Any task not admitted by the worker pool because of cancellation is
        # surfaced explicitly rather than silently disappearing.
        for task in admitted:
            if not results[task.block_id].started and results[task.block_id].error is None:
                results[task.block_id].error = BusinessError(
                    E_CANCELLED,
                    "block was not started because cancellation was requested",
                    location={"block_id": task.block_id},
                )

        self.trace.emit(
            "scheduler_finish",
            started_count=sum(1 for result in results.values() if result.started),
            max_used_bytes=self._max_used_bytes,
            max_active_blocks=self._max_active,
            parallelism=self.parallelism,
        )
        return [results[task.block_id] for task in ordered]

    def preflight(self, tasks: Iterable[BlockTask]) -> dict[str, BusinessError]:
        """Reject impossible tasks before any worker or commit starts."""

        ordered = sorted(tasks, key=lambda task: (task.submit_seq, task.block_id))
        effective_limit = min(self.global_budget_bytes, self.task_budget_bytes)
        errors: dict[str, BusinessError] = {}
        for task in ordered:
            if task.usage.total_bytes > self.task_budget_bytes:
                errors[task.block_id] = BusinessError(
                    E_BUDGET_EXCEEDED,
                    "block exceeds its per-task memory budget and was not started",
                    location={"block_id": task.block_id, "submit_seq": task.submit_seq},
                    context={
                        "required_bytes": task.usage.total_bytes,
                        "task_budget_bytes": self.task_budget_bytes,
                        "usage": task.usage.to_dict(),
                    },
                )
                reason = "per_task"
            elif task.usage.total_bytes > effective_limit:
                errors[task.block_id] = BusinessError(
                    E_BUDGET_EXCEEDED,
                    "block cannot fit inside the global memory budget",
                    location={"block_id": task.block_id, "submit_seq": task.submit_seq},
                    context={
                        "required_bytes": task.usage.total_bytes,
                        "global_budget_bytes": self.global_budget_bytes,
                        "usage": task.usage.to_dict(),
                    },
                )
                reason = "global"
            else:
                continue
            self.trace.emit(
                "budget_preflight_reject",
                block_id=task.block_id,
                submit_seq=task.submit_seq,
                required_bytes=task.usage.total_bytes,
                reason=reason,
            )
        self.trace.emit(
            "budget_preflight",
            submitted_count=len(ordered),
            rejected_count=len(errors),
            global_budget_bytes=self.global_budget_bytes,
            task_budget_bytes=self.task_budget_bytes,
        )
        return errors
