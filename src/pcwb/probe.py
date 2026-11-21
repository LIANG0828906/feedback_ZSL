"""A display-only loop used by headless verification and scripted demos."""

from __future__ import annotations

import threading
import time
from dataclasses import dataclass, field
from typing import Any

from .async_edit import AsyncEditController
from .metrics import InteractionMetrics


@dataclass(slots=True)
class ProbeReport:
    frames: int = 0
    hashes: set[str] = field(default_factory=set)
    old_snapshot_observed_while_running: bool = False
    max_frame_ms: float = 0.0
    metrics: dict[str, Any] | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "frames": self.frames,
            "distinct_display_hashes": len(self.hashes),
            "old_snapshot_observed_while_running": self.old_snapshot_observed_while_running,
            "max_frame_ms": round(self.max_frame_ms, 6),
            "metrics": self.metrics,
        }


class SnapshotProbeLoop:
    """Reads the controller's published snapshot exactly once per frame."""

    def __init__(
        self,
        controller: AsyncEditController,
        metrics: InteractionMetrics,
        *,
        sleep_seconds: float = 0.001,
    ) -> None:
        self.controller = controller
        self.metrics = metrics
        self.sleep_seconds = max(0.0, float(sleep_seconds))
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._lock = threading.Lock()
        self._report = ProbeReport()

    def start(self) -> None:
        self._stop.clear()
        self._thread = threading.Thread(
            target=self._run, name="pcwb-display-probe", daemon=True
        )
        self._thread.start()

    def _run(self) -> None:
        while not self._stop.is_set():
            started = time.perf_counter_ns()
            status = self.controller.status()
            snapshot = self.controller.display_snapshot()
            # Simulate input handling on the display loop.
            input_started = time.perf_counter_ns()
            _ = snapshot.version, snapshot.content_hash
            self.metrics.record_input_latency(
                (time.perf_counter_ns() - input_started) / 1_000_000.0
            )
            elapsed_ms = (time.perf_counter_ns() - started) / 1_000_000.0
            self.metrics.record_frame(elapsed_ms)
            with self._lock:
                self._report.frames += 1
                self._report.hashes.add(snapshot.content_hash)
                self._report.max_frame_ms = max(self._report.max_frame_ms, elapsed_ms)
                if (
                    status.state in {"running", "committing", "cancelling"}
                    and snapshot.content_hash == status.base_hash
                ):
                    self._report.old_snapshot_observed_while_running = True
            if self.sleep_seconds:
                time.sleep(self.sleep_seconds)

    def stop(self) -> ProbeReport:
        self._stop.set()
        if self._thread is not None:
            self._thread.join()
            self._thread = None
        with self._lock:
            report = ProbeReport(
                frames=self._report.frames,
                hashes=set(self._report.hashes),
                old_snapshot_observed_while_running=self._report.old_snapshot_observed_while_running,
                max_frame_ms=self._report.max_frame_ms,
                metrics=self.metrics.summary(),
            )
            self._report = ProbeReport()
        return report
