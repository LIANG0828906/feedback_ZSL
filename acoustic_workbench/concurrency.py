"""Bounded parallel execution with deterministic ordered consumption."""

from __future__ import annotations

from collections import deque
from concurrent.futures import Future, ThreadPoolExecutor
from typing import Callable, Iterable, Iterator, TypeVar

from .codes import E_PARA
from .errors import error

T = TypeVar("T")
R = TypeVar("R")


class BoundedParallelRunner:
    def __init__(
        self,
        max_workers: int,
        per_thread_buffer_bytes: int,
        *,
        stage: str = "parallel",
    ) -> None:
        if not isinstance(max_workers, int) or not 1 <= max_workers <= 16:
            raise error(
                E_PARA,
                "max_workers must be in [1, 16]",
                stage=stage,
                field="max_parallelism",
                context={"value": max_workers},
            )
        if per_thread_buffer_bytes < 1:
            raise error(
                E_PARA,
                "per-thread buffer limit must be positive",
                stage=stage,
                field="thread_buffer_mb",
                context={"value": per_thread_buffer_bytes},
            )
        self.max_workers = max_workers
        self.per_thread_buffer_bytes = per_thread_buffer_bytes
        self.stage = stage

    def map_ordered(
        self,
        items: Iterable[T],
        worker: Callable[[T], R],
        *,
        buffer_estimator: Callable[[T], int] | None = None,
    ) -> Iterator[R]:
        item_list = list(items)
        if not item_list:
            return
        if buffer_estimator:
            for item in item_list:
                estimated = int(buffer_estimator(item))
                if estimated > self.per_thread_buffer_bytes:
                    raise error(
                        E_PARA,
                        "per-thread buffer limit would be exceeded",
                        stage=self.stage,
                        field="thread_buffer_mb",
                        context={
                            "estimated_bytes": estimated,
                            "limit_bytes": self.per_thread_buffer_bytes,
                            "item": str(item),
                        },
                    )
        if self.max_workers == 1 or len(item_list) == 1:
            for item in item_list:
                yield worker(item)
            return

        executor = ThreadPoolExecutor(max_workers=self.max_workers, thread_name_prefix="aw")
        pending: deque[Future[R]] = deque()
        iterator = iter(item_list)
        try:
            for _ in range(min(self.max_workers, len(item_list))):
                pending.append(executor.submit(worker, next(iterator)))
            while pending:
                future = pending.popleft()
                yield future.result()
                try:
                    pending.append(executor.submit(worker, next(iterator)))
                except StopIteration:
                    pass
        finally:
            for future in pending:
                future.cancel()
            executor.shutdown(wait=True, cancel_futures=True)
