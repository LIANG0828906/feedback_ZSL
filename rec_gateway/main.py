from __future__ import annotations

import json
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any, AsyncIterator

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, Response
from pydantic import ValidationError

from rec_gateway.config import GatewayConfig
from rec_gateway.errors import BusinessError
from rec_gateway.metrics import GatewayMetrics
from rec_gateway.models import ModelManager, utc_now
from rec_gateway.routing import Router
from rec_gateway.schemas import BatchRequest, ReloadRequest, WeightUpdateRequest
from rec_gateway.service import GatewayService
from rec_gateway.stats import StatsManager
from rec_gateway.store import Store


def _sorted_json(value: Any, status_code: int = 200) -> Response:
    return Response(
        content=json.dumps(
            value,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ),
        media_type="application/json",
        status_code=status_code,
    )


def _business_error_response(error: BusinessError) -> Response:
    return _sorted_json(
        {
            "ok": False,
            "error": error.as_dict(),
        },
        status_code=error.http_status or 400,
    )


def create_app(
    config: GatewayConfig | None = None,
    config_path: str | Path | None = None,
) -> FastAPI:
    resolved_config = config or GatewayConfig.load(config_path)
    store = Store(resolved_config.database)
    default_weights = {
        tenant_id: tenant.canary_weight
        for tenant_id, tenant in resolved_config.tenants.items()
    }
    stored_weights = store.ensure_tenants(default_weights, utc_now())
    router = Router(
        resolved_config.stable_version,
        resolved_config.canary_version,
        stored_weights,
    )
    models = ModelManager(
        resolved_config.model_dir,
        (resolved_config.stable_version, resolved_config.canary_version),
    )
    versions = (resolved_config.stable_version, resolved_config.canary_version)
    tenants = tuple(sorted(resolved_config.tenants))
    metrics = GatewayMetrics(versions, tenants)
    stats = StatsManager(versions, tenants, {version: 2000 for version in versions})
    service = GatewayService(
        resolved_config,
        store,
        models,
        router,
        metrics,
        stats,
    )

    @asynccontextmanager
    async def lifespan(_: FastAPI) -> AsyncIterator[None]:
        service.start()
        try:
            yield
        finally:
            service.close()

    app = FastAPI(
        title="Recommendation Migration Gateway",
        version="1.0.0",
        docs_url="/docs",
        redoc_url=None,
        lifespan=lifespan,
    )
    app.state.service = service
    app.state.config = resolved_config
    app.state.store = store
    app.state.models = models
    app.state.router = router
    app.state.metrics = metrics

    @app.exception_handler(BusinessError)
    async def handle_business_error(
        _: Request, exc: BusinessError
    ) -> Response:
        return _business_error_response(exc)

    @app.exception_handler(RequestValidationError)
    async def handle_request_validation(
        _: Request, exc: RequestValidationError
    ) -> Response:
        errors = exc.errors()
        first = errors[0] if errors else {}
        location = ".".join(str(part) for part in first.get("loc", ()))
        return _business_error_response(
            BusinessError(
                "INVALID_REQUEST",
                "request does not match the API schema",
                {
                    "field": location or None,
                    "validation_error": first.get("type", "invalid"),
                },
            )
        )

    @app.exception_handler(Exception)
    async def handle_unexpected(_: Request, __: Exception) -> Response:
        return _business_error_response(
            BusinessError(
                "INTERNAL_ERROR",
                "gateway could not complete the request",
                {"column_index": None},
            )
        )

    def infer_endpoint(request: dict[str, Any]) -> Response:
        try:
            processed = service.infer_one(request)
        except BusinessError as exc:
            return _business_error_response(exc)
        headers = {
            "X-Request-Id": str(processed.payload.get("request_id", "")),
            "X-Served-Version": str(processed.payload.get("served_version", "")),
            "X-Protocol-Version": str(processed.payload.get("protocol_version", "")),
        }
        if processed.idempotent_replay:
            headers["X-Idempotent-Replay"] = "true"
        return Response(
            content=json.dumps(
                processed.payload,
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
            ),
            media_type="application/json",
            status_code=processed.status_code,
            headers=headers,
        )

    @app.post("/v1/infer")
    @app.post("/infer")
    def infer(request: dict[str, Any]) -> Response:
        return infer_endpoint(request)

    @app.post("/v1/infer/batch")
    @app.post("/batch")
    def infer_batch(body: BatchRequest) -> Response:
        result = service.infer_batch(body.resolved_items())
        return _sorted_json(result)

    @app.get("/status")
    @app.get("/state")
    def status() -> Response:
        return _sorted_json(service.status())

    @app.get("/healthz")
    def healthz() -> Response:
        versions = models.snapshot()
        ready = [
            version
            for version, value in versions.items()
            if value["status"] == "ready"
        ]
        payload = {
            "status": "ok" if ready else "unavailable",
            "ready_versions": ready,
        }
        return _sorted_json(payload, status_code=200 if ready else 503)

    @app.get("/metrics")
    def metrics_endpoint() -> Response:
        return Response(
            content=metrics.render(),
            media_type="text/plain; version=0.0.4; charset=utf-8",
        )

    @app.get("/admin/tenants")
    def tenants_endpoint() -> Response:
        return _sorted_json(
            {
                "tenants": [
                    {
                        "tenant_id": tenant_id,
                        "canary_weight": router.weight_for(tenant_id),
                    }
                    for tenant_id in tenants
                ],
                "stable_version": resolved_config.stable_version,
                "canary_version": resolved_config.canary_version,
            }
        )

    @app.put("/admin/tenants/{tenant_id}/weights")
    @app.put("/admin/tenants/{tenant_id}/weight")
    @app.put("/admin/routing/{tenant_id}")
    def update_weight(
        tenant_id: str, body: WeightUpdateRequest
    ) -> Response:
        result = service.update_weight(tenant_id, body.canary_weight)
        return _sorted_json(result)

    @app.get("/admin/route/preview")
    def route_preview(
        tenant_id: str, client_id: str | None = None
    ) -> Response:
        return _sorted_json(router.preview(tenant_id, client_id))

    @app.get("/admin/route-audit")
    def route_audit(
        tenant_id: str | None = None,
        client_id: str | None = None,
        limit: int = 50,
    ) -> Response:
        return _sorted_json(service.route_history(tenant_id, client_id, limit))

    def reload_version(version: str) -> Response:
        if version not in versions:
            raise BusinessError(
                "UNKNOWN_VERSION",
                "unknown model version",
                {"version": version},
            )
        accepted = models.reload_async(version)
        if not accepted:
            raise BusinessError(
                "MODEL_RELOAD_IN_PROGRESS",
                "model version is already loading",
                {"version": version},
            )
        return _sorted_json(
            {
                "accepted": True,
                "version": version,
                "load_status": models.get_state(version),
            },
            status_code=202,
        )

    @app.post("/admin/models/{version}/reload")
    def reload_model(version: str) -> Response:
        return reload_version(version)

    @app.post("/admin/models/reload")
    def reload_model_body(body: ReloadRequest) -> Response:
        return reload_version(body.version)

    @app.post("/admin/reload")
    def reload_model_alias(body: ReloadRequest) -> Response:
        return reload_version(body.version)

    return app


def app_factory() -> FastAPI:
    return create_app()
