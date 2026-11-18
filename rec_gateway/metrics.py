from __future__ import annotations

from typing import Any

from prometheus_client import CollectorRegistry, Counter, Gauge, generate_latest


LATENCY_QUANTILES = ("0.5", "0.95", "0.99")


class GatewayMetrics:
    def __init__(
        self,
        versions: tuple[str, ...],
        tenants: tuple[str, ...],
    ) -> None:
        self.registry = CollectorRegistry()
        self.requests = Counter(
            "gateway_requests_total",
            "Inference requests handled by the gateway",
            ("version", "tenant"),
            registry=self.registry,
        )
        self.errors = Counter(
            "gateway_errors_total",
            "Failed inference requests",
            ("version", "tenant"),
            registry=self.registry,
        )
        self.timeouts = Counter(
            "gateway_timeouts_total",
            "Timed out inference requests",
            ("version", "tenant"),
            registry=self.registry,
        )
        self.protocol_mismatches = Counter(
            "gateway_protocol_mismatch_rejections_total",
            "Requests rejected for a feature protocol mismatch",
            ("version", "tenant"),
            registry=self.registry,
        )
        self.tenant_requests = Counter(
            "gateway_tenant_requests_total",
            "Inference requests by tenant",
            ("tenant",),
            registry=self.registry,
        )
        self.latency_quantiles = Gauge(
            "gateway_latency_ms",
            "End-to-end latency quantiles in milliseconds",
            ("version", "tenant", "quantile"),
            registry=self.registry,
        )
        self.latency_p99 = Gauge(
            "gateway_latency_p99_ms",
            "End-to-end P99 latency in milliseconds",
            ("version", "tenant"),
            registry=self.registry,
        )
        self.error_rate = Gauge(
            "gateway_error_rate",
            "Error rate over the recent per-tenant version window",
            ("version", "tenant"),
            registry=self.registry,
        )
        self.current_weight = Gauge(
            "gateway_current_weight",
            "Current canary traffic weight",
            ("version", "tenant"),
            registry=self.registry,
        )
        self.rollbacks = Counter(
            "gateway_rollbacks_total",
            "Automatic canary rollbacks",
            ("version", "reason"),
            registry=self.registry,
        )
        for version in versions:
            for tenant_id in tenants:
                self.requests.labels(version, tenant_id)
                self.errors.labels(version, tenant_id)
                self.timeouts.labels(version, tenant_id)
                self.protocol_mismatches.labels(version, tenant_id)
                self.latency_p99.labels(version, tenant_id).set(0)
                self.error_rate.labels(version, tenant_id).set(0)
                self.current_weight.labels(version, tenant_id).set(0)
                for quantile in LATENCY_QUANTILES:
                    self.latency_quantiles.labels(
                        version, tenant_id, quantile
                    ).set(0)
        for tenant_id in tenants:
            self.tenant_requests.labels(tenant_id)

    def restore(self, aggregates: list[dict[str, Any]]) -> None:
        for row in aggregates:
            version = row["version"]
            tenant_id = row["tenant_id"]
            requests = int(row["requests"] or 0)
            errors = int(row["errors"] or 0)
            timeouts = int(row["timeouts"] or 0)
            mismatches = int(row["protocol_mismatches"] or 0)
            self.requests.labels(version, tenant_id).inc(requests)
            self.errors.labels(version, tenant_id).inc(errors)
            self.timeouts.labels(version, tenant_id).inc(timeouts)
            self.protocol_mismatches.labels(version, tenant_id).inc(mismatches)
            self.tenant_requests.labels(tenant_id).inc(requests)

    def record(
        self,
        version: str,
        tenant_id: str,
        outcome: str,
        error_code: str | None,
    ) -> None:
        self.requests.labels(version, tenant_id).inc()
        self.tenant_requests.labels(tenant_id).inc()
        if outcome != "success":
            self.errors.labels(version, tenant_id).inc()
        if outcome == "timeout":
            self.timeouts.labels(version, tenant_id).inc()
        if error_code == "FEATURE_PROTOCOL_MISMATCH":
            self.protocol_mismatches.labels(version, tenant_id).inc()

    def update_window(
        self,
        version: str,
        tenant_id: str,
        quantiles: dict[str, float | None],
        p99_ms: float | None,
        error_rate: float,
    ) -> None:
        for quantile in LATENCY_QUANTILES:
            value = quantiles.get(quantile)
            self.latency_quantiles.labels(version, tenant_id, quantile).set(
                float(value or 0.0)
            )
        self.latency_p99.labels(version, tenant_id).set(float(p99_ms or 0.0))
        self.error_rate.labels(version, tenant_id).set(float(error_rate))

    def set_weight(self, version: str, tenant_id: str, weight: float) -> None:
        self.current_weight.labels(version, tenant_id).set(float(weight))

    def record_rollback(self, version: str, reason: str) -> None:
        self.rollbacks.labels(version, reason).inc()

    def render(self) -> bytes:
        return generate_latest(self.registry)
