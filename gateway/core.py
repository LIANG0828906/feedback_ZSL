from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import math
import threading
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

import numpy as np
import onnxruntime as ort

from .config import AppConfig, ModelConfig, ProtocolConfig
from .metrics import MetricsCollector

LOGGER = logging.getLogger("ranking_gateway")


class BusinessError(Exception):
    def __init__(
        self,
        status_code: int,
        code: str,
        message: str,
        *,
        field: str | None = None,
        details: dict[str, Any] | None = None,
        version: str | None = None,
        tenant: str | None = None,
    ) -> None:
        super().__init__(message)
        self.status_code = status_code
        self.code = code
        self.message = message
        self.field = field
        self.details = details or {}
        self.version = version
        self.tenant = tenant

    def body(self, request_id: str) -> dict[str, Any]:
        error: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
            "request_id": request_id,
        }
        if self.field is not None:
            error["field"] = self.field
        if self.version is not None:
            error["version"] = self.version
        error.update(self.details)
        return {"error": error}


@dataclass
class LoadTransition:
    at: float
    from_state: str
    to_state: str
    reason: str
    error_code: str | None = None

    def as_dict(self) -> dict[str, Any]:
        return {
            "at": round(self.at, 6),
            "from": self.from_state,
            "to": self.to_state,
            "reason": self.reason,
            "error_code": self.error_code,
        }


@dataclass
class ModelSlot:
    config: ModelConfig
    protocol: ProtocolConfig
    session: ort.InferenceSession | None = None
    state: str = "PENDING"
    ready: bool = False
    reloading: bool = False
    last_load_error: str | None = None
    last_load_error_code: str | None = None
    loaded_at: float | None = None
    model_id: str | None = None
    input_shape: list[int | str] | None = None
    transitions: list[LoadTransition] = field(default_factory=list)
    load_generation: int = 0

    def __post_init__(self) -> None:
        self.ready_event = threading.Event()

    def transition(self, to_state: str, reason: str, error_code: str | None = None) -> None:
        previous = self.state
        self.state = to_state
        self.transitions.append(
            LoadTransition(
                at=time.time(),
                from_state=previous,
                to_state=to_state,
                reason=reason,
                error_code=error_code,
            )
        )
        del self.transitions[:-100]


@dataclass(frozen=True)
class RouteDecision:
    version: str
    weights: dict[str, float]
    bucket: int
    scope: str


@dataclass(frozen=True)
class ValidationResult:
    feature_names: tuple[str, ...]
    features: tuple[float, ...]


