from __future__ import annotations

import hmac
import logging
import math
import os
import time
from contextlib import asynccontextmanager
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, AsyncIterator

from anyio import to_thread
from fastapi import Body, FastAPI, Request
from fastapi.responses import JSONResponse, PlainTextResponse, Response
from pydantic import BaseModel, Field
from starlette.middleware.base import BaseHTTPMiddleware

from .audit import AuditWriter
from .capacity_report import build_capacity_report
from .circuit import CircuitBreaker
from .config import Config, load_config
from .control import ControlPlane
from .database import Database
from .fallback import QueryCache, SlimSnapshot
from .faults import FaultController
from .limiter import TenantLimiterRegistry, TenantLimits
from .metrics import Metrics
from .monitor import CapacityMonitor, SLIMonitor, StopToken
from .runtime import RuntimeStats
from .service import OrderService, QueryResult, ServiceError
from .sli import SLIRegistry

LOGGER = logging.getLogger("order_guard.app")


def _utc_run_id() -> str:
    return "run-" + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + f"-{os.getpid()}"


def _json_error(
    *,
    status_code: int,
    business_code: str,
    message: str,
    retry_after_seconds: int | None = None,
    details: dict[str, Any] | None = None,
) -> JSONResponse:
    headers = {"X-Business-Code": business_code}
    if retry_after_seconds is not None:
        headers["Retry-After"] = str(max(1, retry_after_seconds))
    return JSONResponse(
        status_code=status_code,
        content={
            "business_code": business_code,
            "message": message,
            "details": details or {},
        },
        headers=headers,
    )


class CreateOrderBody(BaseModel):
    amount_cents: int = Field(gt=0)


class UpdateStatusBody(BaseModel):
    status: str


class AdmissionMiddleware(BaseHTTPMiddleware):
    def __init__(self, app, runtime: "ApplicationRuntime") -> None:
        super().__init__(app)
        self.runtime = runtime

    @staticmethod
    def _is_order_api(path: str, method: str) -> bool:
        return path.startswith("/api/v1/orders") and method in {
            "GET",
            "POST",
            "PATCH",
        }

    async def dispatch(self, request: Request, call_next):
        path = request.url.path
        if not self._is_order_api(path, request.method):
            return await call_next(request)
        tenant_id = (request.headers.get("x-tenant-id") or "").strip()
        if not tenant_id:
            return _json_error(
                status_code=400,
                business_code="TENANT_REQUIRED",
                message="X-Tenant-Id header is required for business requests",
            )
        admission = self.runtime.limiters.acquire(tenant_id)
        if not admission.allowed:
            reason = admission.reason or "admission_rejected"
            self.runtime.metrics.inc(
                "order_guard_rate_limit_rejections_total",
                {"tenant": tenant_id, "reason": reason},
            )
            if reason == "tenant_not_active":
                return _json_error(
                    status_code=403,
                    business_code="TENANT_NOT_ACTIVE",
                    message="tenant is not configured as active",
                    details={"tenant_id": tenant_id},
                )
            retry_ms = max(1, admission.retry_after_ms)
            return _json_error(
                status_code=429,
                business_code=(
                    "TENANT_RATE_LIMITED"
                    if reason == "rate"
                    else "TENANT_CONCURRENCY_LIMITED"
                ),
                message="tenant admission limit exceeded",
                retry_after_seconds=max(1, math.ceil(retry_ms / 1000)),
                details={
                    "tenant_id": tenant_id,
                    "reason": reason,
                    "retry_after_ms": retry_ms,
                    "inflight": admission.inflight,
                    "max_inflight": (
                        admission.limit.max_inflight if admission.limit else None
                    ),
                    "rate_per_second": (
                        admission.limit.rate_per_second
                        if admission.limit
                        else None
                    ),
                },
            )

        self.runtime.runtime_stats.enter(tenant_id)
        started = time.monotonic()
        response: Response | None = None
        try:
            response = await call_next(request)
            if response.status_code >= 400:
                self.runtime.logger.info(
                    "business response status=%s tenant=%s path=%s",
                    response.status_code,
                    tenant_id,
                    path,
                )
            return response
        except Exception:
            if request.method == "GET":
                latency_ms = (time.monotonic() - started) * 1000.0
                self.runtime.sli_registry.record_query(
                    success=False, latency_ms=latency_ms
                )
            raise
        finally:
            if request.method == "GET":
                latency_ms = (time.monotonic() - started) * 1000.0
                success = response is not None and 200 <= response.status_code < 400
                if response is None:
                    success = False
                # Exceptions are sampled above; this only samples completed responses.
                if response is not None:
                    self.runtime.sli_registry.record_query(
                        success=success, latency_ms=latency_ms
                    )
            self.runtime.limiters.release(tenant_id)
            self.runtime.runtime_stats.leave(tenant_id)


