"""Bounded worker pool for parallel window processing.

Authoritative advancement, client prediction and snapshot encoding may run
on a worker pool.  Parallelism, the task queue and the result buffer are
all bounded; when a bound is reached, producers *block* instead of silently
dropping work.  A window that raises is marked failed (and may be retried
by resubmission) without contaminating other windows.  After cancellation,
only already-persisted windows may be used for recovery.
"""

from __future__ import annotations

import queue
import threading
from dataclasses import dataclass
from typing import Any, Callable

from .errors import ParamError

STATUS_OK = "ok"
STATUS_FAILED = "failed"
STATUS_CANCELLED = "cancelled"


@dataclass
class WindowResult:
    window: int
    status: str
    payload: Any = None
    error: str | None = None


class WindowPool:
    def __init__(self, max_workers=4, queue_size=8, result_buffer=64):
        if max_workers < 1:
            raise ParamError("max_workers must be >= 1", field="max_workers")
        if queue_size < 1:
            raise ParamError("queue_size must be >= 1", field="queue_size")
        if result_buffer < 1:
            raise ParamError("result_buffer must be >= 1", field="result_buffer")
        self._tasks: queue.Queue = queue.Queue(maxsize=queue_size)
        self._results: queue.Queue = queue.Queue(maxsize=result_buffer)
        self._cancel = threading.Event()
        self._closed = False
        self._threads = [
            threading.Thread(target=self._worker, daemon=True, name=f"window-{i}")
            for i in range(max_workers)
        ]
        for thread in self._threads:
            thread.start()

    def submit(self, window: int, fn: Callable, *args, **kwargs):
        """Enqueue work; blocks while the task queue is full."""
        if self._closed:
            raise RuntimeError("pool is closed")
        self._tasks.put((window, fn, args, kwargs))  # blocking backpressure

    def poll(self, timeout=None) -> WindowResult:
        """Fetch one finished result; blocks while the buffer is empty."""
        return self._results.get(timeout=timeout)

    def _worker(self):
        while True:
            item = self._tasks.get()
            try:
                if item is None:
                    return
                window, fn, args, kwargs = item
                if self._cancel.is_set():
                    result = WindowResult(window, STATUS_CANCELLED)
                else:
                    try:
                        result = WindowResult(window, STATUS_OK, fn(*args, **kwargs))
                    except Exception as exc:  # window isolated from its peers
                        result = WindowResult(window, STATUS_FAILED, error=repr(exc))
                self._results.put(result)  # blocks if result buffer is full
            finally:
                self._tasks.task_done()

    def cancel(self):
        self._cancel.set()

    @property
    def cancelled(self):
        return self._cancel.is_set()

    def close(self):
        self._closed = True
        for _ in self._threads:
            self._tasks.put(None)
        for thread in self._threads:
            thread.join()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
