from __future__ import annotations

import math
import threading
from collections import deque
from dataclasses import dataclass
from typing import Deque

from rec_gateway.store import Store


@dataclass(frozen=True)
class Sample:
    outcome: str
    latency_ms: float


def percentile(values: list[float], quantile: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    index = max(0, math.ceil(quantile * len(ordered)) - 1)
    return round(ordered[index], 3)


class StatsManager:
    def __init__(
        self,
        versions: tuple[str, ...],
        tenants: tuple[str, ...],
        window_sizes: dict[str, int],
    ) -> None:
        self._lock = threading.RLock()
        self._versions = versions
        self._tenants = tenants
        self._window_sizes = {
            version: max(1, int(window_sizes.get(version, 200)))
            for version in versions
        }
        self._windows: dict[tuple[str, str], Deque[Sample]] = {}
        for version in versions:
            for tenant_id in tenants:
                self._windows[(version, tenant_id)] = deque(
                    maxlen=self._window_sizes[version]
                )

    def restore(self, store: Store) -> None:
        for version in self._versions:
            for tenant_id in self._tenants:
                rows = store.recent_window(
                    version, tenant_id, self._window_sizes[version]
                )
                with self._lock:
                    window = self._windows[(version, tenant_id)]
                    for row in rows:
                        window.append(
                            Sample(
                                outcome=row["outcome"],
                                latency_ms=float(row["latency_ms"]),
                            )
                        )

    def add(self, version: str, tenant_id: str, outcome: str, latency_ms: float) -> None:
        with self._lock:
            window = self._windows.get((version, tenant_id))
            if window is not None:
                window.append(
                    Sample(outcome=outcome, latency_ms=float(latency_ms))
                )

    def snapshot(
        self, version: str, tenant_id: str, limit: int | None = None
    ) -> dict[str, float | int | None]:
        with self._lock:
            samples = list(self._windows.get((version, tenant_id), ()))
        if limit is not None:
            samples = samples[-max(1, int(limit)) :]
        latencies = [sample.latency_ms for sample in samples]
        errors = sum(1 for sample in samples if sample.outcome != "success")
        timeouts = sum(1 for sample in samples if sample.outcome == "timeout")
        count = len(samples)
        return {
            "window_samples": count,
            "p50_ms": percentile(latencies, 0.50),
            "p95_ms": percentile(latencies, 0.95),
            "p99_ms": percentile(latencies, 0.99),
            "error_rate": round(errors / count, 6) if count else 0.0,
            "error_count": errors,
            "timeout_count": timeouts,
        }

    def quantiles(
        self, version: str, tenant_id: str, limit: int | None = None
    ) -> dict[str, float | None]:
        snapshot = self.snapshot(version, tenant_id, limit)
        return {
            "0.5": snapshot["p50_ms"],
            "0.95": snapshot["p95_ms"],
            "0.99": snapshot["p99_ms"],
        }

    def all_snapshots(self) -> dict[str, dict[str, dict[str, object]]]:
        return {
            version: {
                tenant_id: self.snapshot(version, tenant_id)
                for tenant_id in self._tenants
            }
            for version in self._versions
        }
