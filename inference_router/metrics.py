from __future__ import annotations

import threading
from typing import Any

from prometheus_client import CollectorRegistry, generate_latest
from prometheus_client.core import CounterMetricFamily, GaugeMetricFamily

from .health import HealthManager, percentile
from .stats import RequestStats


class RuntimeCollector:
    def __init__(
        self,
        health: HealthManager,
        stats: RequestStats,
        layer_order: list[str],
    ) -> None:
        self.health = health
        self.stats = stats
        self.layer_order = layer_order
        self._lock = threading.Lock()
        self.request_counts = {"success": 0, "error": 0}
        self.retry_switches_total = 0

    def record_request(self, observation: dict[str, Any]) -> None:
        with self._lock:
            key = "success" if observation.get("ok") else "error"
            self.request_counts[key] += 1
            self.retry_switches_total += int(observation.get("retry_switches", 0))

    def collect(self):
        available = GaugeMetricFamily(
            "inference_layer_available_replicas",
            "Number of replicas currently eligible for routing, by layer.",
            labels=["layer"],
        )
        for layer_id in self.layer_order:
            available.add_metric([layer_id], self.health.available_count(layer_id))
        yield available

        calls = CounterMetricFamily(
            "inference_replica_calls",
            "Actual inference attempts sent to each replica.",
            labels=["layer", "replica"],
        )
        timeouts = CounterMetricFamily(
            "inference_replica_timeouts",
            "Timed out inference attempts by replica.",
            labels=["layer", "replica"],
        )
        errors = CounterMetricFamily(
            "inference_replica_errors",
            "Failed inference attempts by replica.",
            labels=["layer", "replica"],
        )
        evictions = CounterMetricFamily(
            "inference_replica_evictions",
            "Number of times a replica was removed from routing.",
            labels=["layer", "replica"],
        )
        latency = GaugeMetricFamily(
            "inference_replica_latency_seconds",
            "Sliding-window call latency quantiles by replica.",
            labels=["layer", "replica", "quantile"],
        )
        for replica_id, state in sorted(self.health.states.items()):
            labels = [state.spec.layer, replica_id]
            calls.add_metric(labels, state.call_count)
            timeouts.add_metric(labels, state.timeout_count)
            errors.add_metric(labels, state.error_count)
            evictions.add_metric(labels, state.eviction_count)
            values = list(state.latencies_ms)
            for quantile in ("0.5", "0.95", "0.99"):
                latency.add_metric(
                    [state.spec.layer, replica_id, quantile],
                    percentile(values, float(quantile)) / 1000.0,
                )
        yield calls
        yield timeouts
        yield errors
        yield evictions
        yield latency

        snapshot = self.stats.snapshot()
        e2e = GaugeMetricFamily(
            "inference_e2e_latency_seconds",
            "End-to-end latency quantiles over the current request window.",
            labels=["quantile"],
        )
        e2e.add_metric(["0.5"], snapshot["e2e"]["p50_ms"] / 1000.0)
        e2e.add_metric(["0.95"], snapshot["e2e"]["p95_ms"] / 1000.0)
        e2e.add_metric(["0.99"], snapshot["e2e"]["p99_ms"] / 1000.0)
        yield e2e
        p99 = GaugeMetricFamily(
            "inference_e2e_p99_seconds",
            "End-to-end P99 latency over the current request window.",
        )
        p99.add_metric([], snapshot["e2e"]["p99_ms"] / 1000.0)
        yield p99

        requests = CounterMetricFamily(
            "inference_requests",
            "Completed router requests by outcome.",
            labels=["outcome"],
        )
        with self._lock:
            request_counts = dict(self.request_counts)
            retry_total = self.retry_switches_total
        for outcome in ("success", "error"):
            requests.add_metric([outcome], request_counts[outcome])
        yield requests
        retries = CounterMetricFamily(
            "inference_retry_switches",
            "Second-tier attempts switched to another replica.",
        )
        retries.add_metric([], retry_total)
        yield retries


class Metrics:
    def __init__(
        self,
        health: HealthManager,
        stats: RequestStats,
        layer_order: list[str],
    ) -> None:
        self.registry = CollectorRegistry(auto_describe=False)
        self.collector = RuntimeCollector(health, stats, layer_order)
        self.registry.register(self.collector)

    def record_request(self, observation: dict[str, Any]) -> None:
        self.collector.record_request(observation)

    def render(self) -> bytes:
        return generate_latest(self.registry)
