"""FastAPI application wiring routing, validation, queues and observability."""

import asyncio
import time
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Annotated

from fastapi import Body, FastAPI, Request
from fastapi.responses import JSONResponse, PlainTextResponse, Response

from .config import GatewayConfig, load_config
from .errors import ErrorCode, GatewayError
from .metrics import GatewayMetrics
from .models import STATE_FAILED, STATE_LOADING, STATE_READY, ModelRegistry
from .pipeline import VersionPipeline
from .protocol import load_protocol_mappings
from .router import Router
from .schemas import ReloadRequest, SlowCallRequest, WeightRequest
from .stats import CircuitBreaker, RollingStats


STATE_CODE = {STATE_LOADING: 0, STATE_READY: 1, STATE_FAILED: 2}
TIMEOUT_CODES = {ErrorCode.QUEUE_WAIT_TIMEOUT, ErrorCode.READY_WAIT_TIMEOUT}


class GatewayService:
    """Composition root holding every runtime component."""

    def __init__(self, config: GatewayConfig):
        self.config = config
        base = Path(config.model_dir)
        mapping_path = base / config.mapping_file
        self.specs = load_protocol_mappings(mapping_path)
        self.registry = ModelRegistry(base, self.specs)
        self.router = Router(
            stable_version=config.stable_version,
            canary_version=config.canary_version,
            default_weight=config.default_canary_weight,
            tenant_defaults=config.tenant_default_weights,
        )
        self.pipelines: dict[str, VersionPipeline] = {
            version: VersionPipeline(version, config.version_config(version), self.registry)
            for version in self.specs
        }
        canary_cfg = config.version_config(config.canary_version)
        self.stats = RollingStats(canary_cfg.window_seconds)
        self.breaker = CircuitBreaker(
            canary_version=config.canary_version,
            p99_threshold_ms=canary_cfg.circuit_p99_ms,
            error_rate_threshold=canary_cfg.circuit_error_rate,
            min_samples=canary_cfg.circuit_min_samples,
        )
        self.metrics = GatewayMetrics()
        self._metrics_server = None
        self._startup_error: str | None = None

    async def start(self) -> None:
        for pipeline in self.pipelines.values():
            pipeline.start()
        self._metrics_server, _ = self.metrics.start_server(
            self.config.metrics_port, host=self.config.host
        )
        # Load models concurrently but in isolation; failures are captured per version.
        await asyncio.gather(
            *(self._load_versionsafe(version) for version in self.specs),
            return_exceptions=True,
        )

    async def _load_versionsafe(self, version: str) -> None:
        try:
            await asyncio.to_thread(self.registry.load_blocking, version)
        except Exception as exc:  # noqa: BLE001
            self._startup_error = f"{version}: {exc}"

    async def stop(self) -> None:
        for pipeline in self.pipelines.values():
            await pipeline.stop()
        if self._metrics_server is not None:
            self._metrics_server.shutdown()

    def canary_available(self) -> bool:
        return self.registry.is_ready(self.config.canary_version)

    def publish_gauges(self) -> None:
        for version, pipeline in self.pipelines.items():
            snap = pipeline.metrics.snapshot()
            self.metrics.queue_depth.labels(model_version=version).set(snap["depth"])
            self.metrics.in_flight.labels(model_version=version).set(snap["in_flight"])
            state = self.registry.state_of(version).state
            self.metrics.model_state.labels(model_version=version).set(STATE_CODE[state])
        weights = self.router.weights_snapshot()
        self.metrics.weight.labels(tenant_id="_default").set(weights["default"])
        for tenant, weight in weights["tenants"].items():
            self.metrics.weight.labels(tenant_id=tenant).set(weight)
        self.metrics.circuit_tripped.labels(
            model_version=self.config.canary_version
        ).set(1.0 if self.breaker.tripped else 0.0)

    async def handle_rank(self, payload: dict) -> dict:
        started = time.perf_counter()
        request_id = str(payload.get("request_id") or "")
        tenant_id = str(payload.get("tenant_id") or "")
        client_id = payload.get("client_id")
        target_version = "unrouted"
        error_code = ErrorCode.OK
        is_timeout = False

        try:
            if not request_id or not tenant_id:
                raise GatewayError(
                    ErrorCode.INVALID_REQUEST,
                    "request_id and tenant_id are required",
                    field="request_id" if not request_id else "tenant_id",
                )
            declared = payload.get("protocol_version")
            if not isinstance(declared, str) or not declared:
                raise GatewayError(
                    ErrorCode.INVALID_REQUEST,
                    "protocol_version is required",
                    field="protocol_version",
                )

            decision = self.router.route(
                tenant_id=tenant_id,
                client_id=client_id,
                request_id=request_id,
                canary_available=self.canary_available(),
            )
            target_version = decision["target_version"]

            if not self.registry.is_ready(target_version):
                cfg = self.config.version_config(target_version)
                try:
                    await asyncio.to_thread(
                        self.registry.wait_until_ready,
                        target_version,
                        cfg.ready_wait_ms / 1000.0,
                    )
                except GatewayError:
                    raise

            spec = self.specs[target_version]
            from .protocol import validate_features

            validate_features(
                declared_protocol=declared,
                columns=payload.get("columns"),
                features=payload.get("features"),
                spec=spec,
            )

            pipeline = self.pipelines[target_version]
            score = await pipeline.submit(
                request_id=request_id,
                tenant_id=tenant_id,
                client_id=client_id,
                vector=payload["features"],
            )
            return {
                "code": ErrorCode.OK.value,
                "request_id": request_id,
                "model_version": target_version,
                "protocol_version": spec.version,
                "score": score,
            }
        except GatewayError as exc:
            error_code = exc.code
            is_timeout = exc.code in TIMEOUT_CODES
            raise
        finally:
            latency_ms = (time.perf_counter() - started) * 1000.0
            is_error = error_code != ErrorCode.OK
            if target_version in self.pipelines:
                self.stats.record(
                    version=target_version,
                    tenant_id=tenant_id or "unknown",
                    latency_ms=latency_ms,
                    is_error=is_error,
                    is_timeout=is_timeout,
                )
                labels = dict(
                    model_version=target_version,
                    tenant_id=tenant_id or "unknown",
                )
                self.metrics.requests.labels(
                    status="error" if is_error else "ok", **labels
                ).inc()
                self.metrics.latency.labels(**labels).observe(latency_ms / 1000.0)
                if is_error:
                    self.metrics.errors.labels(
                        code=error_code.value, **labels
                    ).inc()
                if is_timeout:
                    kind = (
                        "ready_wait"
                        if error_code == ErrorCode.READY_WAIT_TIMEOUT
                        else "queue_wait"
                    )
                    self.metrics.timeouts.labels(kind=kind, **labels).inc()
            self._maybe_trip_breaker()
            self.publish_gauges()

    def _maybe_trip_breaker(self) -> None:
        canary = self.config.canary_version
        summary = self.stats.version_summary(canary)
        record = self.breaker.evaluate(summary)
        if record is not None:
            self.router.zero_canary()

    def status_summary(self) -> dict:
        models = self.registry.snapshots()
        version_stats = {
            version: self.stats.version_summary(version)
            for version in sorted(self.pipelines)
        }
        tenant_stats = {
            tenant: self.stats.tenant_summary(tenant)
            for tenant in self.stats.all_tenants()
        }
        queue_stats = {
            version: self.pipelines[version].metrics.snapshot()
            for version in sorted(self.pipelines)
        }
        return {
            "circuit_breaker": {
                "canary_version": self.config.canary_version,
                "last_rollback": self.breaker.record(),
                "tripped": self.breaker.tripped,
            },
            "models": models,
            "queue": queue_stats,
            "routing": self.router.weights_snapshot(),
            "stats": {
                "by_tenant": tenant_stats,
                "by_version": version_stats,
            },
        }


