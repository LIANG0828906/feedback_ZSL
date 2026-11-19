from __future__ import annotations

import logging
import threading
import time
from dataclasses import dataclass
from typing import Any

from .circuit import CircuitBreaker, CircuitTransition
from .config import Config
from .control import ControlPlane
from .database import Database
from .fallback import QueryCache, SlimSnapshot
from .faults import (
    DatabaseBusy,
    DatabaseFailure,
    DatabaseTimeout,
    DatabaseUnavailable,
)
from .metrics import Metrics

LOGGER = logging.getLogger("order_guard.service")


class ServiceError(Exception):
    def __init__(
        self,
        business_code: str,
        *,
        status_code: int,
        message: str,
        retry_after_seconds: int | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.business_code = business_code
        self.status_code = status_code
        self.message = message
        self.retry_after_seconds = retry_after_seconds
        self.details = details or {}


@dataclass(frozen=True)
class QueryResult:
    rows: list[dict[str, object]]
    page: int
    page_size: int
    page_size_limit: int
    fallback: dict[str, str] | None
    business_code: str
    status_code: int = 200


class OrderService:
    def __init__(
        self,
        config: Config,
        database: Database,
        cache: QueryCache,
        snapshot: SlimSnapshot,
        breaker: CircuitBreaker,
        control: ControlPlane,
        metrics: Metrics,
    ) -> None:
        self.config = config
        self.database = database
        self.cache = cache
        self.snapshot = snapshot
        self.breaker = breaker
        self.control = control
        self.metrics = metrics
        self._breaker_gate = threading.RLock()

    def _apply_breaker_transition(
        self, transition: CircuitTransition | None
    ) -> None:
        if transition is not None:
            try:
                self.control.update_circuit(transition)
            except Exception:
                self.breaker.restore_state(transition.from_state)
                raise

    def _allow_circuit_request(
        self,
    ) -> tuple[bool, str, CircuitTransition | None]:
        with self._breaker_gate:
            allowed, reason, transition = self.breaker.allow_request()
            self._apply_breaker_transition(transition)
            return allowed, reason, transition

    def _record_circuit_success(self) -> None:
        with self._breaker_gate:
            self._apply_breaker_transition(self.breaker.record_success())

    def _record_circuit_failure(
        self, *, timed_out: bool, reason: str
    ) -> None:
        with self._breaker_gate:
            self._apply_breaker_transition(
                self.breaker.record_failure(
                    timed_out=timed_out,
                    reason=reason,
                )
            )

    def _cache_lookup(
        self,
        *,
        tenant_id: str,
        status: str,
        page: int,
        page_size: int,
        slim: bool,
    ) -> list[dict[str, object]] | None:
        rows = self.cache.get(
            tenant_id=tenant_id,
            status=status,
            page=page,
            page_size=page_size,
            slim=slim,
        )
        if rows is not None:
            return rows
        if slim:
            full_rows = self.cache.get(
                tenant_id=tenant_id,
                status=status,
                page=page,
                page_size=page_size,
                slim=False,
            )
            if full_rows is not None:
                return [
                    {
                        "order_id": row["order_id"],
                        "status": row["status"],
                        "created_at": row["created_at"],
                    }
                    for row in full_rows
                ]
        return None

    def _fallback(
        self,
        *,
        tenant_id: str,
        status: str,
        page: int,
        page_size: int,
        page_size_limit: int,
        reason: str,
        mode: str,
        business_code: str,
    ) -> QueryResult:
        for source in self.config.fallback.order:
            if source == "cache":
                rows = self._cache_lookup(
                    tenant_id=tenant_id,
                    status=status,
                    page=page,
                    page_size=page_size,
                    slim=mode == "slim",
                )
                if rows is not None:
                    self.metrics.inc(
                        "order_guard_fallback_total",
                        {"source": "cache", "reason": reason},
                    )
                    return QueryResult(
                        rows=rows,
                        page=page,
                        page_size=len(rows),
                        page_size_limit=page_size_limit,
                        fallback={"source": "cache", "reason": reason},
                        business_code=business_code,
                    )
            elif source == "slim_snapshot":
                if not self.snapshot.loaded:
                    try:
                        self.snapshot.load()
                    except Exception:
                        LOGGER.exception("could not load slim snapshot")
                if self.snapshot.loaded:
                    rows = self.snapshot.query(
                        tenant_id=tenant_id,
                        status=status,
                        page=page,
                        page_size=page_size,
                    )
                    self.metrics.inc(
                        "order_guard_fallback_total",
                        {"source": "slim_snapshot", "reason": reason},
                    )
                    return QueryResult(
                        rows=rows,
                        page=page,
                        page_size=len(rows),
                        page_size_limit=page_size_limit,
                        fallback={"source": "slim_snapshot", "reason": reason},
                        business_code=business_code,
                    )
        return QueryResult(
            rows=[],
            page=page,
            page_size=0,
            page_size_limit=page_size_limit,
            fallback={"source": "none", "reason": reason},
            business_code="QUERY_FALLBACK_UNAVAILABLE",
            status_code=503,
        )

    def query_orders(
        self,
        *,
        tenant_id: str,
        status: str,
        page: int,
        page_size: int | None,
    ) -> QueryResult:
        if status not in self.config.database.statuses:
            raise ServiceError(
                "INVALID_ORDER_STATUS",
                status_code=422,
                message=f"unsupported order status: {status}",
                details={"allowed": list(self.config.database.statuses)},
            )
        if page < 1:
            raise ServiceError(
                "INVALID_PAGE",
                status_code=422,
                message="page must be >= 1",
            )
        requested_size = page_size or self.config.query.default_page_size
        if requested_size < 1:
            raise ServiceError(
                "INVALID_PAGE_SIZE",
                status_code=422,
                message="page_size must be >= 1",
            )
        effective_size = min(requested_size, self.control.page_size_cap)
        state = self.control.state
        mode = self.config.degrade_levels[state.degrade_level].query_mode

        if mode == "cache_first":
            cached = self._cache_lookup(
                tenant_id=tenant_id,
                status=status,
                page=page,
                page_size=effective_size,
                slim=False,
            )
            if cached is not None:
                self.metrics.inc(
                    "order_guard_fallback_total",
                    {"source": "cache", "reason": "slo_cache_first"},
                )
                return QueryResult(
                    rows=cached,
                    page=page,
                    page_size=len(cached),
                    page_size_limit=state.page_size_cap,
                    fallback={"source": "cache", "reason": "slo_cache_first"},
                    business_code="OK",
                )

        allowed, permit_reason, _ = self._allow_circuit_request()
        if not allowed:
            return self._fallback(
                tenant_id=tenant_id,
                status=status,
                page=page,
                page_size=effective_size,
                page_size_limit=state.page_size_cap,
                reason=permit_reason,
                mode=mode,
                business_code=self.config.circuit.code,
            )

        started = time.monotonic()
        slim = mode == "slim"
        try:
            rows = self.database.fetch_orders(
                tenant_id=tenant_id,
                status=status,
                page=page,
                page_size=effective_size,
                slim=slim,
            )
            elapsed_ms = (time.monotonic() - started) * 1000.0
            self.cache.put(
                tenant_id=tenant_id,
                status=status,
                page=page,
                page_size=effective_size,
                slim=slim,
                rows=rows,
            )
            if elapsed_ms > self.config.database.operation_timeout_ms:
                self._record_circuit_failure(
                    timed_out=True,
                    reason="query_operation_timeout",
                )
                return self._fallback(
                    tenant_id=tenant_id,
                    status=status,
                    page=page,
                    page_size=effective_size,
                    page_size_limit=state.page_size_cap,
                    reason="query_operation_timeout",
                    mode=mode,
                    business_code="DATABASE_TIMEOUT",
                )
            self._record_circuit_success()
            final_state = self.control.state
            return QueryResult(
                rows=rows,
                page=page,
                page_size=len(rows),
                page_size_limit=final_state.page_size_cap,
                fallback=None,
                business_code="OK",
            )
        except (
            DatabaseUnavailable,
            DatabaseBusy,
            DatabaseTimeout,
            DatabaseFailure,
        ) as exc:
            timed_out = isinstance(exc, DatabaseTimeout)
            reason = (
                "database_timeout"
                if timed_out
                else (
                    "sqlite_busy"
                    if isinstance(exc, DatabaseBusy)
                    else (
                        "database_unavailable"
                        if isinstance(exc, DatabaseUnavailable)
                        else "database_error"
                    )
                )
            )
            self._record_circuit_failure(
                timed_out=timed_out,
                reason=reason,
            )
            return self._fallback(
                tenant_id=tenant_id,
                status=status,
                page=page,
                page_size=effective_size,
                page_size_limit=state.page_size_cap,
                reason=reason,
                mode=mode,
                business_code=(
                    self.config.circuit.code
                    if self.control.state.circuit_state != "closed"
                    else "DATABASE_DEGRADED"
                ),
            )

    def _ensure_writes_allowed(self) -> None:
        state = self.control.state
        if state.writes_paused:
            self.metrics.inc(
                "order_guard_writes_rejected_total",
                {"reason": "capacity_protected"},
            )
            raise ServiceError(
                state.protection_code,
                status_code=503,
                message="writes are paused while capacity protection is active",
                retry_after_seconds=2,
                details={"degrade_level": state.degrade_level},
            )

    def _write_error(self, exc: Exception) -> ServiceError:
        reason = (
            "sqlite_busy"
            if isinstance(exc, DatabaseBusy)
            else (
                "database_timeout"
                if isinstance(exc, DatabaseTimeout)
                else "database_unavailable"
            )
        )
        self.metrics.inc(
            "order_guard_writes_rejected_total", {"reason": reason}
        )
        return ServiceError(
            "DATABASE_WRITE_UNAVAILABLE",
            status_code=503,
            message="order write could not reach the local database",
            retry_after_seconds=1,
            details={"reason": reason},
        )

    def create_order(
        self, *, tenant_id: str, amount_cents: int
    ) -> dict[str, object]:
        self._ensure_writes_allowed()
        if amount_cents <= 0:
            raise ServiceError(
                "INVALID_AMOUNT",
                status_code=422,
                message="amount_cents must be positive",
            )
        try:
            order = self.database.create_order(
                tenant_id=tenant_id,
                amount_cents=amount_cents,
            )
        except (
            DatabaseUnavailable,
            DatabaseBusy,
            DatabaseTimeout,
            DatabaseFailure,
        ) as exc:
            raise self._write_error(exc) from exc
        self.cache.invalidate_tenant(tenant_id)
        return order

    def update_order_status(
        self,
        *,
        tenant_id: str,
        order_id: str,
        new_status: str,
    ) -> dict[str, object]:
        self._ensure_writes_allowed()
        if new_status not in self.config.database.statuses:
            raise ServiceError(
                "INVALID_ORDER_STATUS",
                status_code=422,
                message=f"unsupported order status: {new_status}",
                details={"allowed": list(self.config.database.statuses)},
            )
        try:
            order = self.database.update_status(
                tenant_id=tenant_id,
                order_id=order_id,
                new_status=new_status,
            )
        except (
            DatabaseUnavailable,
            DatabaseBusy,
            DatabaseTimeout,
            DatabaseFailure,
        ) as exc:
            raise self._write_error(exc) from exc
        if order is None:
            raise ServiceError(
                "ORDER_NOT_FOUND",
                status_code=404,
                message="order does not exist for this tenant",
            )
        self.cache.invalidate_tenant(tenant_id)
        return order
