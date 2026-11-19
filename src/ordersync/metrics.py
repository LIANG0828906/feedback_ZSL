from __future__ import annotations

import json
import math
import threading
from collections import Counter
from typing import Any, Iterable

from .replication import MonitorSnapshot, ReplicaMonitor


CONSISTENCY_LEVELS = ("strong", "session", "eventual")
MAX_INCONSISTENCY_SAMPLES = 200


def percentile(values: Iterable[int | float], percent: float) -> int | None:
    ordered = sorted(float(value) for value in values)
    if not ordered:
        return None
    if percent <= 0:
        return int(round(ordered[0]))
    if percent >= 100:
        return int(round(ordered[-1]))
    index = max(math.ceil((percent / 100.0) * len(ordered)) - 1, 0)
    return int(round(ordered[index]))


def _latency_summary(values: Iterable[int | float]) -> dict[str, Any]:
    frozen = list(values)
    return {
        "count": len(frozen),
        "p50_ms": percentile(frozen, 50),
        "p95_ms": percentile(frozen, 95),
        "p99_ms": percentile(frozen, 99),
        "max_ms": percentile(frozen, 100),
    }


class MetricsCollector:
    def __init__(self, monitor: ReplicaMonitor) -> None:
        self._monitor = monitor
        self._lock = threading.RLock()
        self.reset()

    def reset(self) -> None:
        with self._lock:
            self._reads = {
                level: {
                    "requests": 0,
                    "successes": 0,
                    "consistency_errors": 0,
                    "degraded_primary_reads": 0,
                    "read_errors": 0,
                }
                for level in CONSISTENCY_LEVELS
            }
            self._invalid_consistency_requests = 0
            self._read_latencies = {level: [] for level in CONSISTENCY_LEVELS}
            self._all_read_latencies: list[int] = []
            self._write_requests = 0
            self._write_successes = 0
            self._write_failures = 0
            self._write_failures_by_code: Counter[str] = Counter()
            self._write_latencies: list[int] = []
            self._switch_successes = 0
            self._switch_aborts = 0
            self._switch_durations_ms: list[int] = []
            self._inconsistency_observations = 0
            self._inconsistency_samples: dict[str, dict[str, Any]] = {}
            self._monitor.reset_statistics()

    def record_read(
        self,
        level: str,
        latency_ms: int,
        *,
        success: bool,
        consistency_error: bool = False,
        degraded_primary: bool = False,
        read_error: bool = False,
    ) -> None:
        with self._lock:
            latency = max(int(latency_ms), 0)
            self._all_read_latencies.append(latency)
            if level in self._reads:
                self._read_latencies[level].append(latency)
                bucket = self._reads[level]
                bucket["requests"] += 1
                if success:
                    bucket["successes"] += 1
                if consistency_error:
                    bucket["consistency_errors"] += 1
                if degraded_primary:
                    bucket["degraded_primary_reads"] += 1
                if read_error:
                    bucket["read_errors"] += 1
            else:
                self._invalid_consistency_requests += 1

    def record_invalid_consistency(self, latency_ms: int) -> None:
        with self._lock:
            self._invalid_consistency_requests += 1
            latency = max(int(latency_ms), 0)
            self._all_read_latencies.append(latency)

    def record_write(self, latency_ms: int, *, success: bool, code: str | None = None) -> None:
        with self._lock:
            self._write_requests += 1
            self._write_latencies.append(max(int(latency_ms), 0))
            if success:
                self._write_successes += 1
            else:
                self._write_failures += 1
                self._write_failures_by_code[code or "E_WRITE_UNKNOWN"] += 1

    def record_switch(self, duration_ms: int, *, success: bool) -> None:
        with self._lock:
            if success:
                self._switch_successes += 1
                self._switch_durations_ms.append(max(int(duration_ms), 0))
            else:
                self._switch_aborts += 1

    def record_inconsistency(
        self,
        order_no: str,
        differences: list[dict[str, Any]],
        *,
        classification: str,
        primary_watermark: int | None,
        replica_watermark: int | None,
    ) -> None:
        if not differences:
            return
        with self._lock:
            self._inconsistency_observations += 1
            sample = {
                "order_no": order_no,
                "classification": classification,
                "primary_watermark": (
                    str(primary_watermark)
                    if primary_watermark is not None
                    else None
                ),
                "replica_applied_watermark": (
                    str(replica_watermark)
                    if replica_watermark is not None
                    else None
                ),
                "differences": sorted(
                    differences, key=lambda item: str(item.get("field", ""))
                ),
            }
            existing = self._inconsistency_samples.get(order_no)
            if existing is None:
                self._inconsistency_samples[order_no] = sample
            else:
                old_key = json.dumps(
                    existing["differences"], sort_keys=True, separators=(",", ":")
                )
                new_key = json.dumps(
                    sample["differences"], sort_keys=True, separators=(",", ":")
                )
                if new_key < old_key:
                    self._inconsistency_samples[order_no] = sample

    def snapshot(self) -> dict[str, Any]:
        with self._lock:
            monitor_snapshot = self._monitor.snapshot()
            observations = self._monitor.observation_report()
            gaps = observations["watermark_gap_samples"]
            visibility_latencies = observations[
                "visibility_latency_samples_ms"
            ]
            reads = {
                level: dict(values) for level, values in self._reads.items()
            }
            samples = [
                self._inconsistency_samples[key]
                for key in sorted(self._inconsistency_samples)
            ][:MAX_INCONSISTENCY_SAMPLES]
            switch_durations = sorted(self._switch_durations_ms)
            consistency_errors_total = (
                sum(bucket["consistency_errors"] for bucket in reads.values())
                + self._invalid_consistency_requests
            )
            degraded_primary_reads_total = sum(
                bucket["degraded_primary_reads"] for bucket in reads.values()
            )
            return {
                "schema_version": 1,
                "reads": reads,
                "consistency_errors_total": consistency_errors_total,
                "degraded_primary_reads_total": degraded_primary_reads_total,
                "invalid_consistency_requests": (
                    self._invalid_consistency_requests
                ),
                "read_latency_ms": {
                    "all": _latency_summary(self._all_read_latencies),
                    "by_consistency": {
                        level: _latency_summary(self._read_latencies[level])
                        for level in CONSISTENCY_LEVELS
                    },
                },
                "writes": {
                    "requests": self._write_requests,
                    "successes": self._write_successes,
                    "failures": self._write_failures,
                    "failures_by_code": dict(
                        sorted(self._write_failures_by_code.items())
                    ),
                    "latency_ms": _latency_summary(self._write_latencies),
                },
                "switches": {
                    "succeeded": self._switch_successes,
                    "aborted": self._switch_aborts,
                    "durations_ms": switch_durations,
                    "total_duration_ms": sum(switch_durations),
                    "max_duration_ms": (
                        switch_durations[-1] if switch_durations else None
                    ),
                },
                "replica_lag": {
                    "current": monitor_snapshot.as_dict(),
                    "watermark_gap": _latency_summary(gaps),
                    "visibility_latency_ms": _latency_summary(
                        visibility_latencies
                    ),
                },
                "inconsistencies": {
                    "observations": self._inconsistency_observations,
                    "distinct_orders": len(self._inconsistency_samples),
                    "samples": samples,
                },
            }

    def current_snapshot(self) -> MonitorSnapshot:
        return self._monitor.snapshot()

