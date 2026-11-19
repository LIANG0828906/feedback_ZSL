from __future__ import annotations

import asyncio
import logging
import os
import time
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any, AsyncIterator

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, PlainTextResponse

from .config import AppConfig, load_config
from .core import BusinessError, GatewayService

LOGGER = logging.getLogger("ranking_gateway")


_REQUEST_ALIASES = {
    "tenant_id": ("tenant", "tenantId"),
    "client_id": ("client", "clientId"),
    "protocol_version": (
        "feature_protocol_version",
        "feature_protocol",
        "protocol",
    ),
    "feature_names": ("feature_order", "features_order", "field_names"),
}


def _normalize_predict_body(body: dict[str, Any]) -> dict[str, Any]:
    normalized = dict(body)
    for canonical, aliases in _REQUEST_ALIASES.items():
        if canonical in normalized and normalized[canonical] is not None:
            continue
        for alias in aliases:
            if alias in normalized and normalized[alias] is not None:
                normalized[canonical] = normalized[alias]
                break
    return normalized


def _configure_logging(level: str) -> None:
    if logging.getLogger().handlers:
        return
    logging.basicConfig(
        level=getattr(logging, level.upper(), logging.INFO),
        format="%(asctime)s %(levelname)s %(message)s",
    )


def create_app(config_path: str | Path = "config.json") -> FastAPI:
    config: AppConfig = load_config(config_path)
    _configure_logging(str(config.service.get("log_level", "INFO")))

    @asynccontextmanager
    async def lifespan(app: FastAPI) -> AsyncIterator[None]:
        service = GatewayService(config)
        app.state.gateway = service
        service.start()
        try:
            yield
        finally:
            service.stop()

    app = FastAPI(
        title="Ranking Model Canary Gateway",
        version="1.0.0",
        lifespan=lifespan,
    )

    def service(request: Request) -> GatewayService:
        return request.app.state.gateway

    @app.exception_handler(BusinessError)
    async def business_error_handler(
        _request: Request, exc: BusinessError
    ) -> JSONResponse:
        request_id = getattr(exc, "request_id", None) or ""
        if not request_id:
            request_id = getattr(exc, "request_id", "")
        return JSONResponse(
            status_code=exc.status_code,
            content=exc.body(request_id),
            headers={"X-Error-Code": exc.code},
        )

    @app.exception_handler(RequestValidationError)
    async def validation_error_handler(
        _request: Request, exc: RequestValidationError
    ) -> JSONResponse:
        first = exc.errors()[0] if exc.errors() else {}
        location = [str(item) for item in first.get("loc", ()) if item != "body"]
        field = ".".join(location) if location else None
        return JSONResponse(
            status_code=422,
            content={
                "error": {
                    "code": "INVALID_REQUEST",
                    "message": first.get("msg", "request validation failed"),
                    "field": field,
                }
            },
            headers={"X-Error-Code": "INVALID_REQUEST"},
        )

    @app.exception_handler(Exception)
    async def unhandled_error_handler(_request: Request, exc: Exception) -> JSONResponse:
        LOGGER.exception("unhandled gateway error")
        return JSONResponse(
            status_code=500,
            content={
                "error": {
                    "code": "INTERNAL_ERROR",
                    "message": "unexpected gateway error",
                    "reason": type(exc).__name__,
                }
            },
            headers={"X-Error-Code": "INTERNAL_ERROR"},
        )

    @app.get("/")
    async def root() -> dict[str, Any]:
        return {
            "service": "ranking-gateway",
            "predict": "POST /predict",
            "state": "GET /state",
            "metrics": "GET /metrics",
        }

    @app.get("/healthz")
    async def healthz(request: Request) -> dict[str, Any]:
        gateway = service(request)
        ready = gateway.registry.ready_map()
        return {"status": "ok", "ready_versions": [v for v, value in ready.items() if value]}

    @app.get("/readyz")
    async def readyz(request: Request) -> JSONResponse:
        gateway = service(request)
        ready = gateway.registry.ready_map()
        payload = {"ready": any(ready.values()), "versions": ready}
        return JSONResponse(status_code=200 if any(ready.values()) else 409, content=payload)

    @app.post("/predict")
    @app.post("/v1/predict")
    async def predict(request: Request) -> dict[str, Any]:
        gateway = service(request)
        try:
            raw_body = await request.json()
        except Exception as exc:
            raise BusinessError(
                422,
                "INVALID_JSON",
                "request body must be a JSON object",
                field="body",
            ) from exc
        if not isinstance(raw_body, dict):
            raise BusinessError(
                422,
                "INVALID_REQUEST",
                "request body must be a JSON object",
                field="body",
            )
        body = _normalize_predict_body(raw_body)
        header_aliases = {
            "tenant_id": ("x-tenant-id",),
            "client_id": ("x-client-id",),
            "protocol_version": (
                "x-feature-protocol-version",
                "x-feature-protocol",
            ),
        }
        for canonical, headers in header_aliases.items():
            if body.get(canonical) not in (None, ""):
                continue
            for header in headers:
                value = request.headers.get(header)
                if value:
                    body[canonical] = value
                    break
        request_id = str(body.get("request_id") or "")
        try:
            result = await gateway.predict(body)
        except BusinessError as exc:
            exc.request_id = request_id
            raise
        return result

    @app.get("/state")
    @app.get("/admin/state")
    @app.get("/status")
    @app.get("/admin/status")
    async def state(request: Request) -> dict[str, Any]:
        return service(request).state()

    @app.get("/metrics")
    @app.get("/admin/metrics")
    @app.get("/prometheus")
    async def metrics(request: Request) -> PlainTextResponse:
        return PlainTextResponse(
            service(request).prometheus(),
            media_type="text/plain; version=0.0.4; charset=utf-8",
        )

    @app.post("/admin/weights")
    @app.post("/admin/routing")
    async def update_weights(payload: dict[str, Any], request: Request) -> dict[str, Any]:
        gateway = service(request)
        weights = payload.get("weights") or payload.get("version_weights")
        if weights is None:
            weights = {
                key: value
                for key, value in payload.items()
                if key.endswith("_weight")
                or key.startswith("weight_")
                or key in gateway.config.models
            }
            weights = {
                (
                    key.removesuffix("_weight")
                    if key.endswith("_weight")
                    else key.removeprefix("weight_")
                ): value
                for key, value in weights.items()
            }
        if not isinstance(weights, dict) or not weights:
            raise BusinessError(
                400,
                "WEIGHTS_REQUIRED",
                "weights must be a non-empty object keyed by model version",
                field="weights",
            )
        try:
            parsed = {str(version): float(value) for version, value in weights.items()}
            effective = gateway.routing.update(
                parsed,
                tenant=payload.get("tenant_id"),
                client_id=payload.get("client_id"),
                clear_rollback=bool(payload.get("clear_rollback", False)),
            )
        except (TypeError, ValueError) as exc:
            raise BusinessError(
                400,
                "INVALID_WEIGHTS",
                str(exc),
                field="weights",
            ) from exc
        return {
            "ok": True,
            "updated_at": round(time.time(), 6),
            "scope": (
                "client"
                if payload.get("client_id")
                else "tenant"
                if payload.get("tenant_id")
                else "default"
            ),
            "routing": effective,
        }

    @app.post("/admin/rollback/reset")
    async def reset_rollback(payload: dict[str, Any], request: Request) -> dict[str, Any]:
        gateway = service(request)
        versions = payload.get("versions") or list(gateway.config.models)
        if isinstance(versions, str):
            versions = [versions]
        for version in versions:
            if version not in gateway.config.models:
                raise BusinessError(
                    400,
                    "UNKNOWN_MODEL_VERSION",
                    f"unknown model version {version}",
                    field="versions",
                )
            gateway.routing.enable(str(version))
            gateway.metrics.clear_rollback(str(version))
        return {
            "ok": True,
            "enabled_versions": sorted(versions),
            "routing": gateway.routing.effective_map(),
        }

    @app.post("/admin/reload/{version}")
    async def reload_model(
        version: str, request: Request, wait: bool = True
    ) -> JSONResponse:
        gateway = service(request)
        if version not in gateway.config.models:
            raise BusinessError(
                404,
                "UNKNOWN_MODEL_VERSION",
                f"unknown model version {version}",
                field="version",
            )
        if wait:
            success = await asyncio.to_thread(gateway.registry.reload, version)
        else:
            gateway.registry.reload_async(version)
            success = True
        payload = {
            "ok": success,
            "version": version,
            "state": gateway.registry.state_map()[version],
        }
        if not success:
            payload["error_code"] = "MODEL_LOAD_FAILED"
            return JSONResponse(status_code=409, content=payload)
        return JSONResponse(status_code=200, content=payload)

    @app.get("/admin/faults")
    async def get_faults(request: Request) -> dict[str, Any]:
        gateway = service(request)
        return {
            "enabled": gateway.config.fault_injection_enabled,
            "faults": gateway.registry.faults(),
        }

    @app.post("/admin/faults")
    async def set_faults(payload: dict[str, Any], request: Request) -> dict[str, Any]:
        gateway = service(request)
        if not gateway.config.fault_injection_enabled:
            raise BusinessError(
                403,
                "FAULT_INJECTION_DISABLED",
                "fault injection is disabled by configuration",
            )
        unknown_targets: list[str] = []
        delays = payload.get("inference_delay_ms", {})
        if not isinstance(delays, dict):
            raise BusinessError(
                400,
                "INVALID_FAULT",
                "inference_delay_ms must be an object keyed by model version",
                field="inference_delay_ms",
            )
        for version, delay in delays.items():
            if version not in gateway.config.models:
                unknown_targets.append(str(version))
                continue
            try:
                delay_value = float(delay)
            except (TypeError, ValueError) as exc:
                raise BusinessError(
                    400,
                    "INVALID_FAULT",
                    "inference delay must be a number",
                    field=f"inference_delay_ms.{version}",
                ) from exc
            if delay_value < 0:
                raise BusinessError(
                    400,
                    "INVALID_FAULT",
                    "inference delay must be non-negative",
                    field=f"inference_delay_ms.{version}",
                )
            gateway.registry.set_inference_delay(str(version), delay_value)
        failed_versions = payload.get("fail_next_load", [])
        if isinstance(failed_versions, str):
            failed_versions = [failed_versions]
        if not isinstance(failed_versions, list):
            raise BusinessError(
                400,
                "INVALID_FAULT",
                "fail_next_load must be an array of model versions",
                field="fail_next_load",
            )
        for version in failed_versions:
            if version not in gateway.config.models:
                unknown_targets.append(str(version))
                continue
            gateway.registry.queue_load_failure(str(version))
        if payload.get("clear"):
            gateway.registry.clear_faults()
        if unknown_targets:
            raise BusinessError(
                400,
                "UNKNOWN_MODEL_VERSION",
                "fault injection references unknown model versions",
                field="inference_delay_ms",
                details={"unknown_versions": sorted(set(unknown_targets))},
            )
        return {"ok": True, "faults": gateway.registry.faults()}

    @app.post("/admin/evaluate")
    async def evaluate_now(request: Request) -> dict[str, Any]:
        gateway = service(request)
        records = gateway.rollback_monitor.evaluate_once()
        return {"ok": True, "new_rollbacks": records}

    return app


app = create_app(
    os.getenv(
        "RANKING_GATEWAY_CONFIG",
        str(Path(__file__).resolve().parent.parent / "config.json"),
    )
)
