from __future__ import annotations

import math
import threading
import time
from collections import deque
from dataclasses import asdict, dataclass

from .config import SLIConfig


@dataclass(frozen=True)
class QuerySample:
    observed_at: float
    success: bool
    latency_ms: float


@dataclass(frozen=True)
class SLISnapshot:
    name: str
    kind: str
    window_seconds: float
    target: float | None
    target_ms: float | None
    min_samples: int
    samples: int
    successes: int
    availability: float | None
    p99_latency_ms: float | None
    bad_ratio: float
    error_budget_consumed: float
    error_budget_remaining: float
    breached: bool
    recovered: bool

    @property
    def value(self) -> float | None:
        if self.kind == "availability":
            return self.availability
        return self.p99_latency_ms

    def to_dict(self) -> dict[str, object]:
        data = asdict(self)
        data["value"] = self.value
        return data


class _Window:
    def __init__(self, config: SLIConfig) -> None:
        self.config = config
        self.samples: deque[QuerySample] = deque()
        self.lock = threading.RLock()

    def record(self, sample: QuerySample) -> None:
        with self.lock:
            self.samples.append(sample)
            self._purge(sample.observed_at)

    def _purge(self, now: float) -> None:
        cutoff = now - self.config.window_seconds
        while self.samples and self.samples[0].observed_at < cutoff:
            self.samples.popleft()

    def snapshot(self, now: float | None = None) -> SLISnapshot:
        current = time.monotonic() if now is None else now
        with self.lock:
            self._purge(current)
            samples = tuple(self.samples)
        total = len(samples)
        successes = sum(1 for sample in samples if sample.success)
        availability = successes / total if total else None
        latencies = sorted(sample.latency_ms for sample in samples)
        p99_ms = None
        if latencies:
            rank = max(0, math.ceil(0.99 * len(latencies)) - 1)
            p99_ms = latencies[rank]

        if self.config.kind == "availability":
            allowed_bad_ratio = 1.0 - float(self.config.target)
            bad_ratio = (total - successes) / total if total else 0.0
        else:
            assert self.config.target_ms is not None
            threshold = self.config.target_ms
            allowed_bad_ratio = self.config.allowed_bad_ratio
            bad_ratio = (
                sum(1 for sample in samples if sample.latency_ms > threshold) / total
                if total
                else 0.0
            )

        consumed = bad_ratio / allowed_bad_ratio if allowed_bad_ratio > 0 else 0.0
        consumed = max(0.0, consumed)
        remaining = max(0.0, 1.0 - consumed)
        has_enough_samples = total >= self.config.min_samples
        latency_breached = self.config.kind == "p99_latency" and (
            self.config.target_ms is None
            or p99_ms is None
            or p99_ms > self.config.target_ms
        )
        breached = has_enough_samples and consumed >= 1.0 and (
            self.config.kind == "availability" or latency_breached
        )
        if total == 0:
            recovered = True
        else:
            recovered = has_enough_samples and consumed <= self.config.recover_budget_ratio and (
                self.config.kind == "availability"
                or (
                    self.config.target_ms is not None
                    and p99_ms is not None
                    and p99_ms <= self.config.target_ms
                )
            )
        return SLISnapshot(
            name=self.config.name,
            kind=self.config.kind,
            window_seconds=self.config.window_seconds,
            target=self.config.target,
            target_ms=self.config.target_ms,
            min_samples=self.config.min_samples,
            samples=total,
            successes=successes,
            availability=availability,
            p99_latency_ms=p99_ms,
            bad_ratio=bad_ratio,
            error_budget_consumed=consumed,
            error_budget_remaining=remaining,
            breached=breached,
            recovered=recovered,
        )


class SLIRegistry:
    def __init__(self, configs: dict[str, SLIConfig]) -> None:
        self._windows = {name: _Window(config) for name, config in configs.items()}

    def record_query(
        self,
        *,
        success: bool,
        latency_ms: float,
        observed_at: float | None = None,
    ) -> None:
        sample = QuerySample(
            observed_at=time.monotonic() if observed_at is None else observed_at,
            success=success,
            latency_ms=max(0.0, latency_ms),
        )
        for window in self._windows.values():
            window.record(sample)

    def snapshot(self, name: str, now: float | None = None) -> SLISnapshot:
        return self._windows[name].snapshot(now)

    def snapshots(self, now: float | None = None) -> dict[str, SLISnapshot]:
        current = time.monotonic() if now is None else now
        return {
            name: window.snapshot(current)
            for name, window in self._windows.items()
        }

    def to_dict(self, now: float | None = None) -> dict[str, dict[str, object]]:
        return {
            name: snapshot.to_dict()
            for name, snapshot in self.snapshots(now).items()
        }
