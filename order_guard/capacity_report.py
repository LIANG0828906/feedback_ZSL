from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

from .config import Config
from .metrics import Metrics


@dataclass(frozen=True)
class CapacityReport:
    run_id: str
    scenario: str | None
    current_connection_limit: int
    current_page_size_limit: int
    suggested_connection_limit: int
    suggested_page_size_limit: int
    formula_version: str
    inputs: dict[str, Any]
    derivation: dict[str, Any]

    def to_dict(self) -> dict[str, Any]:
        return {
            "run_id": self.run_id,
            "scenario": self.scenario,
            "formula_version": self.formula_version,
            "current": {
                "connection_limit": self.current_connection_limit,
                "page_size_limit": self.current_page_size_limit,
            },
            "suggested": {
                "connection_limit": self.suggested_connection_limit,
                "page_size_limit": self.suggested_page_size_limit,
            },
            "inputs": self.inputs,
            "derivation": self.derivation,
        }


def build_capacity_report(
    config: Config,
    metrics: Metrics,
    run_id: str,
    *,
    scenario: str | None = None,
) -> CapacityReport:
    latency_sli = config.slis["latency"]
    target_ms = float(latency_sli.target_ms or 1.0)
    current_p99 = metrics.get_gauge(
        "order_guard_sli_peak_value", {"sli": "latency"}
    )
    p99 = float(current_p99 or 0.0)
    peak_inflight = int(
        metrics.get_gauge("order_guard_inflight_peak") or 0
    )
    peak_backlog = int(metrics.get_gauge("order_guard_backlog_peak") or 0)
    busy_events = int(
        metrics.get_counter("order_guard_sqlite_busy_events_total")
    )
    sqlite_operations = int(
        metrics.get_counter("order_guard_sqlite_operations_total")
    )
    rate_rejections = int(
        metrics.counter_sum("order_guard_rate_limit_rejections_total")
    )
    current_connections = config.database.connection_limit
    current_page_limit = config.query.max_page_size

    latency_pressure = (
        min(1.0, target_ms / p99) if p99 > 0.0 else 1.0
    )
    busy_ratio = (
        min(1.0, busy_events / max(1, sqlite_operations))
        if sqlite_operations
        else 0.0
    )
    busy_penalty = max(0.5, 1.0 - busy_ratio)
    pressure = max(0.05, latency_pressure * busy_penalty)
    connection_limit = max(
        1, min(current_connections, math.floor(current_connections * pressure))
    )
    page_size_limit = max(
        10,
        min(current_page_limit, math.floor(current_page_limit * pressure)),
    )
    page_size_limit = min(page_size_limit, config.query.max_page_size)
    return CapacityReport(
        run_id=run_id,
        scenario=scenario,
        current_connection_limit=current_connections,
        current_page_size_limit=current_page_limit,
        suggested_connection_limit=connection_limit,
        suggested_page_size_limit=page_size_limit,
        formula_version="capacity-v1",
        inputs={
            "latency_target_ms": target_ms,
            "observed_peak_p99_latency_ms": p99,
            "peak_inflight": peak_inflight,
            "peak_backlog_depth": peak_backlog,
            "sqlite_busy_events_total": busy_events,
            "sqlite_operations_total": sqlite_operations,
            "sqlite_busy_ratio": round(busy_ratio, 6),
            "rate_limit_rejections_total": rate_rejections,
            "current_connection_limit": current_connections,
            "current_page_size_limit": current_page_limit,
        },
        derivation={
            "latency_pressure": round(latency_pressure, 6),
            "busy_penalty": round(busy_penalty, 6),
            "combined_pressure": round(pressure, 6),
            "connection_formula": "clamp(floor(current_connection_limit * combined_pressure), 1, current_connection_limit)",
            "page_size_formula": "clamp(floor(current_page_size_limit * combined_pressure), 10, current_page_size_limit)",
        },
    )
