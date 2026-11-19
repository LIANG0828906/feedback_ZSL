from __future__ import annotations

from typing import Any

from .errors import EngineError


class MetricsServer:
    """Optional Prometheus endpoint for a running job.

    The SQLite tables remain the source of truth; these gauges are a thin
    operational view for dashboards and health checks.
    """

    def __init__(self, job_id: str, mode: str, port: int) -> None:
        try:
            from prometheus_client import CollectorRegistry, Gauge, start_http_server
        except Exception as exc:  # pragma: no cover
            raise EngineError(
                "IOV_DEPENDENCY_MISSING",
                f"prometheus-client is required for --metrics-port: {exc}",
            ) from exc
        self.registry = CollectorRegistry()
        self.labels = {"job": job_id, "mode": mode}
        self.stage_commit = Gauge(
            "iovflow_stage_committed_batch",
            "Last committed batch for a stage",
            ["job", "mode", "stage"],
            registry=self.registry,
        )
        self.takeovers = Gauge(
            "iovflow_stage_takeovers",
            "Cumulative stage shard takeovers",
            ["job", "mode"],
            registry=self.registry,
        )
        self.completed = Gauge(
            "iovflow_completed_windows",
            "Completed event-time windows",
            ["job", "mode"],
            registry=self.registry,
        )
        self.late = Gauge(
            "iovflow_late_events",
            "Events arriving after their window closed",
            ["job", "mode"],
            registry=self.registry,
        )
        self.spill = Gauge(
            "iovflow_spill_bytes",
            "Window state bytes spilled to local storage",
            ["job", "mode"],
            registry=self.registry,
        )
        self.peak = Gauge(
            "iovflow_peak_memory_mb",
            "Peak sampled process-tree RSS",
            ["job", "mode"],
            registry=self.registry,
        )
        self.p95 = Gauge(
            "iovflow_batch_p95_seconds",
            "P95 batch latency",
            ["job", "mode"],
            registry=self.registry,
        )
        self.job_status = Gauge(
            "iovflow_job_status",
            "0=running, 1=complete, 2=failed",
            ["job", "mode"],
            registry=self.registry,
        )
        start_http_server(int(port), registry=self.registry)
        self.job_status.labels(**self.labels).set(0)

    def update(self, status: str, report: dict[str, Any] | None = None,
               stages: list[dict[str, Any]] | None = None) -> None:
        self.job_status.labels(**self.labels).set(
            1 if status == "complete" else (2 if status == "failed" else 0)
        )
        if stages:
            for stage in stages:
                self.stage_commit.labels(stage=stage["stage_id"], **self.labels).set(
                    int(stage.get("committed_batch") or -1)
                )
        if not report:
            return
        self.takeovers.labels(**self.labels).set(
            int(report.get("fault_recovery", {}).get("takeover_count", 0))
        )
        self.completed.labels(**self.labels).set(
            int(report.get("results", {}).get("completed_windows", 0))
        )
        self.late.labels(**self.labels).set(
            int(report.get("results", {}).get("late_events", 0))
        )
        self.spill.labels(**self.labels).set(
            int(report.get("resources", {}).get("spill_bytes", 0))
        )
        self.peak.labels(**self.labels).set(
            float(report.get("resources", {}).get("peak_memory_mb", 0))
        )
        self.p95.labels(**self.labels).set(
            float(report.get("performance", {}).get("batch_p95_seconds", 0))
        )

