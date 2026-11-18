from __future__ import annotations

import hashlib
import json
import threading
from collections import Counter
from dataclasses import dataclass
from datetime import date, datetime, time
from decimal import Decimal
from typing import Any

from .config import GatewayConfig
from .db import SqlServerDatabase
from .errors import (
    E_CONN,
    E_DIALECT,
    E_FALLBACK,
    E_INDEX_UNAVAILABLE,
    E_INDEX_UNCOVERED,
    E_PLAN_INDEX,
    E_PLAN_SCAN,
    E_PLAN_SORT_SPILL,
    E_SUMMARY_LIMIT,
    E_SUMMARY_UNAVAILABLE,
    GatewayError,
)
from .guard import validate_read_only
from .indexing import Analysis, analyze_query, apply_index_hints
from .limiter import FairLimiter
from .normalizer import normalize_query
from .plan import validate_plan


VALID_DIALECTS = {"mssql-legacy", "mssql-current"}


def normalize_value(value: Any) -> Any:
    if isinstance(value, Decimal):
        return format(value, "f")
    if isinstance(value, datetime):
        return value.isoformat(timespec="seconds")
    if isinstance(value, date):
        return value.isoformat()
    if isinstance(value, time):
        return value.isoformat()
    if isinstance(value, bytes):
        return value.hex()
    if isinstance(value, (str, int, float, bool)) or value is None:
        return value
    return str(value)


def result_digest(columns: list[str], rows: list[list[Any]]) -> str:
    payload = json.dumps(
        {"columns": columns, "rows": rows},
        ensure_ascii=False,
        separators=(",", ":"),
        sort_keys=True,
    )
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


@dataclass(slots=True)
class QueryRequest:
    sql: str
    dialect: str | None
    query_id: str | None = None
    fallback: str | None = None
    tenant: str | None = None


