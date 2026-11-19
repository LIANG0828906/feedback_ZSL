from __future__ import annotations

import math
import threading
import time
from collections import Counter, defaultdict, deque
from dataclasses import dataclass
from typing import Deque, Iterable


def percentile(values: Iterable[float], quantile: float) -> float:
    ordered = sorted(values)
    if not ordered:
        return 0.0
    if quantile <= 0:
        return float(ordered[0])
    index = max(0, math.ceil(quantile * len(ordered)) - 1)
    return float(ordered[min(index, len(ordered) - 1)])


@dataclass(frozen=True)
class RequestSample:
    timestamp: float
    latency_ms: float
    error: bool
    timeout: bool


class MetricsCollector:
    def __init__(
        self,
        model_versions: Iterable[str],
        window_seconds: float,
        max_samples_per_stream: int = 200_000,
    ) -> None:
        self._model_versions = tuple(model_versions)
        self._window_seconds = window_seconds
        self._max_samples_per_stream = max_samples_per_stream
        self._lock = threading.RLock()
        self._requests: Counter[tuple[str, str]] = Counter()
        self._errors: Counter[tuple[str, str]] = Counter()
        self._timeouts: Counter[tuple[str, str]] = Counter()
        self._tenant_requests: Counter[str] = Counter()
        self._error_codes: Counter[tuple[str, str]] = Counter()
        self._windows: dict[tuple[str, str], Deque[RequestSample]] = defaultdict(deque)
        self._all_samples: dict[tuple[str, str], Deque[RequestSample]] = defaultdict(
            lambda: deque(maxlen=self._max_samples_per_stream)
        )
        self._rollbacks = 0
        self._rollback_active: set[str] = set()

    def record(
        self,
        version: str,
        tenant: str,
        latency_ms: float,
        *,
        error: bool,
        timeout: bool,
        error_code: str | None = None,
    ) -> None:
        now = time.time()
        key = (version, tenant)
        sample = RequestSample(
            timestamp=now,
            latency_ms=max(0.0, float(latency_ms)),
            error=bool(error),
            timeout=bool(timeout),
        )
        with self._lock:
            self._requests[key] += 1
            self._tenant_requests[tenant] += 1
            if error:
                self._errors[key] += 1
            if timeout:
                self._timeouts[key] += 1
            if error_code:
                self._error_codes[(version, error_code)] += 1
            self._windows[key].append(sample)
            self._all_samples[key].append(sample)
            self._prune_locked(key, now)

    def _prune_locked(self, key: tuple[str, str], now: float | None = None) -> None:
        reference = time.time() if now is None else now
        cutoff = reference - self._window_seconds
        window = self._windows[key]
        while window and window[0].timestamp < cutoff:
            window.popleft()

    def _window_locked(self, key: tuple[str, str]) -> list[RequestSample]:
        self._prune_locked(key)
        return list(self._windows.get(key, ()))

    def version_window(self, version: str) -> list[RequestSample]:
        with self._lock:
            samples: list[RequestSample] = []
            for (sample_version, _tenant), _ in self._windows.items():
                if sample_version == version:
                    samples.extend(self._window_locked((sample_version, _tenant)))
            return samples

    def tenant_window(self, version: str, tenant: str) -> list[RequestSample]:
        with self._lock:
            return self._window_locked((version, tenant))

    def streams_for_version(self, version: str) -> list[str]:
        with self._lock:
            return sorted(
                tenant for (sample_version, tenant) in self._requests if sample_version == version
            )

    @staticmethod
    def summarize(samples: Iterable[RequestSample]) -> dict[str, float | int]:
        rows = list(samples)
        count = len(rows)
        if not count:
            return {
                "requests": 0,
                "errors": 0,
                "timeouts": 0,
                "error_rate": 0.0,
                "timeout_rate": 0.0,
                "p50_ms": 0.0,
                "p95_ms": 0.0,
                "p99_ms": 0.0,
            }
        latencies = [row.latency_ms for row in rows]
        errors = sum(1 for row in rows if row.error)
        timeouts = sum(1 for row in rows if row.timeout)
        return {
            "requests": count,
            "errors": errors,
            "timeouts": timeouts,
            "error_rate": errors / count,
            "timeout_rate": timeouts / count,
            "p50_ms": round(percentile(latencies, 0.50), 3),
            "p95_ms": round(percentile(latencies, 0.95), 3),
            "p99_ms": round(percentile(latencies, 0.99), 3),
        }

    def window_summary(self, version: str) -> dict[str, float | int]:
        return self.summarize(self.version_window(version))

    def window_summary_for(self, version: str, tenant: str) -> dict[str, float | int]:
        return self.summarize(self.tenant_window(version, tenant))

    def version_tenant_summaries(self) -> dict[str, dict[str, dict[str, float | int]]]:
        result: dict[str, dict[str, dict[str, float | int]]] = {}
        with self._lock:
            keys = sorted(self._requests)
        for version, tenant in keys:
            result.setdefault(version, {})[tenant] = self.summarize(
                self.tenant_window(version, tenant)
            )
        return result

    def all_time_summary(self, version: str) -> dict[str, float | int]:
        with self._lock:
            samples = [
                sample
                for (sample_version, _tenant), rows in self._all_samples.items()
                if sample_version == version
                for sample in rows
            ]
        return self.summarize(samples)

    def all_time_tenant_summaries(
        self,
    ) -> dict[str, dict[str, dict[str, float | int]]]:
        result: dict[str, dict[str, dict[str, float | int]]] = {}
        with self._lock:
            rows = {
                key: list(samples)
                for key, samples in self._all_samples.items()
                if samples
            }
        for (version, tenant), samples in sorted(rows.items()):
            result.setdefault(version, {})[tenant] = self.summarize(samples)
        return result

    def counters(self) -> dict[str, dict[str, int]]:
        with self._lock:
            versions = {
                version: {"requests": 0, "errors": 0, "timeouts": 0}
                for version in self._model_versions
            }
            for (version, _tenant), count in self._requests.items():
                versions.setdefault(
                    version, {"requests": 0, "errors": 0, "timeouts": 0}
                )["requests"] += count
            for (version, _tenant), count in self._errors.items():
                versions[version]["errors"] += count
            for (version, _tenant), count in self._timeouts.items():
                versions[version]["timeouts"] += count
            return versions

    def tenants(self) -> dict[str, int]:
        with self._lock:
            return dict(sorted(self._tenant_requests.items()))

    def error_codes(self) -> dict[str, dict[str, int]]:
        with self._lock:
            result: dict[str, dict[str, int]] = {}
            for (version, code), count in sorted(self._error_codes.items()):
                result.setdefault(version, {})[code] = count
            return result

    def mark_rollback(self, version: str) -> None:
        with self._lock:
            self._rollbacks += 1
            self._rollback_active.add(version)

    def clear_rollback(self, version: str) -> None:
        with self._lock:
            self._rollback_active.discard(version)

    def rollback_counts(self) -> tuple[int, set[str]]:
        with self._lock:
            return self._rollbacks, set(self._rollback_active)

    def render_prometheus(
        self,
        weights: dict[str, float],
        ready: dict[str, bool],
        rollback_active: dict[str, bool],
    ) -> str:
        lines: list[str] = []
        with self._lock:
            request_items = sorted(self._requests.items())
            error_items = sorted(self._errors.items())
            timeout_items = sorted(self._timeouts.items())
            tenant_items = sorted(self._tenant_requests.items())

        def metric(
            name: str,
            metric_type: str,
            help_text: str,
            rows: Iterable[tuple[dict[str, str], float | int]],
        ) -> None:
            lines.append(f"# HELP {name} {help_text}")
            lines.append(f"# TYPE {name} {metric_type}")
            for labels, value in rows:
                rendered = f"{value:.6f}".rstrip("0").rstrip(".")
                if labels:
                    label_text = ",".join(
                        f'{key}="{_escape_label(str(item))}"'
                        for key, item in sorted(labels.items())
                    )
                    lines.append(f"{name}{{{label_text}}} {rendered}")
                else:
                    lines.append(f"{name} {rendered}")

        counters = self.counters()
        metric(
            "ranking_gateway_version_requests_total",
            "counter",
            "Requests attributed to a model version and tenant.",
            (
                ({"version": version, "tenant": tenant}, count)
                for (version, tenant), count in request_items
            ),
        )
        metric(
            "ranking_gateway_version_errors_total",
            "counter",
            "Failed requests attributed to a model version and tenant.",
            (
                ({"version": version, "tenant": tenant}, count)
                for (version, tenant), count in error_items
            ),
        )
        metric(
            "ranking_gateway_version_timeouts_total",
            "counter",
            "Timed-out requests attributed to a model version and tenant.",
            (
                ({"version": version, "tenant": tenant}, count)
                for (version, tenant), count in timeout_items
            ),
        )
        metric(
            "ranking_gateway_tenant_requests_total",
            "counter",
            "Requests grouped by tenant.",
            (({"tenant": tenant}, count) for tenant, count in tenant_items),
        )

        latency_rows: list[tuple[dict[str, str], float | int]] = []
        error_rate_rows: list[tuple[dict[str, str], float | int]] = []
        timeout_rate_rows: list[tuple[dict[str, str], float | int]] = []
        p99_rows: list[tuple[dict[str, str], float | int]] = []
        for version, tenant_summaries in self.version_tenant_summaries().items():
            for tenant, summary in tenant_summaries.items():
                labels = {"version": version, "tenant": tenant}
                for quantile_name, field in (
                    ("0.50", "p50_ms"),
                    ("0.95", "p95_ms"),
                    ("0.99", "p99_ms"),
                ):
                    latency_rows.append(
                        ({**labels, "quantile": quantile_name}, summary[field])
                    )
                error_rate_rows.append((labels, summary["error_rate"]))
                timeout_rate_rows.append((labels, summary["timeout_rate"]))
                p99_rows.append((labels, summary["p99_ms"]))

        metric(
            "ranking_gateway_version_latency_ms",
            "gauge",
            "Rolling-window end-to-end latency quantiles in milliseconds.",
            latency_rows,
        )
        metric(
            "ranking_gateway_version_latency_p99_ms",
            "gauge",
            "Rolling-window end-to-end P99 latency in milliseconds.",
            p99_rows,
        )
        metric(
            "ranking_gateway_version_error_rate",
            "gauge",
            "Rolling-window request error rate.",
            error_rate_rows,
        )
        metric(
            "ranking_gateway_version_timeout_rate",
            "gauge",
            "Rolling-window request timeout rate.",
            timeout_rate_rows,
        )
        metric(
            "ranking_gateway_version_weight",
            "gauge",
            "Current normalized routing weight by model version.",
            (({"version": version}, weight) for version, weight in sorted(weights.items())),
        )
        metric(
            "ranking_gateway_model_ready",
            "gauge",
            "Whether a model version can accept inference requests.",
            (({"version": version}, 1 if value else 0) for version, value in sorted(ready.items())),
        )
        metric(
            "ranking_gateway_rollback_active",
            "gauge",
            "Whether automatic rollback is active for a model version.",
            (
                ({"version": version}, 1 if value else 0)
                for version, value in sorted(rollback_active.items())
            ),
        )
        rollbacks, _active = self.rollback_counts()
        metric(
            "ranking_gateway_rollbacks_total",
            "counter",
            "Total automatic canary rollbacks.",
            [({}, rollbacks)],
        )

        metric(
            "ranking_gateway_version_request_summary",
            "gauge",
            "Per-version all-time request counters.",
            (
                ({"version": version, "kind": kind}, count)
                for version, counts in sorted(counters.items())
                for kind, count in sorted(counts.items())
            ),
        )
        return "\n".join(lines) + "\n"


def _escape_label(value: str) -> str:
    return value.replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"')
