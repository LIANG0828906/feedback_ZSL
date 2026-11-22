"""Bounded deterministic worker-pool execution."""

from __future__ import annotations

import threading
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass
from typing import Callable, Generic, Iterable, TypeVar

T = TypeVar("T")
R = TypeVar("R")


@dataclass(frozen=True)
class Outcome(Generic[T, R]):
    item: T
    value: R | None
    error: BaseException | None
    elapsed_ms: float


class BoundedExecutor:
    """A stable-order executor with a bounded in-flight buffer."""

    def __init__(self, max_workers: int, per_worker_buffer: int) -> None:
        if max_workers < 1:
            raise ValueError("max_workers must be positive")
        if per_worker_buffer < 1:
            raise ValueError("per_worker_buffer must be positive")
        self.max_workers = max_workers
        self.per_worker_buffer = per_worker_buffer
        self._slots = threading.BoundedSemaphore(max_workers * per_worker_buffer)

    def run_ordered(
        self,
        items: Iterable[T],
        worker: Callable[[T], R],
    ) -> list[Outcome[T, R]]:
        item_list = list(items)
        outcomes: list[Outcome[T, R]] = []
        with ThreadPoolExecutor(max_workers=self.max_workers) as pool:
            pending: list[tuple[T, Future[R], float]] = []
            import time

            for item in item_list:
                self._slots.acquire()
                started = time.perf_counter()

                def run(value: T = item, start: float = started) -> R:
                    try:
                        return worker(value)
                    finally:
                        self._slots.release()

                pending.append((item, pool.submit(run), started))
            for item, future, started in pending:
                try:
                    value = future.result()
                    error = None
                except BaseException as exc:  # unit failure must not cancel siblings
                    value = None
                    error = exc
                outcomes.append(
                    Outcome(
                        item=item,
                        value=value,
                        error=error,
                        elapsed_ms=(time.perf_counter() - started) * 1000.0,
                    )
                )
        return outcomes