class ModelRegistry:
    def __init__(self, config: AppConfig, metrics: MetricsCollector) -> None:
        self.config = config
        self.metrics = metrics
        self._lock = threading.RLock()
        self._slots: dict[str, ModelSlot] = {
            version: ModelSlot(
                config=model_config,
                protocol=config.protocols[model_config.protocol_version],
            )
            for version, model_config in config.models.items()
        }
        self._fault_lock = threading.Lock()
        self._inference_delay_ms: dict[str, float] = {}
        self._fail_next_load: set[str] = set()
        self._startup_threads: list[threading.Thread] = []

    def start_background_loads(self) -> None:
        for version in self._slots:
            thread = threading.Thread(
                target=self.reload,
                args=(version,),
                kwargs={"startup": True},
                name=f"model-load-{version}",
                daemon=True,
            )
            thread.start()
            self._startup_threads.append(thread)

    def wait_startup_loads(self, timeout: float | None = None) -> None:
        deadline = None if timeout is None else time.monotonic() + timeout
        for thread in self._startup_threads:
            remaining = None
            if deadline is not None:
                remaining = max(0.0, deadline - time.monotonic())
            thread.join(remaining)

    def _load_model(self, slot: ModelSlot) -> ort.InferenceSession:
        path: Path = slot.config.path
        if not path.is_file():
            raise FileNotFoundError(f"model file does not exist: {path}")
        options = ort.SessionOptions()
        options.intra_op_num_threads = 1
        options.inter_op_num_threads = 1
        options.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
        session = ort.InferenceSession(
            str(path), sess_options=options, providers=["CPUExecutionProvider"]
        )
        inputs = session.get_inputs()
        if len(inputs) != 1:
            raise ValueError(f"expected one model input, got {len(inputs)}")
        shape = list(inputs[0].shape)
        expected_dim = len(slot.protocol.feature_names)
        if len(shape) != 2 or shape[1] != expected_dim:
            raise ValueError(
                f"model input shape {shape} does not match protocol "
                f"{slot.protocol.version} ({expected_dim} features)"
            )
        return session

    def _consume_load_failure(self, version: str, startup: bool) -> bool:
        if startup and version in self.config.startup_failures:
            return True
        with self._fault_lock:
            if version in self._fail_next_load:
                self._fail_next_load.remove(version)
                return True
        return False

    def reload(self, version: str, *, startup: bool = False) -> bool:
        if version not in self._slots:
            raise KeyError(version)
        with self._lock:
            slot = self._slots[version]
            had_ready_session = slot.session is not None
            slot.reloading = True
            slot.transition("LOADING", "startup_load" if startup else "manual_reload")

        try:
            if self._consume_load_failure(version, startup):
                raise RuntimeError("injected model load failure")
            session = self._load_model(slot)
            metadata = session.get_modelmeta()
            model_id = (
                metadata.custom_metadata_map.get("model_id")
                or metadata.producer_name
                or slot.config.path.stem
            )
            input_shape = [
                item if isinstance(item, str) else int(item)
                for item in session.get_inputs()[0].shape
            ]
            with self._lock:
                slot.session = session
                slot.ready = True
                slot.reloading = False
                slot.last_load_error = None
                slot.last_load_error_code = None
                slot.loaded_at = time.time()
                slot.model_id = model_id
                slot.input_shape = input_shape
                slot.load_generation += 1
                slot.ready_event.set()
                slot.transition("READY", "model_loaded")
            LOGGER.info(
                json.dumps(
                    {
                        "event": "model_loaded",
                        "version": version,
                        "model_id": model_id,
                        "protocol_version": slot.protocol.version,
                        "path": str(slot.config.path),
                    },
                    sort_keys=True,
                )
            )
            return True
        except Exception as exc:
            with self._lock:
                slot.reloading = False
                slot.last_load_error = f"{type(exc).__name__}: {exc}"
                slot.last_load_error_code = "MODEL_LOAD_FAILED"
                retain = (
                    had_ready_session
                    and self.config.loading.retain_previous_on_reload_failure
                )
                if retain:
                    slot.ready = True
                    slot.transition(
                        "READY", "reload_failed_previous_retained", "MODEL_LOAD_FAILED"
                    )
                else:
                    slot.session = None
                    slot.ready = False
                    slot.ready_event.clear()
                    slot.transition("FAILED", "model_load_failed", "MODEL_LOAD_FAILED")
            LOGGER.error(
                json.dumps(
                    {
                        "event": "model_load_failed",
                        "version": version,
                        "error_code": "MODEL_LOAD_FAILED",
                        "error": f"{type(exc).__name__}: {exc}",
                        "previous_retained": bool(had_ready_session),
                    },
                    sort_keys=True,
                )
            )
            return False

    def reload_async(self, version: str) -> None:
        if version not in self._slots:
            raise KeyError(version)
        thread = threading.Thread(
            target=self.reload,
            args=(version,),
            name=f"model-reload-{version}",
            daemon=True,
        )
        thread.start()

    async def wait_ready(self, version: str, timeout_ms: int) -> bool:
        deadline = time.monotonic() + timeout_ms / 1000.0
        while True:
            slot = self.get_slot(version)
            if slot.ready and slot.session is not None:
                return True
            if not slot.reloading and slot.state == "FAILED":
                return False
            if time.monotonic() >= deadline:
                return False
            await asyncio.sleep(0.01)

    def get_slot(self, version: str) -> ModelSlot:
        with self._lock:
            return self._slots[version]

    def session_for(self, version: str) -> ort.InferenceSession:
        with self._lock:
            slot = self._slots[version]
            if not slot.ready or slot.session is None:
                raise RuntimeError(f"model {version} is not ready")
            return slot.session

    def ready_versions(self) -> list[str]:
        with self._lock:
            return [
                version
                for version, slot in self._slots.items()
                if slot.ready and slot.session is not None
            ]

    def set_inference_delay(self, version: str, delay_ms: float) -> None:
        if version not in self._slots:
            raise KeyError(version)
        with self._fault_lock:
            if delay_ms <= 0:
                self._inference_delay_ms.pop(version, None)
            else:
                self._inference_delay_ms[version] = float(delay_ms)

    def inference_delay_for(self, version: str) -> float:
        with self._fault_lock:
            return self._inference_delay_ms.get(version, 0.0)

    def queue_load_failure(self, version: str) -> None:
        if version not in self._slots:
            raise KeyError(version)
        with self._fault_lock:
            self._fail_next_load.add(version)

    def faults(self) -> dict[str, Any]:
        with self._fault_lock:
            return {
                "inference_delay_ms": dict(sorted(self._inference_delay_ms.items())),
                "fail_next_load": sorted(self._fail_next_load),
            }

    def clear_faults(self) -> None:
        with self._fault_lock:
            self._inference_delay_ms.clear()
            self._fail_next_load.clear()

    def infer(
        self, version: str, features: Iterable[float]
    ) -> tuple[float, float]:
        delay_ms = self.inference_delay_for(version)
        if delay_ms > 0:
            time.sleep(delay_ms / 1000.0)
        session = self.session_for(version)
        input_name = session.get_inputs()[0].name
        values = np.asarray([list(features)], dtype=np.float32)
        output = session.run(None, {input_name: values})[0]
        score = float(np.asarray(output).reshape(-1)[0])
        if not math.isfinite(score):
            raise ValueError("model returned a non-finite score")
        return score, delay_ms

    def ready_map(self) -> dict[str, bool]:
        with self._lock:
            return {
                version: bool(slot.ready and slot.session is not None)
                for version, slot in self._slots.items()
            }

    def state_map(self) -> dict[str, Any]:
        with self._lock:
            result: dict[str, Any] = {}
            for version, slot in self._slots.items():
                result[version] = {
                    "model_id": slot.model_id,
                    "path": str(slot.config.path),
                    "role": slot.config.role,
                    "load_state": slot.state,
                    "ready": bool(slot.ready and slot.session is not None),
                    "reloading": slot.reloading,
                    "protocol_version": slot.protocol.version,
                    "expected_feature_names": list(slot.protocol.feature_names),
                    "input_shape": slot.input_shape,
                    "loaded_at": slot.loaded_at,
                    "last_load_error": slot.last_load_error,
                    "last_load_error_code": slot.last_load_error_code,
                    "transitions": [item.as_dict() for item in slot.transitions[-25:]],
                }
            return result


