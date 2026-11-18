from __future__ import annotations

import json
import threading
import time
import uuid
from dataclasses import dataclass
from typing import Any

import numpy as np
from pydantic import ValidationError

from rec_gateway.config import GatewayConfig
from rec_gateway.contracts import (
    build_success_payload,
    prepare_feature_vector,
    validate_protocol,
)
from rec_gateway.errors import BusinessError
from rec_gateway.metrics import GatewayMetrics
from rec_gateway.models import ModelManager, ReadyModel, utc_now
from rec_gateway.routing import RouteChoice, Router
from rec_gateway.schemas import InferenceRequest
from rec_gateway.stats import StatsManager
from rec_gateway.store import Store


@dataclass(frozen=True)
class ProcessedResponse:
    payload: dict[str, Any]
    status_code: int
    idempotent_replay: bool = False


class GatewayService:
    def __init__(
        self,
        config: GatewayConfig,
        store: Store,
        models: ModelManager,
        router: Router,
        metrics: GatewayMetrics,
        stats: StatsManager,
    ) -> None:
        self.config = config
        self.store = store
        self.models = models
        self.router = router
        self.metrics = metrics
        self.stats = stats
        self._started = False
        self._start_lock = threading.Lock()
        self._policy_lock = threading.RLock()
        self._rollback_lock = threading.RLock()
        self._rollback_active = False
        self._idempotency_lock = threading.Lock()
        self._inflight_idempotency: set[tuple[str, str]] = set()
        self._semaphore = threading.BoundedSemaphore(config.max_concurrency)

    def start(self) -> None:
        with self._start_lock:
            if self._started:
                return
            self.models.load_all()
            if not self.models.is_ready(self.config.canary_version):
                with self._policy_lock:
                    previous = self.router.set_all_canary_weights(0.0)
                    if any(weight > 0.0 for weight in previous.values()):
                        self.store.update_all_policies(
                            previous,
                            0.0,
                            "model_load_failure",
                            utc_now(),
                        )
            self.stats.restore(self.store)
            self.metrics.restore(self.store.aggregate_counts())
            for event in self.store.recent_rollbacks(200):
                self.metrics.record_rollback(event["version"], event["reason"])
            self._refresh_routing_metrics()
            self._refresh_all_window_metrics()
            self._started = True

    def close(self) -> None:
        self.store.close()

    def _refresh_routing_metrics(self) -> None:
        weights = self.router.tenant_weights()
        for tenant_id in sorted(self.config.tenants):
            self.metrics.set_weight(self.config.stable_version, tenant_id, 1.0)
            self.metrics.set_weight(
                self.config.canary_version, tenant_id, weights[tenant_id]
            )

    def _refresh_all_window_metrics(self) -> None:
        for version in (self.config.stable_version, self.config.canary_version):
            for tenant_id in sorted(self.config.tenants):
                self._refresh_window_metrics(version, tenant_id)

    def _refresh_window_metrics(self, version: str, tenant_id: str) -> None:
        runtime = self.models.get_ready(version)
        window_size = runtime.manifest.slo.window_size if runtime else 2000
        snapshot = self.stats.snapshot(version, tenant_id, window_size)
        self.metrics.update_window(
            version,
            tenant_id,
            self.stats.quantiles(version, tenant_id, window_size),
            snapshot["p99_ms"],
            float(snapshot["error_rate"]),
        )

    @staticmethod
    def _validation_error(exc: ValidationError) -> BusinessError:
        errors = exc.errors(include_url=False, include_input=False)
        first = errors[0] if errors else {}
        location = ".".join(str(part) for part in first.get("loc", ()))
        return BusinessError(
            "INVALID_REQUEST",
            "request does not match the API schema",
            {
                "field": location or None,
                "validation_error": first.get("type", "invalid"),
            },
        )

    def validate_request(self, raw: Any) -> InferenceRequest:
        if not isinstance(raw, dict):
            raise BusinessError(
                "INVALID_REQUEST",
                "request item must be an object",
                {"field": None, "actual_type": type(raw).__name__},
            )
        try:
            return InferenceRequest.model_validate(raw)
        except ValidationError as exc:
            raise self._validation_error(exc)

    def infer_one(
        self, raw: Any, record_validation_rejection: bool = True
    ) -> ProcessedResponse:
        try:
            request = self.validate_request(raw)
        except BusinessError as exc:
            if record_validation_rejection:
                self._record_early_rejection(raw, exc)
            raise
        if not self.router.has_tenant(request.tenant_id):
            raise BusinessError(
                "UNKNOWN_TENANT",
                "tenant is not configured",
                {"tenant_id": request.tenant_id},
            )
        request_id = request.request_id or f"req-{uuid.uuid4().hex}"
        request_hash = request.canonical_hash()
        idempotency_key = (request.tenant_id, request_id)
        claimed = False
        if request.request_id:
            with self._idempotency_lock:
                cached = self.store.get_idempotency(request.tenant_id, request_id)
                if cached is None and idempotency_key not in self._inflight_idempotency:
                    self._inflight_idempotency.add(idempotency_key)
                    claimed = True
            if cached is not None:
                return self._idempotent_response(cached, request_hash, request_id)
            if not claimed:
                deadline = time.monotonic() + min(
                    max(self.config.request_timeout_ms / 1000.0, 0.1),
                    5.0,
                )
                while time.monotonic() < deadline:
                    cached = self.store.get_idempotency(
                        request.tenant_id, request_id
                    )
                    if cached is not None:
                        return self._idempotent_response(
                            cached, request_hash, request_id
                        )
                    time.sleep(0.005)
                raise BusinessError(
                    "IDEMPOTENCY_IN_PROGRESS",
                    "an identical request_id is still being processed",
                    {"request_id": request_id},
                )
        try:
            return self._process_new(request, request_id, request_hash)
        finally:
            if claimed:
                with self._idempotency_lock:
                    self._inflight_idempotency.discard(idempotency_key)

    @staticmethod
    def _idempotent_response(
        cached: dict[str, Any],
        request_hash: str,
        request_id: str,
    ) -> ProcessedResponse:
        if cached["request_hash"] != request_hash:
            raise BusinessError(
                "IDEMPOTENCY_CONFLICT",
                "request_id was already used with a different request body",
                {"request_id": request_id},
            )
        return ProcessedResponse(
            payload=json.loads(cached["response_json"]),
            status_code=int(cached["status_code"]),
            idempotent_replay=True,
        )

    def _process_new(
        self,
        request: InferenceRequest,
        request_id: str,
        request_hash: str,
    ) -> ProcessedResponse:
        started = time.perf_counter()
        choice = self.router.choose(
            request.tenant_id,
            request.client_id,
            request_id,
        )
        selected_version, fallback_reason = self._select_available(choice)
        target_version = selected_version or choice.candidate_version
        selected_runtime: ReadyModel | None = None
        outcome = "error"
        error_code: str | None = None
        acquired = False
        payload: dict[str, Any] | None = None
        status_code = 200
        try:
            if selected_version is None:
                raise BusinessError(
                    "VERSION_UNAVAILABLE",
                    "no model version is currently available",
                    {
                        "stable_version": self.config.stable_version,
                        "canary_version": self.config.canary_version,
                    },
                )
            selected_runtime = self.models.get_ready(selected_version)
            if selected_runtime is None:
                raise BusinessError(
                    "VERSION_UNAVAILABLE",
                    "selected model version is not ready",
                    {"version": selected_version},
                )

            timeout_seconds = (
                min(
                    self.config.request_timeout_ms,
                    selected_runtime.manifest.slo.request_timeout_ms,
                )
                / 1000.0
            )
            if timeout_seconds <= 0:
                timeout_seconds = self.config.request_timeout_ms / 1000.0
            acquired = self._semaphore.acquire(timeout=timeout_seconds)
            if not acquired:
                raise BusinessError(
                    "INFERENCE_TIMEOUT",
                    "request timed out waiting for inference capacity",
                    {"timeout_ms": round(timeout_seconds * 1000.0, 3)},
                )

            validate_protocol(request.protocol_version or "", selected_runtime.manifest)
            feature_vector = prepare_feature_vector(
                selected_runtime.manifest,
                request.features,
                request.feature_names,
            )

            delay_ms = 0.0
            if request.fault_injection is not None:
                if not self.config.fault_injection_enabled:
                    raise BusinessError(
                        "FAULT_INJECTION_DISABLED",
                        "fault injection is disabled by gateway configuration",
                        {"column_index": None},
                    )
                delay_ms = request.fault_injection.inference_delay_ms
            if delay_ms > 0.0:
                time.sleep(delay_ms / 1000.0)
            elapsed_ms = (time.perf_counter() - started) * 1000.0
            if elapsed_ms >= timeout_seconds * 1000.0:
                raise BusinessError(
                    "INFERENCE_TIMEOUT",
                    "request exceeded its end-to-end timeout before inference",
                    {"timeout_ms": round(timeout_seconds * 1000.0, 3)},
                )

            model_input = np.asarray(
                feature_vector, dtype=np.float32
            ).reshape(1, -1)
            try:
                output_values = selected_runtime.session.run(
                    [
                        spec.name
                        for spec in selected_runtime.manifest.output_specs
                    ],
                    {
                        selected_runtime.manifest.input_spec.name: model_input,
                    },
                )
            except Exception:
                raise BusinessError(
                    "INFERENCE_ERROR",
                    "model inference failed",
                    {"version": selected_version},
                )
            elapsed_ms = (time.perf_counter() - started) * 1000.0
            if elapsed_ms >= timeout_seconds * 1000.0:
                raise BusinessError(
                    "INFERENCE_TIMEOUT",
                    "request exceeded its end-to-end timeout during inference",
                    {"timeout_ms": round(timeout_seconds * 1000.0, 3)},
                )
            payload = build_success_payload(
                selected_runtime.manifest,
                selected_version,
                request_id,
                output_values,
                1,
            )
            outcome = "success"
            status_code = 200
        except BusinessError as exc:
            error_code = exc.code
            status_code = exc.http_status or 500
            outcome = "timeout" if exc.code == "INFERENCE_TIMEOUT" else "error"
            payload = self._error_payload(
                request_id,
                request.protocol_version,
                target_version,
                exc,
            )
        except Exception:
            error_code = "INTERNAL_ERROR"
            status_code = 500
            outcome = "error"
            payload = self._error_payload(
                request_id,
                request.protocol_version,
                target_version,
                BusinessError(
                    "INTERNAL_ERROR",
                    "gateway could not complete the request",
                    {"column_index": None},
                    500,
                ),
            )
        finally:
            if acquired:
                self._semaphore.release()

        latency_ms = round((time.perf_counter() - started) * 1000.0, 3)
        served_version = (
            target_version
            if payload is not None
            and bool(payload.get("ok"))
            else None
        )
        created_at = utc_now()
        idempotency_record = None
        if outcome == "success" and request.request_id and payload is not None:
            idempotency_record = {
                "tenant_id": request.tenant_id,
                "request_id": request_id,
                "request_hash": request_hash,
                "status_code": status_code,
                "response_json": json.dumps(
                    payload,
                    ensure_ascii=False,
                    sort_keys=True,
                    separators=(",", ":"),
                ),
                "created_at": created_at,
            }
        self.store.record_request(
            {
                "request_id": request_id,
                "tenant_id": request.tenant_id,
                "client_id": request.client_id,
                "client_keyed": choice.client_keyed,
                "protocol_version": request.protocol_version,
                "client_bucket": choice.client_bucket,
                "canary_weight": choice.canary_weight,
                "candidate_version": choice.candidate_version,
                "selected_version": target_version,
                "served_version": served_version,
                "outcome": outcome,
                "error_code": error_code,
                "fallback_reason": fallback_reason,
                "created_at": created_at,
            },
            {
                "request_id": request_id,
                "tenant_id": request.tenant_id,
                "version": target_version,
                "protocol_version": request.protocol_version,
                "outcome": outcome,
                "latency_ms": latency_ms,
                "error_code": error_code,
                "created_at": created_at,
            },
            idempotency_record,
        )
        self.metrics.record(
            target_version,
            request.tenant_id,
            outcome,
            error_code,
        )
        self.stats.add(
            target_version,
            request.tenant_id,
            outcome,
            latency_ms,
        )
        self._refresh_window_metrics(target_version, request.tenant_id)
        self._evaluate_rollback(target_version, request.tenant_id)
        assert payload is not None
        return ProcessedResponse(payload=payload, status_code=status_code)

    def _select_available(
        self, choice: RouteChoice
    ) -> tuple[str | None, str | None]:
        if self.models.is_ready(choice.candidate_version):
            return choice.candidate_version, None
        alternatives = (
            choice.stable_version
            if choice.candidate_version == choice.canary_version
            else choice.canary_version
        )
        if self.models.is_ready(alternatives):
            return alternatives, "candidate_model_unavailable"
        return None, "all_models_unavailable"

    @staticmethod
    def _error_payload(
        request_id: str,
        declared_protocol: str | None,
        target_version: str | None,
        error: BusinessError,
    ) -> dict[str, Any]:
        return {
            "ok": False,
            "request_id": request_id,
            "protocol_version": declared_protocol,
            "served_version": None,
            "target_version": target_version,
            "error": error.as_dict(),
        }

    def infer_batch(self, raw_items: list[Any]) -> dict[str, Any]:
        if len(raw_items) > self.config.max_batch_size:
            raise BusinessError(
                "BATCH_LIMIT_EXCEEDED",
                "batch size exceeds the configured maximum",
                {
                    "batch_size": len(raw_items),
                    "max_batch_size": self.config.max_batch_size,
                },
            )
        results: list[dict[str, Any]] = []
        for raw in raw_items:
            try:
                processed = self.infer_one(raw, record_validation_rejection=False)
                results.append(processed.payload)
            except BusinessError as exc:
                self._record_early_rejection(raw, exc)
                request_id = None
                tenant_id = None
                protocol_version = None
                target_version = None
                if isinstance(raw, dict):
                    request_id = raw.get("request_id")
                    tenant_id = raw.get("tenant_id")
                    protocol_version = raw.get("protocol_version")
                    if protocol_version is None:
                        protocol_version = raw.get("feature_protocol_version")
                if tenant_id in self.config.tenants:
                    target_version = self.router.choose(
                        tenant_id,
                        raw.get("client_id") if isinstance(raw, dict) else None,
                        str(request_id or ""),
                    ).candidate_version
                results.append(
                    self._error_payload(
                        str(request_id) if request_id else f"req-{uuid.uuid4().hex}",
                        protocol_version,
                        target_version,
                        exc,
                    )
                )
        return {"ok": True, "results": results}

    def _record_early_rejection(self, raw: Any, error: BusinessError) -> None:
        if not isinstance(raw, dict):
            return
        tenant_id = raw.get("tenant_id")
        if not isinstance(tenant_id, str) or tenant_id not in self.config.tenants:
            return
        client_id = raw.get("client_id")
        if not isinstance(client_id, str):
            client_id = None
        request_id_value = raw.get("request_id")
        request_id = (
            request_id_value
            if isinstance(request_id_value, str) and request_id_value
            else f"req-{uuid.uuid4().hex}"
        )
        protocol_version = (
            raw.get("protocol_version")
            or raw.get("feature_protocol_version")
            or raw.get("protocol")
        )
        if not isinstance(protocol_version, str):
            protocol_version = None
        choice = self.router.choose(tenant_id, client_id, request_id)
        selected_version, fallback_reason = self._select_available(choice)
        target_version = selected_version or choice.candidate_version
        created_at = utc_now()
        self.store.record_request(
            {
                "request_id": request_id,
                "tenant_id": tenant_id,
                "client_id": client_id,
                "client_keyed": choice.client_keyed,
                "protocol_version": protocol_version,
                "client_bucket": choice.client_bucket,
                "canary_weight": choice.canary_weight,
                "candidate_version": choice.candidate_version,
                "selected_version": target_version,
                "served_version": None,
                "outcome": "error",
                "error_code": error.code,
                "fallback_reason": fallback_reason,
                "created_at": created_at,
            },
            {
                "request_id": request_id,
                "tenant_id": tenant_id,
                "version": target_version,
                "protocol_version": protocol_version,
                "outcome": "error",
                "latency_ms": 0.0,
                "error_code": error.code,
                "created_at": created_at,
            },
        )
        self.metrics.record(target_version, tenant_id, "error", error.code)
        self.stats.add(target_version, tenant_id, "error", 0.0)
        self._refresh_window_metrics(target_version, tenant_id)
        self._evaluate_rollback(target_version, tenant_id)

    def update_weight(
        self, tenant_id: str, weight: float, source: str = "admin"
    ) -> dict[str, Any]:
        if not self.router.has_tenant(tenant_id):
            raise BusinessError(
                "UNKNOWN_TENANT",
                "tenant is not configured",
                {"tenant_id": tenant_id},
            )
        with self._policy_lock:
            previous, current = self.router.set_weight(tenant_id, weight)
            self.store.update_policy(
                tenant_id,
                previous,
                current,
                source,
                utc_now(),
            )
            self.metrics.set_weight(
                self.config.canary_version, tenant_id, current
            )
            if current > 0.0:
                self._rollback_active = False
        return {
            "tenant_id": tenant_id,
            "previous_canary_weight": previous,
            "canary_weight": current,
            "stable_version": self.config.stable_version,
            "canary_version": self.config.canary_version,
        }

    def _evaluate_rollback(self, version: str, tenant_id: str) -> None:
        if version != self.config.canary_version:
            return
        runtime = self.models.get_ready(version)
        if runtime is None or not self.router.any_canary_enabled():
            return
        slo = runtime.manifest.slo
        snapshot = self.stats.snapshot(version, tenant_id, slo.window_size)
        sample_count = int(snapshot["window_samples"] or 0)
        if sample_count < slo.min_samples:
            return
        p99 = snapshot["p99_ms"]
        error_rate = float(snapshot["error_rate"] or 0.0)
        reason: str | None = None
        metric_name: str | None = None
        observed: float | None = None
        threshold: float | None = None
        if p99 is not None and float(p99) > slo.latency_p99_ms:
            reason = "p99_latency_exceeded"
            metric_name = "p99_latency_ms"
            observed = float(p99)
            threshold = slo.latency_p99_ms
        elif error_rate > slo.error_rate:
            reason = "error_rate_exceeded"
            metric_name = "error_rate"
            observed = error_rate
            threshold = slo.error_rate
        if reason is None:
            return
        with self._rollback_lock, self._policy_lock:
            if self._rollback_active or not self.router.any_canary_enabled():
                return
            previous_weights = self.router.set_all_canary_weights(0.0)
            triggered_at = utc_now()
            self.store.update_all_policies(
                previous_weights,
                0.0,
                "auto_rollback",
                triggered_at,
            )
            self.store.record_rollback(
                version=version,
                tenant_id=tenant_id,
                reason=reason,
                metric_name=metric_name,
                observed=observed,
                threshold=threshold,
                window_samples=sample_count,
                previous_weights=previous_weights,
                triggered_at=triggered_at,
            )
            for version_name in (
                self.config.stable_version,
                self.config.canary_version,
            ):
                for target_tenant in sorted(self.config.tenants):
                    weight = (
                        1.0
                        if version_name == self.config.stable_version
                        else self.router.weight_for(target_tenant)
                    )
                    self.metrics.set_weight(version_name, target_tenant, weight)
            self.metrics.record_rollback(version, reason)
            self._rollback_active = True

    def status(self) -> dict[str, Any]:
        policies = {
            tenant_id: {
                "canary_weight": self.router.weight_for(tenant_id),
                "stable_weight": 1.0,
            }
            for tenant_id in sorted(self.config.tenants)
        }
        rollbacks = self.store.recent_rollbacks(20)
        return {
            "schema_version": 1,
            "status": (
                "ready"
                if any(
                    item["status"] == "ready"
                    for item in self.models.snapshot().values()
                )
                else "unavailable"
            ),
            "routing": {
                "stable_version": self.config.stable_version,
                "canary_version": self.config.canary_version,
                "tenants": policies,
                "policy": "deterministic_client_bucket_without_stickiness",
            },
            "versions": self.models.snapshot(),
            "recent_rollbacks": rollbacks,
            "generated_at": utc_now(),
        }

    def route_history(
        self,
        tenant_id: str | None,
        client_id: str | None,
        limit: int,
    ) -> dict[str, Any]:
        return {
            "tenant_id": tenant_id,
            "client_id": client_id,
            "events": self.store.route_history(tenant_id, client_id, limit),
        }
