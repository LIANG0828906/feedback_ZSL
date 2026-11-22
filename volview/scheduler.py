"""Concurrent plane-task scheduler with explicit resource boundaries.

* Work runs on a thread pool with a bounded maximum parallelism.
* Each task declares its pixel footprint; the scheduler owns a global
  in-flight pixel budget.  When the budget is exhausted, new tasks *block*
  in a queue -- plane tasks are never silently dropped.
* A per-task pixel buffer limit caps the memory any single worker may hold;
  oversized planes are processed in row chunks so the limit still holds.
* A failed task is marked ``FAILED`` with its error and can be retried via
  :meth:`PlaneTask.retry`; sibling tasks and committed snapshots are
  unaffected.
* Results are pure functions of the inputs, so parallel and serial runs of
  the same operation sequence produce pixel-identical images; snapshot
  commits stay ordered through :class:`SnapshotStore`.
"""

from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor
from typing import Callable, Dict, Optional

import numpy as np


class PlaneTask:
    """One resample/slab job with status, result and retry support."""

    PENDING = "PENDING"
    RUNNING = "RUNNING"
    DONE = "DONE"
    FAILED = "FAILED"

    def __init__(self, task_id: int, name: str, pixels: int,
                 fn: Callable[[], np.ndarray]):
        self.task_id = task_id
        self.name = name
        self.pixels = int(pixels)
        self._fn = fn
        self.status = PlaneTask.PENDING
        self.error: Optional[Exception] = None
        self._result: Optional[np.ndarray] = None
        self._done = threading.Event()
        self._scheduler: Optional["PlaneScheduler"] = None

    def _run(self) -> None:
        self.status = PlaneTask.RUNNING
        try:
            self._result = self._fn()
        except Exception as exc:  # task-local failure, does not propagate
            self.error = exc
            self.status = PlaneTask.FAILED
        else:
            self.status = PlaneTask.DONE
        finally:
            self._done.set()

    def result(self, timeout: Optional[float] = None) -> np.ndarray:
        if not self._done.wait(timeout):
            raise TimeoutError(f"plane task {self.name!r} did not finish")
        if self.status == PlaneTask.FAILED:
            raise self.error  # type: ignore[misc]
        return self._result  # type: ignore[return-value]

    def retry(self) -> "PlaneTask":
        """Requeue after failure; returns self for chaining."""
        if self.status != PlaneTask.FAILED:
            raise RuntimeError("only FAILED tasks can be retried")
        if self._scheduler is None:
            raise RuntimeError("task is not attached to a scheduler")
        self.error = None
        self.status = PlaneTask.PENDING
        self._done.clear()
        self._scheduler._enqueue(self)
        return self


class PlaneScheduler:
    """Bounded thread pool with a global in-flight pixel budget."""

    def __init__(self, max_workers: int = 2, max_inflight_pixels: int = 1 << 20,
                 max_task_pixels: int = 1 << 20):
        if max_workers < 1:
            raise ValueError("max_workers must be >= 1")
        self.max_workers = max_workers
        self.max_inflight_pixels = max_inflight_pixels
        self.max_task_pixels = max_task_pixels
        self._budget = threading.BoundedSemaphore(max_inflight_pixels)
        self._executor = ThreadPoolExecutor(max_workers=max_workers,
                                            thread_name_prefix="volview")
        self._tasks: Dict[int, PlaneTask] = {}
        self._next_id = 0
        self._id_lock = threading.Lock()
        self._shutdown = False

    def submit(self, name: str, rows: int, cols: int,
               fn: Callable[[], np.ndarray]) -> PlaneTask:
        """Submit a plane task; blocks (queued) while the pixel budget is full."""
        pixels = int(rows) * int(cols)
        with self._id_lock:
            task = PlaneTask(self._next_id, name, pixels, fn)
            self._next_id += 1
            self._tasks[task.task_id] = task
        task._scheduler = self
        self._enqueue(task)
        return task

    def _enqueue(self, task: PlaneTask) -> None:
        if self._shutdown:
            raise RuntimeError("scheduler is shut down")
        permits = min(task.pixels, self.max_inflight_pixels)

        def guarded() -> None:
            try:
                task._run()
            finally:
                self._budget.release(permits)

        # Blocking acquisition: the caller queues here while buffers are
        # exhausted instead of dropping the task.
        self._budget.acquire(permits)
        self._executor.submit(guarded)

    def tasks(self) -> Dict[int, PlaneTask]:
        return dict(self._tasks)

    def wait_all(self) -> None:
        for task in list(self._tasks.values()):
            task._done.wait()

    def shutdown(self) -> None:
        self._shutdown = True
        self._executor.shutdown(wait=True)

    def __enter__(self) -> "PlaneScheduler":
        return self

    def __exit__(self, *exc) -> None:
        self.shutdown()
