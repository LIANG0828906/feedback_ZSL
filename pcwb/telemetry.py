"""Bounded latency telemetry and deterministic trajectory logging."""

from __future__ import annotations

import json
import threading
import time
from collections import deque
from dataclasses import dataclass

from .versions import canonical_json


@dataclass
class FrameSample:
    frame: int
    timestamp: float
    frame_ms: float
    input_latency_ms: float


class LatencyMonitor:
    """Ring-buffer sampler for frame time and input-response latency.

    Keeps at most ``capacity`` samples so memory stays bounded during long
    interactive sessions. A pluggable clock makes scripted runs fully
    deterministic (real ``time.perf_counter`` is used by default).
    """

    def __init__(self, capacity: int = 4096, clock=time.perf_counter):
        self.capacity = int(capacity)
        self._clock = clock
        self._samples: deque[FrameSample] = deque(maxlen=self.capacity)
        self._lock = threading.Lock()
        self._frame = 0
        self._pending_input_at: float | None = None

    def record_input(self) -> None:
        with self._lock:
            self._pending_input_at = self._clock()

    def record_frame(self, frame_ms: float) -> FrameSample:
        with self._lock:
            self._frame += 1
            now = self._clock()
            latency = (0.0 if self._pending_input_at is None
                       else (now - self._pending_input_at) * 1000.0)
            self._pending_input_at = None
            sample = FrameSample(
                frame=self._frame, timestamp=now,
                frame_ms=float(frame_ms), input_latency_ms=float(latency))
            self._samples.append(sample)
            return sample

    def samples(self) -> list[FrameSample]:
        with self._lock:
            return list(self._samples)

    def stats(self) -> dict:
        with self._lock:
            if not self._samples:
                return {"frames": 0, "frame_ms_avg": 0.0,
                        "frame_ms_max": 0.0, "input_latency_ms_max": 0.0,
                        "frame_ms_p95": 0.0}
            frames = [s.frame_ms for s in self._samples]
            latencies = [s.input_latency_ms for s in self._samples
                         if s.input_latency_ms > 0.0]
            frames_sorted = sorted(frames)
            p95_idx = min(len(frames_sorted) - 1,
                          int(round(0.95 * (len(frames_sorted) - 1))))
            return {
                "frames": len(frames),
                "frame_ms_avg": round(float(sum(frames) / len(frames)), 6),
                "frame_ms_max": round(float(max(frames)), 6),
                "frame_ms_p95": round(float(frames_sorted[p95_idx]), 6),
                "input_latency_ms_max": round(
                    float(max(latencies)) if latencies else 0.0, 6),
            }


class TrajectoryLog:
    """Append-only, byte-deterministic trajectory log.

    Every line is a standalone canonical JSON object (JSONL). When a
    deterministic clock is supplied, identical seeds/operation sequences
    produce byte-identical log files across machines and runs.
    """

    def __init__(self, path: str):
        self.path = path
        self._lock = threading.Lock()
        with open(self.path, "wb"):
            pass

    def emit(self, event: dict) -> None:
        line = canonical_json(event)
        with self._lock:
            with open(self.path, "ab") as handle:
                handle.write(line)

    def read(self) -> list[dict]:
        with open(self.path, "rb") as handle:
            return [json.loads(line) for line in handle if line.strip()]