def create_app(config: GatewayConfig | None = None) -> FastAPI:
    config = config or load_config()
    service = GatewayService(config)

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        app.state.service = service
        await service.start()
        yield
        await service.stop()

    app = FastAPI(title="inference-gateway", lifespan=lifespan)
    app.state.service = service

    @app.exception_handler(GatewayError)
    async def gateway_error_handler(request: Request, exc: GatewayError):
        # Deliberately HTTP 200: business errors never surface as 503.
        return JSONResponse(status_code=200, content=exc.to_dict())

    @app.exception_handler(Exception)
    async def unexpected_error_handler(request: Request, exc: Exception):
        return JSONResponse(
            status_code=200,
            content={
                "code": ErrorCode.INTERNAL_ERROR.value,
                "message": f"{type(exc).__name__}: {exc}",
            },
        )

    @app.get("/healthz")
    async def healthz():
        return {"status": "ok"}

    @app.post("/v1/rank")
    async def rank(request: Request):
        payload = await request.json()
        result = await service.handle_rank(payload)
        return JSONResponse(status_code=200, content=result)

    @app.get("/v1/status")
    async def status():
        from fastapi.encoders import jsonable_encoder
        import json as _json

        payload = _json.dumps(
            jsonable_encoder(service.status_summary()), sort_keys=True
        )
        return Response(
            content=payload,
            media_type="application/json",
            headers={"Cache-Control": "no-store"},
        )

    @app.get("/v1/routing/trace")
    async def routing_trace(client_id: str):
        return {"client_id": client_id, "decisions": service.router.client_trace(client_id)}

    @app.post("/v1/admin/weight")
    async def set_weight(body: Annotated[WeightRequest, Body(...)]):
        if body.tenant_id is None:
            service.router.set_default_weight(body.weight)
        else:
            service.router.set_tenant_weight(body.tenant_id, body.weight)
        if body.reset_circuit_breaker:
            service.breaker.reset()
            service.stats.reset()
        service.publish_gauges()
        return {"code": ErrorCode.OK.value, "routing": service.router.weights_snapshot()}

    @app.post("/v1/admin/reload")
    async def reload_model(body: Annotated[ReloadRequest, Body(...)]):
        if body.version not in service.specs:
            raise GatewayError(
                ErrorCode.INVALID_REQUEST,
                f"unknown model version '{body.version}'",
                field="version",
            )
        refreshed = load_protocol_mappings(
            Path(service.config.model_dir) / service.config.mapping_file
        )
        service.specs = refreshed
        service.registry.refresh_specs(refreshed)
        state = await asyncio.to_thread(service.registry.reload, body.version)
        service.breaker.reset()
        return {"code": ErrorCode.OK.value, "model": state.snapshot()}

    @app.post("/v1/admin/slow-calls")
    async def slow_calls(body: Annotated[SlowCallRequest, Body(...)]):
        if body.version not in service.pipelines:
            raise GatewayError(
                ErrorCode.INVALID_REQUEST,
                f"unknown model version '{body.version}'",
                field="version",
            )
        service.pipelines[body.version].inject_slow_calls(body.count, body.delay_ms)
        return {"code": ErrorCode.OK.value}

    @app.get("/metrics")
    async def metrics():
        payload, content_type = service.metrics.render()
        return Response(content=payload, media_type=content_type)

    return app
