from __future__ import annotations

import threading
from concurrent.futures import Future, ThreadPoolExecutor
from collections.abc import Callable
from typing import Any


class BufferLimitError(RuntimeError):
    pass


class BoundedExecutor:
    """Thread pool with a hard bound on submitted-but-unfinished tasks."""

    def __init__(self, max_workers: int, max_pending: int, label: str = "worker") -> None:
        if max_workers < 1:
            raise ValueError("max_workers must be >= 1")
        if max_pending < max_workers:
            raise ValueError("max_pending must be >= max_workers")
        self.max_workers = max_workers
        self.max_pending = max_pending
        self.label = label
        self._slots = threading.BoundedSemaphore(max_pending)
        self._executor = ThreadPoolExecutor(
            max_workers=max_workers,
            thread_name_prefix=label,
        )
        self._closed = False
        self._lock = threading.Lock()

    def submit(self, function: Callable[..., Any], *args: Any, **kwargs: Any) -> Future:
        with self._lock:
            if self._closed:
                raise RuntimeError("executor is closed")
        if not self._slots.acquire(blocking=False):
            raise BufferLimitError(
                f"{self.label} buffer limit ({self.max_pending}) reached"
            )
        try:
            future = self._executor.submit(function, *args, **kwargs)
        except Exception:
            self._slots.release()
            raise

        def release(_: Future) -> None:
            self._slots.release()

        future.add_done_callback(release)
        return future

    def shutdown(self, wait: bool = True) -> None:
        with self._lock:
            self._closed = True
        self._executor.shutdown(wait=wait, cancel_futures=False)