class ApplicationRuntime:
    def __init__(self, config: Config) -> None:
        self.config = config
        self.logger = LOGGER
        self.run_id = (
            config.audit.run_id
            or os.environ.get("ORDER_GUARD_RUN_ID")
            or _utc_run_id()
        )
        self.metrics = Metrics()
        self.metrics.declare_defaults(tenant.tenant_id for tenant in config.tenants)
        self.faults = FaultController()
        self.audit = AuditWriter(config.audit.path, self.run_id)
        self.database = Database(config, self.metrics, self.faults)
        self.sli_registry = SLIRegistry(config.slis)
        self.limiters = TenantLimiterRegistry(list(config.tenants))
        self.runtime_stats = RuntimeStats(
            self.metrics, config.capacity.worker_threads
        )
        self.cache = QueryCache(config)
        self.snapshot = SlimSnapshot(config, self.database)
        try:
            self.snapshot.load()
        except Exception:
            LOGGER.exception("initial slim snapshot load failed")
        self.breaker = CircuitBreaker(config.circuit)
        self.control = ControlPlane(
            config,
            self.audit,
            self.metrics,
            metrics_snapshot=self.metrics_snapshot,
            sli_snapshot=self.sli_registry.to_dict,
        )
        self.control.set_limiter_lookup(self.limiters.current_limits)
        self.service = OrderService(
            config,
            self.database,
            self.cache,
            self.snapshot,
            self.breaker,
            self.control,
            self.metrics,
        )
        self.stop_token = StopToken()
        self.sli_monitor = SLIMonitor(
            config,
            self.sli_registry,
            self.control,
            self.metrics,
            self.stop_token,
        )
        self.capacity_monitor = CapacityMonitor(
            config,
            self.control,
            self.metrics,
            self.runtime_stats,
            self.stop_token,
        )
        self.started_at = time.time()

    def metrics_snapshot(self) -> dict[str, object]:
        active = int(
            self.metrics.get_gauge("order_guard_active_queries") or 0
        )
        total = self.runtime_stats.total()
        return {
            "inflight_total": total,
            "inflight_peak": self.runtime_stats.peak(),
            "active_queries": active,
            "backlog_depth": max(0, total - active),
            "backlog_peak": self.runtime_stats.peak_queue(),
            "worker_thread_occupancy_ratio": round(
                min(1.0, total / max(1, self.config.capacity.worker_threads)),
                6,
            ),
            "sqlite_busy_events_total": int(
                self.metrics.get_counter(
                    "order_guard_sqlite_busy_events_total"
                )
            ),
            "rate_limit_rejections_total": int(
                self.metrics.counter_sum(
                    "order_guard_rate_limit_rejections_total"
                )
            ),
            "circuit_state": self.breaker.state.value,
            "fault": self.faults.snapshot().to_dict(),
        }

    def start(self) -> None:
        self.control.start()
        self.sli_monitor.evaluate_once()
        self.sli_monitor.start()
        self.capacity_monitor.start()
        LOGGER.info(
            "service started run_id=%s config=%s", self.run_id, self.config.source_path
        )

    def stop(self) -> None:
        self.stop_token.stop()
        self.sli_monitor.join(timeout=2.0)
        self.capacity_monitor.join(timeout=2.0)

    def capacity_report(self, scenario: str | None = None) -> dict[str, Any]:
        return build_capacity_report(
            self.config,
            self.metrics,
            self.run_id,
            scenario=scenario,
        ).to_dict()

    def status(self) -> dict[str, Any]:
        return {
            "run_id": self.run_id,
            "started_at": datetime.fromtimestamp(
                self.started_at, tz=timezone.utc
            ).isoformat(timespec="seconds"),
            "uptime_seconds": round(time.time() - self.started_at, 3),
            "protection": self.control.state.to_dict(),
            "circuit": self.breaker.snapshot().__dict__,
            "capacity": {
                "worker_threads": self.config.capacity.worker_threads,
                "page_size_cap": self.control.page_size_cap,
                "protected": self.control.state.capacity_protected,
            },
            "fault": self.faults.snapshot().to_dict(),
            "slis": self.sli_registry.to_dict(),
            "sli_peaks": {
                name: {
                    "peak_value": self.metrics.get_gauge(
                        "order_guard_sli_peak_value", {"sli": name}
                    ),
                    "peak_error_budget_consumed": self.metrics.get_gauge(
                        "order_guard_error_budget_peak_consumed_ratio",
                        {"sli": name},
                    ),
                }
                for name in self.config.slis
            },
            "tenants": self.limiters.snapshots(),
            "metrics": self.metrics_snapshot(),
        }


