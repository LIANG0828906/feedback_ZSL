"""Bounded worker, sample-buffer, and intersection-cache budgets."""

from __future__ import annotations

import contextlib
import threading
import time
from collections import OrderedDict
from dataclasses import dataclass
from typing import Any, Iterator

from .errors import parameter_error


@dataclass(frozen=True)
class ResourceLimits:
    max_parallelism: int = 2
    per_thread_sample_buffer: int = 4096
    intersection_cache_budget: int = 512
    max_pending_blocks: int = 8

    def __post_init__(self) -> None:
        for field_name, value in (
            ("max_parallelism", self.max_parallelism),
            ("per_thread_sample_buffer", self.per_thread_sample_buffer),
            ("intersection_cache_budget", self.intersection_cache_budget),
            ("max_pending_blocks", self.max_pending_blocks),
        ):
            if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
                raise parameter_error(f"resources.{field_name}", "resource budget must be a positive integer")

    def to_dict(self) -> dict[str, int]:
        return {
            "max_parallelism": self.max_parallelism,
            "per_thread_sample_buffer": self.per_thread_sample_buffer,
            "intersection_cache_budget": self.intersection_cache_budget,
            "max_pending_blocks": self.max_pending_blocks,
        }


class BoundedIntersectionCache:
    def __init__(self, capacity: int) -> None:
        self.capacity = capacity
        self._data: OrderedDict[Any, Any] = OrderedDict()
        self._lock = threading.RLock()
        self.hits = 0
        self.misses = 0
        self.evictions = 0

    def get(self, key: Any) -> tuple[bool, Any]:
        with self._lock:
            if key in self._data:
                value = self._data.pop(key)
                self._data[key] = value
                self.hits += 1
                return True, value
            self.misses += 1
            return False, None

    def put(self, key: Any, value: Any) -> None:
        with self._lock:
            if key in self._data:
                self._data.pop(key)
            self._data[key] = value
            while len(self._data) > self.capacity:
                self._data.popitem(last=False)
                self.evictions += 1

    def snapshot(self) -> dict[str, int]:
        with self._lock:
            return {
                "capacity": self.capacity,
                "size": len(self._data),
                "hits": self.hits,
                "misses": self.misses,
                "evictions": self.evictions,
            }


class ResourceGovernor:
    """Blocking semaphores; work is queued, never silently dropped."""

    def __init__(self, limits: ResourceLimits) -> None:
        self.limits = limits
        self._threads = threading.BoundedSemaphore(limits.max_parallelism)
        self._cache = threading.BoundedSemaphore(limits.intersection_cache_budget)
        self._samples = threading.BoundedSemaphore(
            limits.per_thread_sample_buffer * limits.max_parallelism
        )
        self._stats_lock = threading.Lock()
        self._stats = {
            "worker_waits": 0,
            "cache_waits": 0,
            "sample_waits": 0,
            "peak_cache_units": 0,
            "peak_sample_units": 0,
            "cache_units_in_use": 0,
            "sample_units_in_use": 0,
        }
        self.intersection_cache = BoundedIntersectionCache(limits.intersection_cache_budget)

    @contextlib.contextmanager
    def thread_slot(self) -> Iterator[None]:
        started = time.perf_counter()
        acquired = self._threads.acquire(timeout=30.0)
        waited = time.perf_counter() - started
        if not acquired:
            raise TimeoutError("worker pool budget wait exceeded 30 seconds")
        with self._stats_lock:
            if waited > 1.0e-6:
                self._stats["worker_waits"] += 1
        try:
            yield
        finally:
            self._threads.release()

    @contextlib.contextmanager
    def lease(self, cache_units: int, sample_units: int) -> Iterator[None]:
        if cache_units <= 0 or sample_units <= 0:
            raise parameter_error("resources.lease", "lease units must be positive")
        cache_request = min(cache_units, self.limits.intersection_cache_budget)
        sample_request = min(
            sample_units,
            self.limits.per_thread_sample_buffer,
        )
        cache_started = time.perf_counter()
        cache_acquired = self._acquire_many(self._cache, cache_request, 30.0)
        cache_wait = time.perf_counter() - cache_started
        if cache_acquired != cache_request:
            self._release_many(self._cache, cache_acquired)
            raise TimeoutError("intersection cache budget wait exceeded 30 seconds")
        sample_started = time.perf_counter()
        sample_acquired = self._acquire_many(self._samples, sample_request, 30.0)
        sample_wait = time.perf_counter() - sample_started
        if sample_acquired != sample_request:
            self._release_many(self._cache, cache_request)
            self._release_many(self._samples, sample_acquired)
            raise TimeoutError("sample buffer budget wait exceeded 30 seconds")
        with self._stats_lock:
            if cache_wait > 1.0e-6:
                self._stats["cache_waits"] += 1
            if sample_wait > 1.0e-6:
                self._stats["sample_waits"] += 1
            self._stats["cache_units_in_use"] += cache_request
            self._stats["sample_units_in_use"] += sample_request
            self._stats["peak_cache_units"] = max(
                self._stats["peak_cache_units"], self._stats["cache_units_in_use"]
            )
            self._stats["peak_sample_units"] = max(
                self._stats["peak_sample_units"], self._stats["sample_units_in_use"]
            )
        try:
            yield
        finally:
            self._release_many(self._cache, cache_request)
            self._release_many(self._samples, sample_request)
            with self._stats_lock:
                self._stats["cache_units_in_use"] -= cache_request
                self._stats["sample_units_in_use"] -= sample_request

    @staticmethod
    def _acquire_many(semaphore: threading.BoundedSemaphore, count: int, timeout: float) -> int:
        acquired = 0
        deadline = time.perf_counter() + timeout
        while acquired < count:
            remaining = max(0.0, deadline - time.perf_counter())
            if not semaphore.acquire(timeout=remaining):
                break
            acquired += 1
        return acquired

    @staticmethod
    def _release_many(semaphore: threading.BoundedSemaphore, count: int) -> None:
        for _ in range(count):
            semaphore.release()

    def snapshot(self) -> dict[str, int]:
        with self._stats_lock:
            result = dict(self._stats)
        result.update({f"cache_{key}": value for key, value in self.intersection_cache.snapshot().items()})
        return result


__all__ = ["BoundedIntersectionCache", "ResourceGovernor", "ResourceLimits"]