class RoutingTable:
    def __init__(self, config: AppConfig) -> None:
        self._config = config
        self._lock = threading.RLock()
        self._default = self._normalize(config.routing.default_weights)
        self._tenant = {
            tenant: self._normalize(weights)
            for tenant, weights in config.routing.tenant_weights.items()
        }
        self._client = {
            tenant: {
                client: self._normalize(weights)
                for client, weights in clients.items()
            }
            for tenant, clients in config.routing.client_weights.items()
        }
        self._disabled_versions: set[str] = set()

    def _normalize(self, weights: dict[str, float]) -> dict[str, float]:
        complete = {version: max(0.0, float(weights.get(version, 0.0))) for version in self._config.models}
        total = sum(complete.values())
        if total <= 0:
            complete = {version: 0.0 for version in self._config.models}
            complete[self._config.stable_version] = 1.0
            return complete
        return {version: value / total for version, value in complete.items()}

    def _configured_weights_locked(
        self, tenant: str, client_id: str | None
    ) -> tuple[dict[str, float], str]:
        if client_id and client_id in self._client.get(tenant, {}):
            weights = dict(self._client[tenant][client_id])
            scope = "client"
        elif tenant in self._tenant:
            weights = dict(self._tenant[tenant])
            scope = "tenant"
        else:
            weights = dict(self._default)
            scope = "default"
        return weights, scope

    def _effective_weights_locked(
        self, tenant: str, client_id: str | None
    ) -> tuple[dict[str, float], str]:
        weights, scope = self._configured_weights_locked(tenant, client_id)
        for version in self._disabled_versions:
            if version in weights:
                weights[version] = 0.0
        return self._normalize(weights), scope

    def route(self, tenant: str, client_id: str | None) -> RouteDecision:
        with self._lock:
            weights, scope = self._effective_weights_locked(tenant, client_id)
            bucket_key = f"{self._config.routing.hash_seed}\0{tenant}\0{client_id or ''}"
            digest = hashlib.sha256(bucket_key.encode("utf-8")).digest()
            bucket = int.from_bytes(digest[:8], "big") % self._config.routing.bucket_count
            threshold = bucket / self._config.routing.bucket_count
            cumulative = 0.0
            selected = self._config.stable_version
            for version in self._config.models:
                weight = weights.get(version, 0.0)
                if weight <= 0:
                    continue
                cumulative += weight
                selected = version
                if threshold < cumulative:
                    break
            return RouteDecision(
                version=selected,
                weights=weights,
                bucket=bucket,
                scope=scope,
            )

    def update(
        self,
        weights: dict[str, float],
        *,
        tenant: str | None = None,
        client_id: str | None = None,
        clear_rollback: bool = False,
    ) -> dict[str, dict[str, float]]:
        unknown = set(weights) - set(self._config.models)
        if unknown:
            raise ValueError(f"unknown model versions: {', '.join(sorted(unknown))}")
        if client_id and not tenant:
            raise ValueError("tenant is required when client_id is provided")
        with self._lock:
            if client_id:
                current = dict(self._client.get(tenant, {}).get(client_id, self._default))  # type: ignore[arg-type]
            elif tenant:
                current = dict(self._tenant.get(tenant, self._default))
            else:
                current = dict(self._default)
            merged = dict(current)
            merged.update(weights)
            normalized = self._normalize(merged)
            if client_id:
                self._client.setdefault(tenant, {})[client_id] = normalized  # type: ignore[index]
            elif tenant:
                self._tenant[tenant] = normalized
            else:
                self._default = normalized
            if clear_rollback:
                self._disabled_versions.clear()
            return self.effective_map()

    def disable(self, version: str) -> None:
        with self._lock:
            self._disabled_versions.add(version)

    def enable(self, version: str) -> None:
        with self._lock:
            self._disabled_versions.discard(version)

    def disabled(self) -> set[str]:
        with self._lock:
            return set(self._disabled_versions)

    def default_weights(self) -> dict[str, float]:
        with self._lock:
            return dict(self._default)

    def effective_map(self) -> dict[str, dict[str, float]]:
        with self._lock:
            return {
                "default": self._effective_weights_locked("__unknown__", None)[0],
                "tenants": {
                    tenant: self._effective_weights_locked(tenant, None)[0]
                    for tenant in sorted(self._tenant)
                },
                "clients": {
                    tenant: {
                        client: self._effective_weights_locked(tenant, client)[0]
                        for client in sorted(clients)
                    }
                    for tenant, clients in sorted(self._client.items())
                },
                "disabled_versions": sorted(self._disabled_versions),
            }


