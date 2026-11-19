"""Stable Prometheus metric names, labels and a separate metrics HTTP server."""

from __future__ import annotations

from prometheus_client import (
    CONTENT_TYPE_LATEST,
    CollectorRegistry,
    Counter,
    Gauge,
    Histogram,
    generate_latest,
    start_http_server,
)


class GatewayMetrics:
    """Metric names and labels are part of the stable API contract."""

    def __init__(self):
        self.registry = CollectorRegistry()
        self.requests = Counter(
            "inference_gateway_requests_total",
            "Total requests handled by the gateway",
            labelnames=("model_version", "tenant_id", "status"),
            registry=self.registry,
        )
        self.errors = Counter(
            "inference_gateway_errors_total",
            "Total business errors by code",
            labelnames=("model_version", "tenant_id", "code"),
            registry=self.registry,
        )
        self.timeouts = Counter(
            "inference_gateway_timeouts_total",
            "Total queue or readiness timeouts",
            labelnames=("model_version", "tenant_id", "kind"),
            registry=self.registry,
        )
        self.latency = Histogram(
            "inference_gateway_request_latency_seconds",
            "End-to-end request latency in seconds",
            labelnames=("model_version", "tenant_id"),
            buckets=(
                0.005, 0.01, 0.02, 0.035, 0.05, 0.075, 0.1, 0.15,
                0.2, 0.3, 0.5, 0.75, 1.0, 2.0,
            ),
            registry=self.registry,
        )
        self.queue_depth = Gauge(
            "inference_gateway_queue_depth",
            "Current number of requests waiting in the version queue",
            labelnames=("model_version",),
            registry=self.registry,
        )
        self.in_flight = Gauge(
            "inference_gateway_inflight_requests",
            "Currently executing inference batches slots in use",
            labelnames=("model_version",),
            registry=self.registry,
        )
        self.weight = Gauge(
            "inference_gateway_canary_weight",
            "Current canary routing weight",
            labelnames=("tenant_id",),
            registry=self.registry,
        )
        self.model_state = Gauge(
            "inference_gateway_model_state",
            "Model loading state: 0=loading, 1=ready, 2=failed",
            labelnames=("model_version",),
            registry=self.registry,
        )
        self.circuit_tripped = Gauge(
            "inference_gateway_circuit_breaker_tripped",
            "1 if the canary circuit breaker has tripped, else 0",
            labelnames=("model_version",),
            registry=self.registry,
        )

    def start_server(self, port: int, host: str = "127.0.0.1"):
        return start_http_server(port, addr=host, registry=self.registry)

    def render(self) -> tuple[bytes, str]:
        return generate_latest(self.registry), CONTENT_TYPE_LATEST
