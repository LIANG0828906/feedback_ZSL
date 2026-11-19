from __future__ import annotations

import threading

from .metrics import Metrics


class RuntimeStats:
    def __init__(self, metrics: Metrics, worker_threads: int) -> None:
        self.metrics = metrics
        self.worker_threads = max(1, worker_threads)
        self._lock = threading.RLock()
        self._total = 0
        self._per_tenant: dict[str, int] = {}
        self._peak = 0
        self._peak_queue = 0

    def enter(self, tenant_id: str) -> None:
        with self._lock:
            self._total += 1
            self._per_tenant[tenant_id] = self._per_tenant.get(tenant_id, 0) + 1
            self._peak = max(self._peak, self._total)
            tenant_inflight = self._per_tenant[tenant_id]
            total = self._total
            peak = self._peak
        self.metrics.set(
            "order_guard_inflight_requests",
            tenant_inflight,
            {"tenant": tenant_id},
        )
        self.metrics.set("order_guard_inflight_total", total)
        self.metrics.set("order_guard_inflight_peak", peak)

    def leave(self, tenant_id: str) -> None:
        with self._lock:
            self._total = max(0, self._total - 1)
            self._per_tenant[tenant_id] = max(
                0, self._per_tenant.get(tenant_id, 0) - 1
            )
            tenant_inflight = self._per_tenant[tenant_id]
            total = self._total
        self.metrics.set(
            "order_guard_inflight_requests",
            tenant_inflight,
            {"tenant": tenant_id},
        )
        self.metrics.set("order_guard_inflight_total", total)

    def total(self) -> int:
        with self._lock:
            return self._total

    def peak(self) -> int:
        with self._lock:
            return self._peak

    def observe_queue(self, active_queries: int) -> tuple[int, float]:
        total = self.total()
        queue_depth = max(0, total - max(0, active_queries))
        occupancy = min(1.0, total / self.worker_threads)
        with self._lock:
            self._peak_queue = max(self._peak_queue, queue_depth)
            peak_queue = self._peak_queue
        self.metrics.set("order_guard_backlog_depth", queue_depth)
        self.metrics.set("order_guard_backlog_peak", peak_queue)
        self.metrics.set(
            "order_guard_worker_thread_occupancy_ratio", occupancy
        )
        return queue_depth, occupancy

    def peak_queue(self) -> int:
        with self._lock:
            return self._peak_queue

    def snapshot(self) -> dict[str, object]:
        with self._lock:
            return {
                "inflight_total": self._total,
                "inflight_peak": self._peak,
                "backlog_peak": self._peak_queue,
                "per_tenant_inflight": dict(self._per_tenant),
            }