class RollbackMonitor:
    def __init__(
        self,
        config: AppConfig,
        metrics: MetricsCollector,
        routing: RoutingTable,
    ) -> None:
        self._config = config
        self._metrics = metrics
        self._routing = routing
        self._stop = threading.Event()
        self._lock = threading.Lock()
        self._records: list[dict[str, Any]] = []
        self._thread = threading.Thread(
            target=self._run, name="rollback-monitor", daemon=True
        )

    def start(self) -> None:
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        self._thread.join(timeout=2.0)

    def records(self) -> list[dict[str, Any]]:
        with self._lock:
            return [dict(item) for item in self._records]

    def _run(self) -> None:
        interval = self._config.monitoring.evaluate_interval_ms / 1000.0
        while not self._stop.wait(interval):
            self.evaluate_once()

    def evaluate_once(self, now: float | None = None) -> list[dict[str, Any]]:
        created: list[dict[str, Any]] = []
        for version in self._config.monitoring.auto_rollback_versions:
            if version in self._routing.disabled():
                continue
            triggers: list[dict[str, Any]] = []
            scope = "__version__"
            observed_summary = self._metrics.window_summary(version)
            if observed_summary["requests"] >= self._config.monitoring.min_samples:
                triggers.extend(self._threshold_triggers(observed_summary))
            if not triggers:
                for tenant in self._metrics.streams_for_version(version):
                    summary = self._metrics.window_summary_for(version, tenant)
                    if summary["requests"] < self._config.monitoring.min_samples:
                        continue
                    tenant_triggers = self._threshold_triggers(summary)
                    if tenant_triggers:
                        triggers = tenant_triggers
                        observed_summary = summary
                        scope = tenant
                        break
            if not triggers:
                continue

            rollback_at = time.time() if now is None else now
            record = {
                "version": version,
                "fallback_to": self._config.stable_version,
                "scope": scope,
                "tenant": None if scope == "__version__" else scope,
                "trigger_metric": triggers[0]["metric"],
                "observed": triggers[0]["observed"],
                "threshold": triggers[0]["threshold"],
                "triggers": triggers,
                "window": {
                    "requests": observed_summary["requests"],
                    "errors": observed_summary["errors"],
                    "timeouts": observed_summary["timeouts"],
                    "error_rate": rounded(observed_summary["error_rate"], 6),
                    "p99_ms": observed_summary["p99_ms"],
                },
                "rollback_at": round(rollback_at, 6),
                "rollback_time_iso": time.strftime(
                    "%Y-%m-%dT%H:%M:%SZ", time.gmtime(rollback_at)
                ),
            }
            self._routing.disable(version)
            self._metrics.mark_rollback(version)
            with self._lock:
                self._records.append(record)
            LOGGER.warning(json.dumps({"event": "auto_rollback", **record}, sort_keys=True))
            created.append(record)
        return created

    def _threshold_triggers(self, summary: dict[str, float | int]) -> list[dict[str, Any]]:
        result: list[dict[str, Any]] = []
        p99 = float(summary["p99_ms"])
        error_rate = float(summary["error_rate"])
        if p99 > self._config.monitoring.p99_latency_ms:
            result.append(
                {
                    "metric": "p99_latency_ms",
                    "observed": p99,
                    "threshold": self._config.monitoring.p99_latency_ms,
                }
            )
        if error_rate > self._config.monitoring.error_rate:
            result.append(
                {
                    "metric": "error_rate",
                    "observed": rounded(error_rate, 6),
                    "threshold": self._config.monitoring.error_rate,
                }
            )
        return result


