"""Bounded thread-pool helper.

The pool limits both worker count and the number of outstanding submitted jobs.
Independent matches may run in parallel; one Engine must remain single-owner.
"""

from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any, Callable, Iterable

from .codes import E_ARG, TickArenaError


class BoundedParallel:
    def __init__(self, max_workers: int = 2, queue_limit: int = 4) -> None:
        if isinstance(max_workers, bool) or not isinstance(max_workers, int) or max_workers < 1:
            raise TickArenaError(E_ARG, "max_workers must be a positive integer")
        if (
            isinstance(queue_limit, bool)
            or not isinstance(queue_limit, int)
            or queue_limit < 1
        ):
            raise TickArenaError(E_ARG, "queue_limit must be a positive integer")
        self.max_workers = max_workers
        self.queue_limit = queue_limit

    def map(self, function: Callable[[Any], Any], items: Iterable[Any]) -> list[Any]:
        materialized = list(items)
        if not materialized:
            return []
        results: list[Any] = [None] * len(materialized)
        semaphore = threading.BoundedSemaphore(self.queue_limit)
        workers = min(self.max_workers, len(materialized))
        with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="tickarena") as pool:
            future_index: dict[Any, int] = {}
            for index, item in enumerate(materialized):
                semaphore.acquire()
                future = pool.submit(function, item)
                future.add_done_callback(lambda _future: semaphore.release())
                future_index[future] = index
            for future in as_completed(future_index):
                results[future_index[future]] = future.result()
        return results

