"""Rolling-window latency/error statistics and canary circuit breaker."""

from __future__ import annotations

import threading
import time
from collections import deque
from dataclasses import dataclass


def percentile(sorted_values: list[float], pct: float) -> float:
    if not sorted_values:
        return 0.0
    if len(sorted_values) == 1:
        return sorted_values[0]
    rank = (len(sorted_values) - 1) * pct
    lower = int(rank)
    upper = min(lower + 1, len(sorted_values) - 1)
    fraction = rank - lower
    return sorted_values[lower] * (1.0 - fraction) + sorted_values[upper] * fraction


@dataclass
class Sample:
    ts: float
    latency_ms: float
    is_error: bool
    is_timeout: bool


class RollingStats:
    """Thread-safe rolling window keyed per version and per tenant."""

    def __init__(self, window_seconds: float):
        self._window = window_seconds
        self._lock = threading.Lock()
        self._by_version: dict[str, deque] = {}
        self._by_tenant: dict[str, deque] = {}

    def record(
        self,
        version: str,
        tenant_id: str,
        latency_ms: float,
        is_error: bool,
        is_timeout: bool,
    ) -> None:
        now = time.time()
        sample = Sample(now, latency_ms, is_error, is_timeout)
        with self._lock:
            self._by_version.setdefault(version, deque()).append(sample)
            self._by_tenant.setdefault(tenant_id, deque()).append(sample)
            self._evict(self._by_version[version], now)
            self._evict(self._by_tenant[tenant_id], now)

    def _evict(self, samples: deque, now: float) -> None:
        cutoff = now - self._window
        while samples and samples[0].ts < cutoff:
            samples.popleft()

    def _summary(self, samples: deque) -> dict:
        values = sorted(item.latency_ms for item in samples)
        total = len(samples)
        errors = sum(1 for item in samples if item.is_error)
        timeouts = sum(1 for item in samples if item.is_timeout)
        return {
            "count": total,
            "error_count": errors,
            "error_rate": (errors / total) if total else 0.0,
            "p50_ms": round(percentile(values, 0.50), 3),
            "p90_ms": round(percentile(values, 0.90), 3),
            "p99_ms": round(percentile(values, 0.99), 3),
            "timeout_count": timeouts,
        }

    def version_summary(self, version: str) -> dict:
        with self._lock:
            samples = self._by_version.get(version)
            if samples is None:
                return self._summary(deque())
            self._evict(samples, time.time())
            return self._summary(deque(samples))

    def tenant_summary(self, tenant_id: str) -> dict:
        with self._lock:
            samples = self._by_tenant.get(tenant_id)
            if samples is None:
                return self._summary(deque())
            self._evict(samples, time.time())
            return self._summary(deque(samples))

    def all_versions(self) -> list[str]:
        with self._lock:
            return sorted(self._by_version)

    def all_tenants(self) -> list[str]:
        with self._lock:
            return sorted(self._by_tenant)

    def reset(self) -> None:
        """Drop every retained sample (used when starting a new experiment)."""

        with self._lock:
            self._by_version.clear()
            self._by_tenant.clear()


class CircuitBreaker:
    """Trips the canary weight to zero when P99 or error rate leaves the SLO."""

    def __init__(
        self,
        canary_version: str,
        p99_threshold_ms: float,
        error_rate_threshold: float,
        min_samples: int,
    ):
        self._canary = canary_version
        self._p99_threshold = p99_threshold_ms
        self._error_threshold = error_rate_threshold
        self._min_samples = min_samples
        self._lock = threading.Lock()
        self._tripped = False
        self._last_record: dict | None = None

    @property
    def tripped(self) -> bool:
        with self._lock:
            return self._tripped

    def evaluate(self, summary: dict) -> dict | None:
        with self._lock:
            if self._tripped or summary["count"] < self._min_samples:
                return None
            reasons = []
            if summary["p99_ms"] > self._p99_threshold:
                reasons.append(
                    f"p99_ms {summary['p99_ms']} > threshold {self._p99_threshold}"
                )
            if summary["error_rate"] > self._error_threshold:
                reasons.append(
                    f"error_rate {summary['error_rate']:.3f} > threshold "
                    f"{self._error_threshold:.3f}"
                )
            if not reasons:
                return None
            self._tripped = True
            self._last_record = {
                "canary_version": self._canary,
                "reasons": reasons,
                "sample_count": summary["count"],
                "trigger_p99_ms": summary["p99_ms"],
                "trigger_error_rate": summary["error_rate"],
                "tripped_at": time.time(),
                "window_timeout_count": summary["timeout_count"],
            }
            return dict(self._last_record)

    def reset(self) -> None:
        with self._lock:
            self._tripped = False

    def record(self) -> dict | None:
        with self._lock:
            return dict(self._last_record) if self._last_record else None