def rounded(value: float, digits: int) -> float:
    return float(round(value, digits))


class GatewayService:
    def __init__(self, config: AppConfig) -> None:
        self.config = config
        self.metrics = MetricsCollector(
            config.models.keys(), config.monitoring.window_seconds
        )
        self.registry = ModelRegistry(config, self.metrics)
        self.routing = RoutingTable(config)
        self.rollback_monitor = RollbackMonitor(config, self.metrics, self.routing)
        self.started_at = time.time()
        self._startup_complete = False

    def start(self) -> None:
        self.registry.start_background_loads()
        self.rollback_monitor.start()

    def stop(self) -> None:
        self.rollback_monitor.stop()
        self.registry.wait_startup_loads(timeout=2.0)

    async def predict(self, request: dict[str, Any]) -> dict[str, Any]:
        started = time.perf_counter()
        request_id = request.get("request_id") or uuid.uuid4().hex
        tenant = str(request.get("tenant_id") or "").strip()
        if not tenant:
            raise BusinessError(
                422, "TENANT_REQUIRED", "tenant_id is required", field="tenant_id"
            )
        client_value = request.get("client_id")
        client_id = str(client_value).strip() if client_value not in (None, "") else None
        protocol_version = str(request.get("protocol_version") or "").strip()
        if not protocol_version:
            raise BusinessError(
                422,
                "PROTOCOL_REQUIRED",
                "protocol_version is required",
                field="protocol_version",
            )
        decision = self.routing.route(tenant, client_id)
        version = decision.version
        fallback_from: str | None = None

        try:
            validation = self.validate_request(request, protocol_version, version, tenant)
        except BusinessError as exc:
            elapsed_ms = (time.perf_counter() - started) * 1000.0
            self.metrics.record(
                version,
                tenant,
                elapsed_ms,
                error=True,
                timeout=False,
                error_code=exc.code,
            )
            raise

        if not await self.registry.wait_ready(
            version, self.config.loading.ready_timeout_ms
        ):
            fallback = self.config.stable_version
            if fallback != version and await self.registry.wait_ready(
                fallback, self.config.loading.ready_timeout_ms
            ):
                fallback_slot = self.registry.get_slot(fallback)
                if fallback_slot.protocol.version != protocol_version:
                    elapsed_ms = (time.perf_counter() - started) * 1000.0
                    self.metrics.record(
                        fallback,
                        tenant,
                        elapsed_ms,
                        error=True,
                        timeout=False,
                        error_code="MODEL_PROTOCOL_MISMATCH",
                    )
                    raise BusinessError(
                        422,
                        "MODEL_PROTOCOL_MISMATCH",
                        "the ready fallback model does not accept the declared feature protocol",
                        field="protocol_version",
                        details={
                            "declared_protocol": protocol_version,
                            "model_protocol": fallback_slot.protocol.version,
                            "requested_version": version,
                            "fallback_version": fallback,
                        },
                        version=fallback,
                        tenant=tenant,
                    )
                fallback_from = version
                version = fallback
            else:
                elapsed_ms = (time.perf_counter() - started) * 1000.0
                self.metrics.record(
                    version,
                    tenant,
                    elapsed_ms,
                    error=True,
                    timeout=False,
                    error_code="MODEL_NOT_READY",
                )
                raise BusinessError(
                    409,
                    "MODEL_NOT_READY",
                    "no compatible model version became ready before the timeout",
                    details={
                        "requested_version": version,
                        "ready_timeout_ms": self.config.loading.ready_timeout_ms,
                    },
                    version=version,
                    tenant=tenant,
                )

        timeout_seconds = self.config.monitoring.request_timeout_ms / 1000.0
        try:
            score, injected_delay_ms = await asyncio.wait_for(
                asyncio.to_thread(
                    self.registry.infer, version, validation.features
                ),
                timeout=timeout_seconds,
            )
        except asyncio.TimeoutError as exc:
            elapsed_ms = (time.perf_counter() - started) * 1000.0
            self.metrics.record(
                version,
                tenant,
                elapsed_ms,
                error=True,
                timeout=True,
                error_code="INFERENCE_TIMEOUT",
            )
            raise BusinessError(
                504,
                "INFERENCE_TIMEOUT",
                "model inference exceeded the configured timeout",
                details={"timeout_ms": self.config.monitoring.request_timeout_ms},
                version=version,
                tenant=tenant,
            ) from exc
        except Exception as exc:
            elapsed_ms = (time.perf_counter() - started) * 1000.0
            self.metrics.record(
                version,
                tenant,
                elapsed_ms,
                error=True,
                timeout=False,
                error_code="INFERENCE_FAILED",
            )
            LOGGER.exception(
                json.dumps(
                    {
                        "event": "inference_failed",
                        "request_id": request_id,
                        "tenant": tenant,
                        "version": version,
                    },
                    sort_keys=True,
                )
            )
            raise BusinessError(
                500,
                "INFERENCE_FAILED",
                "model inference failed",
                details={"reason": type(exc).__name__},
                version=version,
                tenant=tenant,
            ) from exc

        elapsed_ms = (time.perf_counter() - started) * 1000.0
        self.metrics.record(
            version,
            tenant,
            elapsed_ms,
            error=False,
            timeout=False,
        )
        response = {
            "request_id": request_id,
            "tenant_id": tenant,
            "client_id": client_id,
            "model_version": version,
            "model_id": self.registry.get_slot(version).model_id,
            "protocol_version": protocol_version,
            "score": round(score, 8),
            "latency_ms": round(elapsed_ms, 3),
            "routing": {
                "bucket": decision.bucket,
                "scope": decision.scope,
                "weights": decision.weights,
            },
        }
        if fallback_from:
            response["fallback_from"] = fallback_from
        return response

    def validate_request(
        self,
        request: dict[str, Any],
        protocol_version: str,
        selected_version: str,
        tenant: str,
    ) -> ValidationResult:
        protocol = self.config.protocols.get(protocol_version)
        if protocol is None:
            raise BusinessError(
                422,
                "UNSUPPORTED_PROTOCOL",
                "declared feature protocol is not registered",
                field="protocol_version",
                details={
                    "declared_protocol": protocol_version,
                    "supported_protocols": sorted(self.config.protocols),
                },
                version=selected_version,
                tenant=tenant,
            )

        features_value = request.get("features")
        names_value = request.get("feature_names")
        if isinstance(features_value, dict):
            dict_names = list(features_value.keys())
            dict_values = list(features_value.values())
            if names_value is None:
                names_value = dict_names
            features_value = dict_values
        if not isinstance(features_value, list):
            raise BusinessError(
                422,
                "FEATURES_REQUIRED",
                "features must be an ordered JSON array or object",
                field="features",
                version=selected_version,
                tenant=tenant,
            )
        if not isinstance(names_value, list):
            raise BusinessError(
                422,
                "FEATURE_NAMES_REQUIRED",
                "feature_names is required so field order can be verified",
                field="feature_names",
                version=selected_version,
                tenant=tenant,
            )
        names = [str(item) for item in names_value]
        expected = protocol.feature_names
        if len(features_value) != len(expected):
            raise BusinessError(
                422,
                "FEATURE_COUNT_MISMATCH",
                "feature count does not match the declared protocol",
                field="features",
                details={
                    "declared_protocol": protocol_version,
                    "expected_count": len(expected),
                    "actual_count": len(features_value),
                },
                version=selected_version,
                tenant=tenant,
            )
        if len(names) != len(expected):
            raise BusinessError(
                422,
                "FEATURE_NAMES_COUNT_MISMATCH",
                "feature_names count does not match the declared protocol",
                field="feature_names",
                details={
                    "declared_protocol": protocol_version,
                    "expected_count": len(expected),
                    "actual_count": len(names),
                },
                version=selected_version,
                tenant=tenant,
            )
        for index, (actual_name, expected_name) in enumerate(zip(names, expected)):
            if actual_name != expected_name:
                raise BusinessError(
                    422,
                    "FEATURE_ORDER_MISMATCH",
                    "feature field order does not match the declared protocol",
                    field=f"feature_names[{index}]",
                    details={
                        "declared_protocol": protocol_version,
                        "index": index,
                        "expected": expected_name,
                        "actual": actual_name,
                    },
                    version=selected_version,
                    tenant=tenant,
                )
        values: list[float] = []
        for index, raw_value in enumerate(features_value):
            if isinstance(raw_value, bool):
                raise BusinessError(
                    422,
                    "FEATURE_VALUE_INVALID",
                    "feature values must be finite numbers",
                    field=f"features[{index}]",
                    version=selected_version,
                    tenant=tenant,
                )
            try:
                value = float(raw_value)
            except (TypeError, ValueError) as exc:
                raise BusinessError(
                    422,
                    "FEATURE_VALUE_INVALID",
                    "feature values must be finite numbers",
                    field=f"features[{index}]",
                    details={"actual": repr(raw_value)},
                    version=selected_version,
                    tenant=tenant,
                ) from exc
            if not math.isfinite(value):
                raise BusinessError(
                    422,
                    "FEATURE_VALUE_INVALID",
                    "feature values must be finite numbers",
                    field=f"features[{index}]",
                    version=selected_version,
                    tenant=tenant,
                )
            values.append(value)

        model_protocol = self.config.models[selected_version].protocol_version
        if model_protocol != protocol_version:
            raise BusinessError(
                422,
                "MODEL_PROTOCOL_MISMATCH",
                "declared feature protocol is incompatible with the selected model",
                field="protocol_version",
                details={
                    "declared_protocol": protocol_version,
                    "model_protocol": model_protocol,
                    "model_version": selected_version,
                },
                version=selected_version,
                tenant=tenant,
            )
        return ValidationResult(feature_names=tuple(names), features=tuple(values))

    def state(self) -> dict[str, Any]:
        rollbacks, active = self.metrics.rollback_counts()
        routing_scopes = self.routing.effective_map()
        current_weights = routing_scopes["default"]
        versions = self.registry.state_map()
        for version, details in versions.items():
            details["load_status"] = details["load_state"]
            details["current_weight"] = current_weights.get(version, 0.0)
        return {
            "service": "ranking-gateway",
            "started_at": round(self.started_at, 6),
            "stable_version": self.config.stable_version,
            "versions": versions,
            "routing": {
                "weights": current_weights,
                "configured_weights": self.routing.default_weights(),
                "buckets": self.config.routing.bucket_count,
                "scopes": routing_scopes,
            },
            "rollback": {
                "count": rollbacks,
                "active_versions": sorted(active),
                "recent": self.rollback_monitor.records()[-20:],
            },
            "metrics": {
                "window_seconds": self.config.monitoring.window_seconds,
                "min_samples": self.config.monitoring.min_samples,
                "p99_latency_threshold_ms": self.config.monitoring.p99_latency_ms,
                "error_rate_threshold": self.config.monitoring.error_rate,
                "timeout_ms": self.config.monitoring.request_timeout_ms,
                "per_version": {
                    version: self.metrics.all_time_summary(version)
                    for version in self.config.models
                },
                "per_version_tenant": self.metrics.all_time_tenant_summaries(),
                "error_codes": self.metrics.error_codes(),
            },
        }

    def prometheus(self) -> str:
        rollbacks, _active = self.metrics.rollback_counts()
        del rollbacks
        return self.metrics.render_prometheus(
            weights=self.routing.effective_map()["default"],
            ready=self.registry.ready_map(),
            rollback_active={
                version: version in self.routing.disabled()
                for version in self.config.models
            },
        )
