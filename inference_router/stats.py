from __future__ import annotations

import threading
from collections import deque
from typing import Any

from .health import percentile


class RequestStats:
    def __init__(self, window_size: int, p99_threshold_ms: float) -> None:
        self.window_size = window_size
        self.p99_threshold_ms = float(p99_threshold_ms)
        self._lock = threading.RLock()
        self._observations: deque[dict[str, Any]] = deque(maxlen=window_size)
        self._evictions: deque[dict[str, Any]] = deque(maxlen=1000)

    def record_request(self, observation: dict[str, Any]) -> None:
        with self._lock:
            self._observations.append(observation)

    def record_event(self, event: dict[str, Any]) -> None:
        if event.get("event_type") != "evict":
            return
        with self._lock:
            self._evictions.append(event)

    def snapshot(self) -> dict[str, Any]:
        with self._lock:
            observations = list(self._observations)
            evictions = list(self._evictions)
        e2e_values = [float(item["e2e_ms"]) for item in observations]
        p50 = percentile(e2e_values, 0.50)
        p95 = percentile(e2e_values, 0.95)
        p99 = percentile(e2e_values, 0.99)
        window_start = min(
            (int(item["created_at_ms"]) for item in observations), default=None
        )
        if window_start is None:
            window_evictions = evictions[-10:]
        else:
            window_evictions = [
                event for event in evictions if int(event.get("created_at_ms", 0)) >= window_start
            ]

        layer_stats: dict[str, dict[str, Any]] = {}
        replica_stats: dict[str, dict[str, Any]] = {}
        retry_switches = 0
        for observation in observations:
            is_tail_request = float(observation["e2e_ms"]) > self.p99_threshold_ms
            switch_layer = observation.get("switch_layer_id")
            switch_count = int(observation.get("retry_switches", 0))
            retry_switches += switch_count
            if switch_layer and switch_count:
                layer = layer_stats.setdefault(
                    switch_layer,
                    {
                        "calls": 0,
                        "timeouts": 0,
                        "errors": 0,
                        "retry_switches": 0,
                        "evictions": 0,
                        "total_latency_ms": 0.0,
                        "tail_latency_ms": 0.0,
                    },
                )
                layer["retry_switches"] += switch_count
            for call in observation.get("calls", []):
                layer_id = call["layer_id"]
                layer = layer_stats.setdefault(
                    layer_id,
                    {
                        "calls": 0,
                        "timeouts": 0,
                        "errors": 0,
                        "retry_switches": 0,
                        "evictions": 0,
                        "total_latency_ms": 0.0,
                        "tail_latency_ms": 0.0,
                    },
                )
                layer["calls"] += 1
                layer["total_latency_ms"] += float(call["latency_ms"])
                if is_tail_request:
                    layer["tail_latency_ms"] += float(call["latency_ms"])
                if call.get("timed_out"):
                    layer["timeouts"] += 1
                if call.get("outcome") != "success":
                    layer["errors"] += 1
                replica_id = call["replica_id"]
                replica = replica_stats.setdefault(
                    replica_id,
                    {
                        "layer_id": layer_id,
                        "replica_id": replica_id,
                        "calls": 0,
                        "timeouts": 0,
                        "errors": 0,
                        "evictions": 0,
                        "total_latency_ms": 0.0,
                        "tail_latency_ms": 0.0,
                    },
                )
                replica["calls"] += 1
                replica["total_latency_ms"] += float(call["latency_ms"])
                if is_tail_request:
                    replica["tail_latency_ms"] += float(call["latency_ms"])
                if call.get("timed_out"):
                    replica["timeouts"] += 1
                if call.get("outcome") != "success":
                    replica["errors"] += 1

        for event in window_evictions:
            layer_id = event.get("layer_id")
            replica_id = event.get("replica_id")
            if layer_id in layer_stats:
                layer_stats[layer_id]["evictions"] += 1
            if replica_id in replica_stats:
                replica_stats[replica_id]["evictions"] += 1

        culprit = None
        if p99 > self.p99_threshold_ms and replica_stats:
            worst = max(
                replica_stats.values(),
                key=lambda item: (
                    item["tail_latency_ms"],
                    item["total_latency_ms"],
                    item["calls"],
                    item["replica_id"],
                ),
            )
            culprit = {
                "layer_id": worst["layer_id"],
                "replica_id": worst["replica_id"],
                "contribution_ms": round(
                    worst["tail_latency_ms"] or worst["total_latency_ms"], 3
                ),
                "calls": worst["calls"],
                "timeouts": worst["timeouts"],
            }
        for item in layer_stats.values():
            item["total_latency_ms"] = round(item["total_latency_ms"], 3)
            item["tail_latency_ms"] = round(item["tail_latency_ms"], 3)
        for item in replica_stats.values():
            item["total_latency_ms"] = round(item["total_latency_ms"], 3)
            item["tail_latency_ms"] = round(item["tail_latency_ms"], 3)
        return {
            "window": {
                "request_count": len(observations),
                "started_at_ms": window_start,
                "size": self.window_size,
            },
            "e2e": {
                "p50_ms": round(p50, 3),
                "p95_ms": round(p95, 3),
                "p99_ms": round(p99, 3),
            },
            "p99_threshold_ms": self.p99_threshold_ms,
            "p99_exceeded": p99 > self.p99_threshold_ms,
            "culprit": culprit,
            "layers": {
                key: layer_stats[key]
                for key in sorted(layer_stats)
            },
            "replicas": {
                key: replica_stats[key]
                for key in sorted(replica_stats)
            },
            "retry_switches": retry_switches,
            "evictions": sorted(
                window_evictions,
                key=lambda item: (int(item.get("created_at_ms", 0)), item.get("replica_id", "")),
            ),
        }
