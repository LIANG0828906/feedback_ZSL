"""Sampling for frame duration and input-response latency."""

from __future__ import annotations

import threading
import time
from collections import deque
from dataclasses import dataclass
from typing import Any


def _percentile(sorted_values: list[float], fraction: float) -> float:
    if not sorted_values:
        return 0.0
    position = (len(sorted_values) - 1) * fraction
    lower = int(position)
    upper = min(lower + 1, len(sorted_values) - 1)
    weight = position - lower
    return sorted_values[lower] * (1.0 - weight) + sorted_values[upper] * weight


@dataclass(frozen=True, slots=True)
class TimingStats:
    count: int
    average_ms: float
    p95_ms: float
    p99_ms: float
    maximum_ms: float
    budget_ms: float
    over_budget: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "count": self.count,
            "average_ms": round(self.average_ms, 6),
            "p95_ms": round(self.p95_ms, 6),
            "p99_ms": round(self.p99_ms, 6),
            "max_ms": round(self.maximum_ms, 6),
            "budget_ms": self.budget_ms,
            "over_budget": self.over_budget,
        }


class TimingSampler:
    def __init__(self, budget_ms: float = 16.667, max_samples: int = 10000) -> None:
        self.budget_ms = float(budget_ms)
        self._samples: deque[float] = deque(maxlen=max_samples)
        self._lock = threading.Lock()

    def record(self, duration_ms: float) -> None:
        value = max(0.0, float(duration_ms))
        with self._lock:
            self._samples.append(value)

    def record_span(self, start_ns: int, end_ns: int | None = None) -> float:
        end = time.perf_counter_ns() if end_ns is None else end_ns
        duration = (end - start_ns) / 1_000_000.0
        self.record(duration)
        return duration

    def stats(self) -> TimingStats:
        with self._lock:
            values = sorted(self._samples)
        if not values:
            return TimingStats(0, 0.0, 0.0, 0.0, 0.0, self.budget_ms, 0)
        return TimingStats(
            count=len(values),
            average_ms=sum(values) / len(values),
            p95_ms=_percentile(values, 0.95),
            p99_ms=_percentile(values, 0.99),
            maximum_ms=values[-1],
            budget_ms=self.budget_ms,
            over_budget=sum(value > self.budget_ms for value in values),
        )

    def values(self) -> list[float]:
        with self._lock:
            return list(self._samples)


class InteractionMetrics:
    def __init__(self, *, frame_budget_ms: float = 16.667) -> None:
        self.frames = TimingSampler(frame_budget_ms)
        self.input_latency = TimingSampler(frame_budget_ms)
        self._edit_job_ms: deque[float] = deque(maxlen=1000)
        self._lock = threading.Lock()

    def record_frame(self, duration_ms: float) -> None:
        self.frames.record(duration_ms)

    def record_input_latency(self, duration_ms: float) -> None:
        self.input_latency.record(duration_ms)

    def record_edit_job(self, duration_ms: float) -> None:
        with self._lock:
            self._edit_job_ms.append(max(0.0, float(duration_ms)))

    def edit_jobs(self) -> list[float]:
        with self._lock:
            return list(self._edit_job_ms)

    def summary(self) -> dict[str, Any]:
        return {
            "frames": self.frames.stats().to_dict(),
            "input_latency": self.input_latency.stats().to_dict(),
            "edit_jobs_ms": [round(value, 6) for value in self.edit_jobs()],
        }