def _service_error_response(exc: ServiceError) -> JSONResponse:
    return _json_error(
        status_code=exc.status_code,
        business_code=exc.business_code,
        message=exc.message,
        retry_after_seconds=exc.retry_after_seconds,
        details=exc.details,
    )


def create_app(config: Config | str | Path | None = None) -> FastAPI:
    if config is None:
        config = os.environ.get(
            "ORDER_GUARD_CONFIG", "config/service.json"
        )
    if isinstance(config, (str, Path)):
        config = load_config(config)
    runtime = ApplicationRuntime(config)

    @asynccontextmanager
    async def lifespan(_: FastAPI) -> AsyncIterator[None]:
        limiter = to_thread.current_default_thread_limiter()
        limiter.total_tokens = config.service.business_threads
        runtime.start()
        try:
            yield
        finally:
            runtime.stop()

    app = FastAPI(
        title="Order Guard",
        version="1.0.0",
        lifespan=lifespan,
    )
    app.state.runtime = runtime
    app.add_middleware(AdmissionMiddleware, runtime=runtime)

    def require_admin(request: Request) -> None:
        token = request.headers.get("x-admin-token", "")
        if not hmac.compare_digest(token, config.service.admin_token):
            raise ServiceError(
                "ADMIN_UNAUTHORIZED",
                status_code=401,
                message="valid X-Admin-Token is required",
            )

    @app.get("/healthz")
    async def healthz() -> dict[str, Any]:
        state = runtime.control.state
        return {
            "status": "ok",
            "run_id": runtime.run_id,
            "protection_state": state.protection_state,
            "degrade_level": state.degrade_level,
            "capacity_protected": state.capacity_protected,
            "circuit_state": runtime.breaker.state.value,
        }

    @app.get("/readyz")
    async def readyz() -> dict[str, Any]:
        return {
            "status": "ready",
            "run_id": runtime.run_id,
            "degraded": runtime.control.state.degrade_level > 0,
        }

    @app.get("/metrics")
    async def prometheus_metrics() -> PlainTextResponse:
        return PlainTextResponse(
            runtime.metrics.render(),
            media_type="text/plain; version=0.0.4",
        )

    def query_response(result: QueryResult) -> JSONResponse:
        state = runtime.control.state
        headers = {
            "X-Business-Code": result.business_code,
            "X-Protection-Code": state.protection_code,
            "X-Degrade-Level": str(state.degrade_level),
            "X-Tenant-Id": "",
        }
        content: dict[str, Any] = {
            "business_code": result.business_code,
            "protection_code": state.protection_code,
            "degrade_level": state.degrade_level,
            "page": result.page,
            "page_size": result.page_size,
            "page_size_limit": result.page_size_limit,
            "rows": result.rows,
        }
        if result.fallback is not None:
            content["fallback"] = result.fallback
            headers["X-Fallback-Source"] = result.fallback["source"]
        return JSONResponse(
            status_code=result.status_code,
            content=content,
            headers=headers,
        )

    @app.get("/api/v1/orders")
    def list_orders(
        request: Request,
        status: str,
        page: int = 1,
        page_size: int | None = None,
    ) -> JSONResponse:
        tenant_id = request.headers.get("x-tenant-id", "").strip()
        try:
            result = runtime.service.query_orders(
                tenant_id=tenant_id,
                status=status,
                page=page,
                page_size=page_size,
            )
        except ServiceError as exc:
            return _service_error_response(exc)
        response = query_response(result)
        response.headers["X-Tenant-Id"] = tenant_id
        return response

    @app.post("/api/v1/orders")
    def create_order(
        request: Request, body: CreateOrderBody
    ) -> JSONResponse:
        tenant_id = request.headers.get("x-tenant-id", "").strip()
        try:
            order = runtime.service.create_order(
                tenant_id=tenant_id,
                amount_cents=body.amount_cents,
            )
        except ServiceError as exc:
            return _service_error_response(exc)
        state = runtime.control.state
        return JSONResponse(
            status_code=201,
            content={
                "business_code": "OK",
                "protection_code": state.protection_code,
                "order": order,
            },
            headers={
                "X-Business-Code": "OK",
                "X-Protection-Code": state.protection_code,
                "X-Degrade-Level": str(state.degrade_level),
                "X-Tenant-Id": tenant_id,
            },
        )

    @app.patch("/api/v1/orders/{order_id}/status")
    def update_status(
        request: Request,
        order_id: str,
        body: UpdateStatusBody,
    ) -> JSONResponse:
        tenant_id = request.headers.get("x-tenant-id", "").strip()
        try:
            order = runtime.service.update_order_status(
                tenant_id=tenant_id,
                order_id=order_id,
                new_status=body.status,
            )
        except ServiceError as exc:
            return _service_error_response(exc)
        state = runtime.control.state
        return JSONResponse(
            status_code=200,
            content={
                "business_code": "OK",
                "protection_code": state.protection_code,
                "order": order,
            },
            headers={
                "X-Business-Code": "OK",
                "X-Protection-Code": state.protection_code,
                "X-Degrade-Level": str(state.degrade_level),
                "X-Tenant-Id": tenant_id,
            },
        )

    @app.get("/admin/status")
    async def admin_status(request: Request) -> Response:
        try:
            require_admin(request)
        except ServiceError as exc:
            return _service_error_response(exc)
        return JSONResponse(runtime.status())

    @app.get("/admin/slis")
    async def admin_slis(request: Request) -> Response:
        try:
            require_admin(request)
        except ServiceError as exc:
            return _service_error_response(exc)
        return JSONResponse(runtime.sli_registry.to_dict())

    @app.get("/admin/audit")
    async def admin_audit(
        request: Request,
        run_id: str | None = None,
        after_seq: int = 0,
        limit: int = 500,
    ) -> Response:
        try:
            require_admin(request)
        except ServiceError as exc:
            return _service_error_response(exc)
        events = runtime.audit.query(
            run_id=run_id,
            after_seq=after_seq,
            limit=limit,
        )
        return JSONResponse(
            {
                "run_id": run_id or runtime.run_id,
                "events": events,
                "count": len(events),
            }
        )

    @app.get("/admin/capacity-report")
    async def admin_capacity_report(
        request: Request, scenario: str | None = None
    ) -> Response:
        try:
            require_admin(request)
        except ServiceError as exc:
            return _service_error_response(exc)
        return JSONResponse(runtime.capacity_report(scenario))

    @app.post("/admin/faults")
    async def set_fault(
        request: Request, body: dict[str, Any] = Body(...)
    ) -> Response:
        try:
            require_admin(request)
        except ServiceError as exc:
            return _service_error_response(exc)
        allowed = {
            "mode",
            "delay_ms",
            "error_rate",
            "duration_seconds",
        }
        unknown = set(body) - allowed
        if unknown:
            return _json_error(
                status_code=422,
                business_code="INVALID_FAULT",
                message="unsupported fault fields",
                details={"unknown": sorted(unknown)},
            )
        try:
            state = runtime.faults.set(
                str(body.get("mode", "none")),
                delay_ms=int(body.get("delay_ms", 0)),
                error_rate=float(body.get("error_rate", 1.0)),
                duration_seconds=(
                    float(body["duration_seconds"])
                    if body.get("duration_seconds") is not None
                    else None
                ),
            )
        except (TypeError, ValueError) as exc:
            return _json_error(
                status_code=422,
                business_code="INVALID_FAULT",
                message=str(exc),
            )
        return JSONResponse(
            {"business_code": "OK", "fault": state.to_dict()},
            headers={"X-Business-Code": "OK"},
        )

    @app.delete("/admin/faults")
    async def clear_fault(request: Request) -> Response:
        try:
            require_admin(request)
        except ServiceError as exc:
            return _service_error_response(exc)
        return JSONResponse(
            {
                "business_code": "OK",
                "fault": runtime.faults.clear().to_dict(),
            },
            headers={"X-Business-Code": "OK"},
        )

    @app.patch("/admin/tenants/{tenant_id}/limits")
    async def adjust_limits(
        request: Request,
        tenant_id: str,
        body: dict[str, Any] = Body(...),
    ) -> Response:
        try:
            require_admin(request)
        except ServiceError as exc:
            return _service_error_response(exc)
        old_limits = runtime.limiters.current_limits(tenant_id)
        if old_limits is None:
            return _json_error(
                status_code=404,
                business_code="TENANT_NOT_FOUND",
                message="tenant is not configured",
            )
        allowed = {"rate_per_second", "burst", "max_inflight"}
        unknown = set(body) - allowed
        if unknown or not body:
            return _json_error(
                status_code=422,
                business_code="INVALID_LIMIT_ADJUSTMENT",
                message="provide at least one supported limit field",
                details={"unknown": sorted(unknown)},
            )
        try:
            rate = float(body.get("rate_per_second", old_limits.rate_per_second))
            burst = int(body.get("burst", old_limits.burst))
            max_inflight = int(
                body.get("max_inflight", old_limits.max_inflight)
            )
        except (TypeError, ValueError) as exc:
            return _json_error(
                status_code=422,
                business_code="INVALID_LIMIT_ADJUSTMENT",
                message=str(exc),
            )
        if rate <= 0 or burst < 1 or max_inflight < 1:
            return _json_error(
                status_code=422,
                business_code="INVALID_LIMIT_ADJUSTMENT",
                message="rate_per_second must be > 0; burst and max_inflight must be >= 1",
            )
        new_limits = TenantLimits(
            rate_per_second=rate,
            burst=burst,
            max_inflight=max_inflight,
        )

        def apply() -> None:
            runtime.limiters.adjust(
                tenant_id,
                rate_per_second=rate,
                burst=burst,
                max_inflight=max_inflight,
            )

        runtime.control.adjust_tenant_limits(
            tenant_id=tenant_id,
            old_limits=old_limits,
            new_limits=new_limits,
            apply=apply,
        )
        return JSONResponse(
            {
                "business_code": "RATE_LIMIT_ADJUSTED",
                "tenant_id": tenant_id,
                "previous": old_limits.__dict__,
                "current": new_limits.__dict__,
            },
            headers={"X-Business-Code": "RATE_LIMIT_ADJUSTED"},
        )

    return app