class QueryEngine:
    def __init__(
        self,
        config: GatewayConfig,
        database: SqlServerDatabase | None = None,
        limiter: FairLimiter | None = None,
    ) -> None:
        self.config = config
        self.database = database or SqlServerDatabase(
            config.database,
            config.limits.query_timeout_s,
        )
        self.limiter = limiter or FairLimiter(
            config.limits.max_connections,
            config.limits.per_tenant_connections,
            config.limits.acquire_timeout_ms,
        )
        self._counter_lock = threading.Lock()
        self._submitted = 0
        self._succeeded = 0
        self._degraded = 0
        self._rejected = 0
        self._by_error: Counter[str] = Counter()
        self._by_reason: Counter[str] = Counter()
        self._degradation_trace: dict[str, dict[str, Any]] = {}

    def query(self, request: QueryRequest) -> dict[str, Any]:
        self._record_submitted()
        if not isinstance(request.dialect, str) or request.dialect not in VALID_DIALECTS:
            self._record(
                rejected=E_DIALECT,
                reason="missing" if request.dialect is None else "invalid",
            )
            raise GatewayError(
                E_DIALECT,
                "dialect must be exactly one of mssql-legacy or mssql-current",
                details={
                    "received": request.dialect,
                    "allowed": sorted(VALID_DIALECTS),
                },
            )

        query_id = request.query_id or self._auto_query_id(request.sql, request.dialect)
        try:
            validated = validate_read_only(request.sql)
            normalized = normalize_query(validated.sql, request.dialect, self.config)
            analysis = analyze_query(normalized.sql, self.config)
            execution_sql = apply_index_hints(normalized.sql, analysis)
            fallback = request.fallback or self.config.fallback.default
            if fallback not in self.config.fallback.allowed:
                raise GatewayError(
                    E_FALLBACK,
                    "fallback must be one of the configured degradation modes",
                    details={"received": fallback, "allowed": self.config.fallback.allowed},
                )
            tenant_key = str(
                analysis.tenant_id
                if analysis.tenant_id is not None
                else (request.tenant or "unknown")
            )
            if not analysis.can_execute_detail:
                return self._degrade_or_reject(
                    query_id=query_id,
                    dialect=request.dialect,
                    analysis=analysis,
                    fallback=fallback,
                    tenant_key=tenant_key,
                    normalized_sql=execution_sql,
                    notes=normalized.notes,
                )
            return self._execute_detail(
                query_id=query_id,
                dialect=request.dialect,
                analysis=analysis,
                fallback=fallback,
                tenant_key=tenant_key,
                normalized_sql=execution_sql,
                notes=normalized.notes,
            )
        except GatewayError as exc:
            self._record(rejected=exc.code, reason=str(exc.details.get("reason", "gateway")))
            raise

    def _execute_detail(
        self,
        *,
        query_id: str,
        dialect: str,
        analysis: Analysis,
        fallback: str,
        tenant_key: str,
        normalized_sql: str,
        notes: list[str],
    ) -> dict[str, Any]:
        self.limiter.acquire(tenant_key)
        connection = None
        try:
            connection = self.database.connect()
            for index_spec in analysis.expected_indexes:
                state = self.database.index_state(
                    connection,
                    index_spec.table,
                    index_spec.name,
                )
                if state != "enabled":
                    raise GatewayError(
                        E_INDEX_UNAVAILABLE,
                        "the selected covering index is unavailable",
                        fragment=analysis.condition_fragment,
                        details={
                            "reason": "index_disabled" if state == "disabled" else "index_missing",
                            "index": index_spec.name,
                            "table": index_spec.table,
                            "condition_fragment": analysis.condition_fragment,
                        },
                    )

            expected = [index_spec.name for index_spec in analysis.expected_indexes]
            fragment = analysis.condition_fragment
            try:
                preflight = self.database.preflight_plan(connection, normalized_sql)
                validate_plan(
                    preflight,
                    expected,
                    require_seek=True,
                    reject_sort_spill=True,
                    fragment=fragment,
                )
                raw = self.database.execute_with_plan(connection, normalized_sql)
                validate_plan(
                    raw.plan,
                    expected,
                    require_seek=True,
                    reject_sort_spill=True,
                    fragment=fragment,
                )
            except GatewayError as exc:
                if (
                    fallback == "summary"
                    and exc.code in {E_PLAN_SCAN, E_PLAN_INDEX, E_PLAN_SORT_SPILL}
                    and analysis.tenant_id is not None
                    and analysis.status is not None
                ):
                    return self._summary_response(
                        connection=connection,
                        query_id=query_id,
                        dialect=dialect,
                        tenant_id=analysis.tenant_id,
                        status=analysis.status,
                        reason_code=exc.code,
                        reason=exc.message,
                        condition_fragment=exc.fragment or fragment,
                        normalized_sql=normalized_sql,
                        notes=notes,
                    )
                raise

            columns = raw.columns
            rows = [[normalize_value(value) for value in row] for row in raw.rows]
            response = {
                "ok": True,
                "query_id": query_id,
                "dialect": dialect,
                "mode": "detail",
                "aggregate": False,
                "degraded": False,
                "columns": columns,
                "rows": rows,
                "row_count": len(rows),
                "result_digest": result_digest(columns, rows),
                "index_names": sorted(expected),
                "access": raw.plan.access,
                "plan_rows": [row.to_dict() for row in raw.plan.rows],
                "normalization_notes": list(notes),
            }
            with self._counter_lock:
                self._succeeded += 1
            return response
        finally:
            if connection is not None:
                connection.close()
            self.limiter.release(tenant_key)

    def _degrade_or_reject(
        self,
        *,
        query_id: str,
        dialect: str,
        analysis: Analysis,
        fallback: str,
        tenant_key: str,
        normalized_sql: str,
        notes: list[str],
    ) -> dict[str, Any]:
        fragment = analysis.condition_fragment
        if fallback != "summary":
            raise GatewayError(
                E_INDEX_UNCOVERED,
                "no preset covering index matches the tenant/status prefix and selected columns",
                fragment=fragment,
                details={
                    "reason": "uncovered_query",
                    "fallback": "reject",
                    "condition_fragment": fragment,
                    "tables": [
                        {
                            "alias": table.alias,
                            "table": table.table,
                            "required_columns": sorted(table.required_columns),
                            "reason": table.unresolved_reason,
                        }
                        for table in analysis.unresolved
                    ],
                },
            )
        if analysis.tenant_id is None or analysis.status is None:
            raise GatewayError(
                E_SUMMARY_UNAVAILABLE,
                "summary fallback requires constant TenantId and Status predicates",
                fragment=fragment,
                details={
                    "reason": "summary_key_missing",
                    "tenant_id": analysis.tenant_id,
                    "status": analysis.status,
                },
            )
        self.limiter.acquire(tenant_key)
        connection = None
        try:
            connection = self.database.connect()
            return self._summary_response(
                connection=connection,
                query_id=query_id,
                dialect=dialect,
                tenant_id=analysis.tenant_id,
                status=analysis.status,
                reason_code=E_INDEX_UNCOVERED,
                reason="no preset covering index covers the requested detail columns",
                condition_fragment=fragment,
                normalized_sql=normalized_sql,
                notes=notes,
            )
        finally:
            if connection is not None:
                connection.close()
            self.limiter.release(tenant_key)

    def _summary_response(
        self,
        *,
        connection,
        query_id: str,
        dialect: str,
        tenant_id: int,
        status: str,
        reason_code: str,
        reason: str,
        condition_fragment: str | None,
        normalized_sql: str,
        notes: list[str],
    ) -> dict[str, Any]:
        max_rows = self.config.limits.summary_max_rows
        if max_rows <= 0:
            raise GatewayError(
                E_SUMMARY_LIMIT,
                "summary fallback is disabled by the configured row limit",
                fragment=condition_fragment,
            )
        raw = self.database.summary_rows(
            connection,
            tenant_id=tenant_id,
            status=status,
            max_rows=max_rows,
        )
        columns = raw.columns
        rows = [[normalize_value(value) for value in row] for row in raw.rows]
        lower_columns = [column.lower() for column in columns]
        key_parts = []
        for column in ("tenantid", "status", "summarydate"):
            key_parts.append(lower_columns.index(column) if column in lower_columns else None)
        summary_keys = []
        for row in rows:
            values = [
                str(row[index]) if index is not None and index < len(row) else ""
                for index in key_parts
            ]
            summary_keys.append("|".join(values) if all(values) else "")
        response = {
            "ok": True,
            "query_id": query_id,
            "dialect": dialect,
            "mode": "summary",
            "aggregate": True,
            "degraded": True,
            "columns": columns,
            "rows": rows,
            "row_count": len(rows),
            "result_digest": result_digest(columns, rows),
            "source": "dbo.OrderSummaryDaily",
            "summary_row_keys": summary_keys,
            "degradation": {
                "reason_code": reason_code,
                "reason": reason,
                "condition_fragment": condition_fragment,
                "fallback": "summary",
                "summary_row_keys": summary_keys,
            },
            "access": raw.plan.access,
            "index_names": sorted(raw.plan.seek_indexes),
            "plan_rows": [row.to_dict() for row in raw.plan.rows],
            "normalization_notes": list(notes),
        }
        trace_key = f"{query_id}|{dialect}"
        with self._counter_lock:
            self._degraded += 1
            self._by_reason[f"summary:{reason_code}"] += 1
            self._degradation_trace[trace_key] = {
                "query_id": query_id,
                "dialect": dialect,
                "tenant_id": tenant_id,
                "status": status,
                "summary_row_keys": summary_keys,
                "reason_code": reason_code,
            }
            if len(self._degradation_trace) > 200:
                oldest = next(iter(self._degradation_trace))
                self._degradation_trace.pop(oldest, None)
        return response

    def status(self) -> dict[str, Any]:
        with self._counter_lock:
            counters = {
                "submitted": self._submitted,
                "succeeded_detail": self._succeeded,
                "degraded_summary": self._degraded,
                "rejected": self._rejected,
                "by_error": dict(sorted(self._by_error.items())),
                "by_reason": dict(sorted(self._by_reason.items())),
            }
            trace = {
                key: dict(value)
                for key, value in sorted(self._degradation_trace.items())
            }
        return {
            "connections": self.limiter.status(),
            "counters": counters,
            "degradation_trace": trace,
            "limits": {
                "query_timeout_s": self.config.limits.query_timeout_s,
                "summary_max_rows": self.config.limits.summary_max_rows,
            },
            "indexes": {
                "configured": sorted(index.name for index in self.config.indexes),
            },
        }

    def _auto_query_id(self, sql: str, dialect: str) -> str:
        digest = hashlib.sha256(f"{dialect}\0{sql}".encode("utf-8")).hexdigest()
        return f"auto-{digest[:16]}"

    def _record_submitted(self) -> None:
        with self._counter_lock:
            self._submitted += 1

    def _record(self, *, rejected: str, reason: str) -> None:
        with self._counter_lock:
            self._rejected += 1
            self._by_error[rejected] += 1
            self._by_reason[reason] += 1
