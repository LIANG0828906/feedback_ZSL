"""Prometheus metrics with an isolated registry per engine run."""

from __future__ import annotations

from pathlib import Path
from typing import Any

from prometheus_client import CollectorRegistry, Counter, Gauge, generate_latest, start_http_server

from .util import atomic_write_text


class BackfillMetrics:
    def __init__(self, *, slots: int, workers: int) -> None:
        self.registry = CollectorRegistry(auto_describe=True)
        self.events_total = Counter(
            "backfill_events_total",
            "Input event rows parsed",
            ["kind"],
            registry=self.registry,
        )
        self.batches_total = Counter(
            "backfill_batches_total",
            "Refund batches committed",
            registry=self.registry,
        )
        self.shards_total = Counter(
            "backfill_shards_total",
            "Payment history shards committed",
            registry=self.registry,
        )
        self.dedup_dropped_total = Counter(
            "backfill_dedup_dropped_total",
            "Events discarded by the canonical dedup rule",
            registry=self.registry,
        )
        self.late_total = Counter(
            "backfill_late_events_total",
            "Events arriving after their window closed",
            registry=self.registry,
        )
        self.windows_committed_total = Counter(
            "backfill_minute_windows_committed_total",
            "Minute windows committed exactly once",
            registry=self.registry,
        )
        self.spill_bytes_total = Counter(
            "backfill_spill_bytes_total",
            "Bytes written to local spill files",
            registry=self.registry,
        )
        self.peak_memory_bytes = Gauge(
            "backfill_peak_memory_bytes",
            "Observed peak RSS of the engine process tree",
            registry=self.registry,
        )
        self.open_windows = Gauge(
            "backfill_open_key_windows",
            "Uncommitted key-window states",
            registry=self.registry,
        )
        self.watermark = Gauge(
            "backfill_watermark_us",
            "Current event-time watermark in epoch microseconds",
            registry=self.registry,
        )
        self.logical_slots = Gauge(
            "backfill_logical_slots",
            "Configured logical processing slots",
            registry=self.registry,
        )
        self.worker_processes = Gauge(
            "backfill_worker_processes",
            "Aggregation worker processes (capped at 2)",
            registry=self.registry,
        )
        self.logical_slots.set(slots)
        self.worker_processes.set(workers)
        self._http_server: Any = None

    def observe_state(self, state: Any) -> None:
        self.open_windows.set(state.open_key_windows_count())
        watermark = state.watermark()
        if watermark is not None:
            self.watermark.set(watermark)

    def start_http(self, port: int) -> None:
        self._http_server = start_http_server(port, registry=self.registry)

    def render(self) -> str:
        return generate_latest(self.registry).decode("utf-8")

    def write(self, path: Path) -> None:
        atomic_write_text(path, self.render())
