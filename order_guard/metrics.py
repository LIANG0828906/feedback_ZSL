from __future__ import annotations

import math
import threading
from collections import defaultdict
from typing import Iterable


def _escape(value: str) -> str:
    return value.replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"')


def _labels(labels: dict[str, str] | None) -> str:
    if not labels:
        return ""
    body = ",".join(f'{key}="{_escape(str(value))}"' for key, value in sorted(labels.items()))
    return "{" + body + "}"


def _number(value: float) -> str:
    if math.isnan(value):
        return "NaN"
    if math.isinf(value):
        return "+Inf" if value > 0 else "-Inf"
    if float(value).is_integer():
        return str(int(value))
    return f"{value:.6g}"


class Metrics:
    """Small thread-safe registry with a stable Prometheus text rendering."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._counters: dict[tuple[str, tuple[tuple[str, str], ...]], float] = defaultdict(float)
        self._gauges: dict[tuple[str, tuple[tuple[str, str], ...]], float] = {}
        self._types: dict[str, str] = {}
        self._help: dict[str, str] = {}

    @staticmethod
    def _key(name: str, labels: dict[str, str] | None) -> tuple[str, tuple[tuple[str, str], ...]]:
        return name, tuple(sorted((str(key), str(value)) for key, value in (labels or {}).items()))

    def declare(self, name: str, kind: str, help_text: str) -> None:
        with self._lock:
            self._types[name] = kind
            self._help[name] = help_text

    def inc(
        self, name: str, labels: dict[str, str] | None = None, amount: float = 1.0
    ) -> None:
        with self._lock:
            self._types.setdefault(name, "counter")
            key = self._key(name, labels)
            self._counters[key] += amount

    def set(self, name: str, value: float, labels: dict[str, str] | None = None) -> None:
        with self._lock:
            self._types.setdefault(name, "gauge")
            self._gauges[self._key(name, labels)] = float(value)

    def get_counter(
        self, name: str, labels: dict[str, str] | None = None
    ) -> float:
        with self._lock:
            return self._counters.get(self._key(name, labels), 0.0)

    def get_gauge(self, name: str, labels: dict[str, str] | None = None) -> float | None:
        with self._lock:
            return self._gauges.get(self._key(name, labels))

    def counter_sum(self, name: str) -> float:
        with self._lock:
            return sum(value for (metric, _), value in self._counters.items() if metric == name)

    def counter_series(self, name: str) -> list[tuple[dict[str, str], float]]:
        with self._lock:
            output: list[tuple[dict[str, str], float]] = []
            for (metric, labels), value in sorted(self._counters.items()):
                if metric == name:
                    output.append((dict(labels), value))
            return output

    def snapshot(self) -> dict[str, float]:
        with self._lock:
            return {
                name + _labels(dict(labels)): value
                for (name, labels), value in sorted(
                    {**self._counters, **self._gauges}.items()
                )
            }

    def render(self) -> str:
        with self._lock:
            counters = dict(self._counters)
            gauges = dict(self._gauges)
            types = dict(self._types)
            helps = dict(self._help)
        names = sorted(set(counters) | set(gauges), key=lambda item: item[0])
        lines: list[str] = []
        emitted_type: set[str] = set()
        for name, labels in names:
            if name not in emitted_type:
                if name in helps:
                    lines.append(f"# HELP {name} {helps[name]}")
                lines.append(f"# TYPE {name} {types.get(name, 'gauge')}")
                emitted_type.add(name)
            label_text = _labels(dict(labels))
            if (name, labels) in counters:
                value = counters[(name, labels)]
                lines.append(f"{name}{label_text} {_number(value)}")
            else:
                value = gauges[(name, labels)]
                lines.append(f"{name}{label_text} {_number(value)}")
        return "\n".join(lines) + ("\n" if lines else "")

    def declare_defaults(self, tenants: Iterable[str]) -> None:
        declarations = {
            "order_guard_rate_limit_rejections_total": (
                "counter",
                "Business requests rejected by the per-tenant admission gate.",
            ),
            "order_guard_inflight_requests": (
                "gauge",
                "Accepted business requests currently in flight.",
            ),
            "order_guard_inflight_total": (
                "gauge",
                "Total accepted business requests currently in flight.",
            ),
            "order_guard_inflight_peak": (
                "gauge",
                "Peak accepted business requests in flight for this run.",
            ),
            "order_guard_backlog_depth": (
                "gauge",
                "Accepted business requests waiting behind database capacity.",
            ),
            "order_guard_backlog_peak": (
                "gauge",
                "Peak accepted business requests waiting for database capacity.",
            ),
            "order_guard_worker_thread_occupancy_ratio": (
                "gauge",
                "Estimated business worker occupancy ratio.",
            ),
            "order_guard_circuit_state": (
                "gauge",
                "Orders database circuit state: 0 closed, 1 half-open, 2 open.",
            ),
            "order_guard_degrade_level": (
                "gauge",
                "Current effective degradation level.",
            ),
            "order_guard_protection_active": (
                "gauge",
                "Whether a named protection mode is active.",
            ),
            "order_guard_capacity_protected": (
                "gauge",
                "Whether capacity protection is active.",
            ),
            "order_guard_sqlite_busy_events_total": (
                "counter",
                "SQLite database-is-locked events observed by the service.",
            ),
            "order_guard_sqlite_operations_total": (
                "counter",
                "SQLite operations attempted by the service.",
            ),
            "order_guard_db_pool_timeout_total": (
                "counter",
                "Database operation attempts that timed out waiting for a connection slot.",
            ),
            "order_guard_sli_value": (
                "gauge",
                "Current SLI value from the sliding window.",
            ),
            "order_guard_error_budget_consumed_ratio": (
                "gauge",
                "Consumed share of the configured error budget.",
            ),
            "order_guard_error_budget_remaining_ratio": (
                "gauge",
                "Remaining share of the configured error budget.",
            ),
            "order_guard_error_budget_peak_consumed_ratio": (
                "gauge",
                "Peak consumed share of the configured error budget in this run.",
            ),
            "order_guard_sli_samples": (
                "gauge",
                "Samples currently present in an SLI sliding window.",
            ),
            "order_guard_sli_peak_value": (
                "gauge",
                "Maximum SLI value observed in a window during this run.",
            ),
            "order_guard_fallback_total": (
                "counter",
                "Query responses served by a fallback source.",
            ),
            "order_guard_circuit_transitions_total": (
                "counter",
                "Orders database circuit transitions.",
            ),
            "order_guard_capacity_transitions_total": (
                "counter",
                "Capacity protection transitions.",
            ),
            "order_guard_slo_breaches_total": (
                "counter",
                "SLI window evaluations that exhausted an error budget.",
            ),
            "order_guard_writes_rejected_total": (
                "counter",
                "Write requests rejected by protection modes.",
            ),
            "order_guard_query_page_size_limit": (
                "gauge",
                "Current effective query page size cap.",
            ),
            "order_guard_db_connection_limit": (
                "gauge",
                "Configured concurrent SQLite operation limit.",
            ),
            "order_guard_active_queries": (
                "gauge",
                "Queries currently executing against SQLite.",
            ),
            "order_guard_tenant_limits": (
                "gauge",
                "Current per-tenant admission limits.",
            ),
        }
        for name, (kind, help_text) in declarations.items():
            self.declare(name, kind, help_text)
        for tenant in tenants:
            self.set("order_guard_inflight_requests", 0, {"tenant": tenant})
            for reason in ("rate", "concurrency"):
                self.inc(
                    "order_guard_rate_limit_rejections_total",
                    {"tenant": tenant, "reason": reason},
                    0,
                )
        for name in (
            "order_guard_sqlite_busy_events_total",
            "order_guard_sqlite_operations_total",
            "order_guard_db_pool_timeout_total",
        ):
            self.inc(name, 0)
        for source in ("cache", "slim_snapshot", "none"):
            for reason in (
                "circuit_open",
                "half_open_probe_inflight",
                "query_operation_timeout",
                "database_timeout",
                "database_unavailable",
                "sqlite_busy",
                "database_error",
                "slo_cache_first",
            ):
                self.inc(
                    "order_guard_fallback_total",
                    {"source": source, "reason": reason},
                    0,
                )
        for target in ("closed", "half_open", "open", "reopened"):
            self.inc(
                "order_guard_circuit_transitions_total", {"to": target}, 0
            )
        for target in ("entered", "exited"):
            self.inc(
                "order_guard_capacity_transitions_total", {"to": target}, 0
            )
        for sli in ("availability", "latency"):
            self.inc("order_guard_slo_breaches_total", {"sli": sli}, 0)
        for reason in (
            "capacity_protected",
            "sqlite_busy",
            "database_timeout",
            "database_unavailable",
        ):
            self.inc(
                "order_guard_writes_rejected_total", {"reason": reason}, 0
            )
        self.set("order_guard_circuit_state", 0, {"circuit": "orders_db"})
        self.set("order_guard_degrade_level", 0)
        self.set("order_guard_capacity_protected", 0)
        for mode in ("capacity", "slo", "circuit"):
            self.set("order_guard_protection_active", 0, {"mode": mode})
        self.set("order_guard_query_page_size_limit", 0)
        self.set("order_guard_active_queries", 0)
        self.set("order_guard_backlog_depth", 0)
        self.set("order_guard_backlog_peak", 0)
        self.set("order_guard_worker_thread_occupancy_ratio", 0)
        self.set("order_guard_inflight_total", 0)
        self.set("order_guard_inflight_peak", 0)
