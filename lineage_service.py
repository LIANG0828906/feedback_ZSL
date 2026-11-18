#!/usr/bin/env python3
"""HTTP service for column-level warehouse change impact analysis."""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import math
import os
import re
import sqlite3
import threading
import traceback
from datetime import datetime, timezone
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, unquote, urlparse

from sql_lineage import (
    BusinessError,
    LineageEngine,
    load_warehouse_metadata,
)


LOGGER = logging.getLogger("lineage_service")
MAX_BODY_BYTES = 1024 * 1024
DEFAULT_THRESHOLD_PCT = 0.0


RUNTIME_DDL = """
CREATE TABLE IF NOT EXISTS change_requests (
    change_id TEXT PRIMARY KEY,
    fingerprint TEXT NOT NULL UNIQUE,
    table_name TEXT NOT NULL,
    column_name TEXT NOT NULL,
    change_type TEXT NOT NULL,
    new_type TEXT,
    inferred_new_type INTEGER NOT NULL DEFAULT 0,
    drift_threshold REAL NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    completed_at TEXT
);
CREATE TABLE IF NOT EXISTS impact_results (
    change_id TEXT PRIMARY KEY,
    result_json TEXT NOT NULL,
    affected_task_count INTEGER NOT NULL,
    direct_count INTEGER NOT NULL,
    transitive_count INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS drift_checks (
    drift_id TEXT PRIMARY KEY,
    change_id TEXT NOT NULL,
    threshold_pct REAL NOT NULL,
    status TEXT NOT NULL,
    result_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (change_id, threshold_pct)
);
CREATE TABLE IF NOT EXISTS rollback_plans (
    change_id TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    result_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_events (
    event_id INTEGER PRIMARY KEY AUTOINCREMENT,
    change_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    event_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_drift_change
    ON drift_checks(change_id, created_at);
CREATE INDEX IF NOT EXISTS idx_audit_change
    ON audit_events(change_id, event_id);
"""


TYPE_PATTERN = re.compile(
    r"^(?:"
    r"INTEGER|INT|BIGINT|SMALLINT|TINYINT|REAL|DOUBLE|FLOAT|NUMERIC|DECIMAL|"
    r"TEXT|VARCHAR|CHAR|DATE|TIMESTAMP|DATETIME"
    r")(?:\s*\(\s*\d+(?:\s*,\s*\d+)?\s*\))?$",
    re.IGNORECASE,
)


CHANGE_TYPE_ALIASES = {
    "delete": "delete_column",
    "drop": "delete_column",
    "drop_column": "delete_column",
    "delete_column": "delete_column",
    "column_delete": "delete_column",
    "删除列": "delete_column",
    "type_narrow": "type_narrow",
    "type_narrowing": "type_narrow",
    "narrow": "type_narrow",
    "narrow_type": "type_narrow",
    "narrow_column": "type_narrow",
    "类型收窄": "type_narrow",
}


def utc_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def sha256_text(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def _safe_json_loads(value: str | None, default: Any) -> Any:
    if not value:
        return default
    try:
        return json.loads(value)
    except (TypeError, ValueError):
        return default


def _public_change(row: sqlite3.Row, change: dict[str, Any]) -> dict[str, Any]:
    return {
        "change_id": row["change_id"],
        "table_name": row["table_name"],
        "column_name": row["column_name"],
        "change_type": row["change_type"],
        "new_type": row["new_type"],
        "inferred_new_type": change.get("inferred_new_type", False),
        "drift_threshold_pct": float(row["drift_threshold"]),
        "status": row["status"],
        "created_at": row["created_at"],
        "completed_at": row["completed_at"],
    }


class ImpactService:
    """Business API and transactional SQLite persistence."""

    def __init__(self, db_path: str | Path) -> None:
        self.db_path = str(Path(db_path).resolve())
        self.ready = False
        self.fatal_error: BusinessError | None = None
        self.metadata = None
        self.engine: LineageEngine | None = None
        self.started_at = utc_now()
        self._write_lock = threading.RLock()
        self._completion_lock = threading.RLock()
        self._load()

    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self.db_path, timeout=10)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA foreign_keys = ON")
        conn.execute("PRAGMA busy_timeout = 5000")
        return conn

    def _load(self) -> None:
        if not Path(self.db_path).is_file():
            self.fatal_error = BusinessError(
                "E_METADATA_UNAVAILABLE",
                "The metadata database is not available.",
                {"object_type": "metadata_database"},
                http_status=503,
            )
            return
        try:
            conn = self._connect()
            try:
                conn.executescript(RUNTIME_DDL)
                change_columns = {
                    row["name"]
                    for row in conn.execute(
                        "PRAGMA table_info(change_requests)"
                    )
                }
                if "inferred_new_type" not in change_columns:
                    conn.execute(
                        "ALTER TABLE change_requests ADD COLUMN "
                        "inferred_new_type INTEGER NOT NULL DEFAULT 0"
                    )
                conn.commit()
                metadata = load_warehouse_metadata(conn)
            finally:
                conn.close()
            self.metadata = metadata
            self.engine = LineageEngine(metadata, self.db_path)
            self.ready = True
        except BusinessError as error:
            self.fatal_error = error
            LOGGER.error(
                "Metadata initialization failed: %s %s",
                error.code,
                error.message,
            )
        except sqlite3.DatabaseError:
            self.fatal_error = BusinessError(
                "E_METADATA_INVALID",
                "The SQLite metadata database could not be opened.",
                {"object_type": "metadata_database"},
                http_status=503,
            )
            LOGGER.error("Metadata initialization failed with a SQLite error")
        except Exception:
            self.fatal_error = BusinessError(
                "E_INTERNAL",
                "Service initialization failed.",
                {},
                http_status=500,
            )
            LOGGER.exception("Unexpected metadata initialization failure")

    def _ensure_ready(self) -> None:
        if not self.ready or self.engine is None or self.metadata is None:
            if self.fatal_error is not None:
                raise self.fatal_error
            raise BusinessError(
                "E_INTERNAL",
                "Service is not ready.",
                {},
                http_status=500,
            )

    def health(self) -> tuple[int, dict[str, Any]]:
        if not self.ready:
            body: dict[str, Any] = {
                "ok": False,
                "status": "DEGRADED",
                "service": "warehouse-change-impact",
                "started_at": self.started_at,
            }
            if self.fatal_error is not None:
                body.update(self.fatal_error.as_dict())
                body["ok"] = False
                return self.fatal_error.http_status, body
            return 500, body
        assert self.metadata is not None
        task_count = len(self.metadata.tasks)
        return 200, {
            "ok": True,
            "status": "READY",
            "code": "OK",
            "service": "warehouse-change-impact",
            "started_at": self.started_at,
            "snapshot_id": self.metadata.snapshot_id,
            "snapshot_fingerprint": self.engine.snapshot_fingerprint
            if self.engine
            else None,
            "counts": {
                "tables": len(self.metadata.schema),
                "tasks": task_count,
                "reports": len(
                    {report["report_id"] for report in self.metadata.reports}
                ),
                "metrics": len(self.metadata.metrics),
            },
        }

    def _normalize_threshold(self, value: Any, default: float) -> float:
        if value is None:
            return float(default)
        if isinstance(value, bool):
            raise BusinessError(
                "E_THRESHOLD_RANGE",
                "threshold_pct must be a number from 0 to 100.",
                {"field": "threshold_pct"},
            )
        try:
            threshold = float(value)
        except (TypeError, ValueError) as error:
            raise BusinessError(
                "E_THRESHOLD_RANGE",
                "threshold_pct must be a number from 0 to 100.",
                {"field": "threshold_pct"},
            ) from error
        if not math.isfinite(threshold) or threshold < 0 or threshold > 100:
            raise BusinessError(
                "E_THRESHOLD_RANGE",
                "threshold_pct must be between 0 and 100 inclusive.",
                {"field": "threshold_pct", "value": value},
            )
        if threshold == 0:
            threshold = 0.0
        return round(threshold, 8)

    def _infer_narrow_type(self, current_type: str) -> str:
        upper = str(current_type).upper()
        if any(
            token in upper
            for token in ("REAL", "DOUBLE", "FLOAT", "NUMERIC", "DECIMAL")
        ):
            return "INTEGER"
        if any(token in upper for token in ("TEXT", "CHAR", "CLOB")):
            return "TEXT(8)"
        if any(token in upper for token in ("DATE", "TIME")):
            return "TEXT(10)"
        if "INT" in upper:
            return "SMALLINT"
        return "TEXT(8)"

    def normalize_change(self, payload: Any) -> dict[str, Any]:
        self._ensure_ready()
        assert self.metadata is not None
        assert self.engine is not None
        if not isinstance(payload, dict):
            raise BusinessError(
                "E_INVALID_REQUEST",
                "The request body must be a JSON object.",
                {"field": "body"},
            )

        task_name = payload.get("task_name", payload.get("task"))
        if task_name is not None:
            canonical_task = next(
                (
                    task.task_name
                    for task in self.metadata.tasks
                    if task.task_name.lower() == str(task_name).lower()
                ),
                None,
            )
            if canonical_task is None:
                raise BusinessError(
                    "E_UNKNOWN_TASK",
                    "The requested task does not exist.",
                    {"object_type": "task", "object_name": task_name},
                )

        table_value = payload.get(
            "table_name", payload.get("table", payload.get("table_id"))
        )
        column_value = payload.get(
            "column_name",
            payload.get("column", payload.get("column_id")),
        )
        if not isinstance(table_value, str) or not table_value.strip():
            raise BusinessError(
                "E_INVALID_REQUEST",
                "table_name is required.",
                {"field": "table_name"},
            )
        if not isinstance(column_value, str) or not column_value.strip():
            raise BusinessError(
                "E_INVALID_REQUEST",
                "column_name is required.",
                {"field": "column_name"},
            )
        raw_change_type = payload.get(
            "change_type",
            payload.get("type", payload.get("operation")),
        )
        if not isinstance(raw_change_type, str):
            raise BusinessError(
                "E_INVALID_CHANGE_TYPE",
                "change_type must be delete_column or type_narrow.",
                {"field": "change_type"},
            )
        change_type = CHANGE_TYPE_ALIASES.get(
            raw_change_type.strip().lower()
        )
        if change_type is None:
            raise BusinessError(
                "E_INVALID_CHANGE_TYPE",
                "Unsupported change_type.",
                {
                    "field": "change_type",
                    "value": raw_change_type,
                    "allowed": ["delete_column", "type_narrow"],
                },
            )

        table_name, column_name = self.engine.resolve_change(
            table_value.strip(), column_value.strip()
        )
        current_type = self.metadata.schema[table_name]["columns"][
            column_name
        ]
        new_type = None
        inferred = False
        if change_type == "type_narrow":
            requested_type = payload.get(
                "new_type",
                payload.get(
                    "narrowed_type", payload.get("target_type")
                ),
            )
            if requested_type is None:
                new_type = self._infer_narrow_type(current_type)
                inferred = True
            elif not isinstance(requested_type, str) or not requested_type.strip():
                raise BusinessError(
                    "E_INVALID_TYPE",
                    "new_type must be a non-empty SQL type name.",
                    {"field": "new_type"},
                )
            else:
                new_type = requested_type.strip().upper()
                new_type = re.sub(r"\s+", " ", new_type)
                if len(new_type) > 64 or not TYPE_PATTERN.match(new_type):
                    raise BusinessError(
                        "E_INVALID_TYPE",
                        "new_type is not a supported SQL type.",
                        {"field": "new_type", "value": requested_type},
                    )

        raw_threshold = payload.get(
            "threshold_pct",
            payload.get(
                "threshold", payload.get("drift_threshold")
            ),
        )
        threshold = self._normalize_threshold(
            raw_threshold, DEFAULT_THRESHOLD_PCT
        )
        fingerprint_value = {
            "version": 1,
            "table_name": table_name,
            "column_name": column_name,
            "change_type": change_type,
            "new_type": new_type,
        }
        fingerprint = sha256_text(canonical_json(fingerprint_value))
        change_id = "chg_" + fingerprint[:20]
        return {
            "change_id": change_id,
            "fingerprint": fingerprint,
            "table_name": table_name,
            "column_name": column_name,
            "change_type": change_type,
            "new_type": new_type,
            "inferred_new_type": inferred,
            "original_type": current_type,
            "drift_threshold_pct": threshold,
        }

    def _change_from_row(
        self, row: sqlite3.Row
    ) -> dict[str, Any]:
        return {
            "change_id": row["change_id"],
            "fingerprint": row["fingerprint"],
            "table_name": row["table_name"],
            "column_name": row["column_name"],
            "change_type": row["change_type"],
            "new_type": row["new_type"],
            "inferred_new_type": bool(row["inferred_new_type"]),
            "drift_threshold_pct": float(row["drift_threshold"]),
        }

    def _audit(
        self,
        conn: sqlite3.Connection,
        change_id: str,
        event_type: str,
        payload: dict[str, Any],
        created_at: str,
    ) -> None:
        conn.execute(
            "INSERT INTO audit_events "
            "(change_id, event_type, event_json, created_at) "
            "VALUES (?, ?, ?, ?)",
            (
                change_id,
                event_type,
                canonical_json(payload),
                created_at,
            ),
        )

    def _get_change_row(
        self, conn: sqlite3.Connection, change_id: str
    ) -> sqlite3.Row | None:
        return conn.execute(
            "SELECT * FROM change_requests WHERE change_id = ?",
            (change_id,),
        ).fetchone()

    def _get_change_by_fingerprint(
        self, conn: sqlite3.Connection, fingerprint: str
    ) -> sqlite3.Row | None:
        return conn.execute(
            "SELECT * FROM change_requests WHERE fingerprint = ?",
            (fingerprint,),
        ).fetchone()

    def _get_impact_row(
        self, conn: sqlite3.Connection, change_id: str
    ) -> sqlite3.Row | None:
        return conn.execute(
            "SELECT * FROM impact_results WHERE change_id = ?",
            (change_id,),
        ).fetchone()

    def _store_submission(self, change: dict[str, Any]) -> sqlite3.Row:
        with self._write_lock:
            conn = self._connect()
            try:
                conn.execute("BEGIN IMMEDIATE")
                existing = self._get_change_by_fingerprint(
                    conn, change["fingerprint"]
                )
                if existing is not None:
                    self._audit(
                        conn,
                        existing["change_id"],
                        "change_resubmitted",
                        {
                            "change_id": existing["change_id"],
                            "idempotent": True,
                            "change": {
                                key: change[key]
                                for key in (
                                    "table_name",
                                    "column_name",
                                    "change_type",
                                    "new_type",
                                    "inferred_new_type",
                                    "drift_threshold_pct",
                                )
                            },
                        },
                        utc_now(),
                    )
                    conn.commit()
                    return existing
                now = utc_now()
                conn.execute(
                    "INSERT INTO change_requests ("
                    "change_id, fingerprint, table_name, column_name, "
                    "change_type, new_type, inferred_new_type, "
                    "drift_threshold, status, created_at, completed_at"
                    ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)",
                    (
                        change["change_id"],
                        change["fingerprint"],
                        change["table_name"],
                        change["column_name"],
                        change["change_type"],
                        change["new_type"],
                        1 if change["inferred_new_type"] else 0,
                        change["drift_threshold_pct"],
                        "COMPUTING",
                        now,
                    ),
                )
                self._audit(
                    conn,
                    change["change_id"],
                    "change_submitted",
                    {
                        "change_id": change["change_id"],
                        "change": {
                            key: change[key]
                            for key in (
                                "table_name",
                                "column_name",
                                "change_type",
                                "new_type",
                                "inferred_new_type",
                                "drift_threshold_pct",
                            )
                        },
                    },
                    now,
                )
                conn.commit()
                row = self._get_change_row(conn, change["change_id"])
                assert row is not None
                return row
            except sqlite3.IntegrityError:
                conn.rollback()
                existing = self._get_change_by_fingerprint(
                    conn, change["fingerprint"]
                )
                if existing is None:
                    raise
                return existing
            finally:
                conn.close()

    def _impact_response(
        self,
        change_row: sqlite3.Row,
        impact: dict[str, Any],
        created_at: str,
    ) -> dict[str, Any]:
        change_public = _public_change(
            change_row, self._change_from_row(change_row)
        )
        summary = impact.get("summary", {})
        return {
            "ok": True,
            "code": "OK",
            "change_id": change_row["change_id"],
            "status": change_row["status"],
            "change": change_public,
            "impact": impact,
            "affected_tasks": impact.get("affected_tasks", []),
            "affected_reports": impact.get("affected_reports", []),
            "summary": summary,
            "affected_task_count": summary.get("affected_task_count", 0),
            "direct_count": summary.get("direct_count", 0),
            "transitive_count": summary.get("transitive_count", 0),
            "rollback_order": impact.get("rollback_order", []),
            "created_at": created_at,
        }

    def _complete_impact(
        self, change: dict[str, Any], impact: dict[str, Any]
    ) -> tuple[sqlite3.Row, dict[str, Any], str]:
        with self._completion_lock:
            conn = self._connect()
            try:
                conn.execute("BEGIN IMMEDIATE")
                change_row = self._get_change_row(
                    conn, change["change_id"]
                )
                if change_row is None:
                    raise BusinessError(
                        "E_NOT_FOUND",
                        "The change identifier does not exist.",
                        {"change_id": change["change_id"]},
                        http_status=404,
                    )
                impact_row = self._get_impact_row(
                    conn, change["change_id"]
                )
                if impact_row is not None:
                    conn.commit()
                    stored = _safe_json_loads(
                        impact_row["result_json"], impact
                    )
                    return change_row, stored, impact_row["created_at"]
                now = utc_now()
                summary = impact.get("summary", {})
                conn.execute(
                    "INSERT INTO impact_results ("
                    "change_id, result_json, affected_task_count, "
                    "direct_count, transitive_count, created_at"
                    ") VALUES (?, ?, ?, ?, ?, ?)",
                    (
                        change["change_id"],
                        canonical_json(impact),
                        int(summary.get("affected_task_count", 0)),
                        int(summary.get("direct_count", 0)),
                        int(summary.get("transitive_count", 0)),
                        now,
                    ),
                )
                conn.execute(
                    "UPDATE change_requests SET status = 'COMPLETED', "
                    "completed_at = ? WHERE change_id = ?",
                    (now, change["change_id"]),
                )
                self._audit(
                    conn,
                    change["change_id"],
                    "impact_completed",
                    {
                        "change_id": change["change_id"],
                        "summary": summary,
                    },
                    now,
                )
                conn.commit()
                updated = self._get_change_row(
                    conn, change["change_id"]
                )
                assert updated is not None
                return updated, impact, now
            except BaseException:
                conn.rollback()
                raise
            finally:
                conn.close()

    def submit_change(self, payload: Any) -> tuple[int, dict[str, Any]]:
        change = self.normalize_change(payload)
        change_row = self._store_submission(change)

        conn = self._connect()
        try:
            impact_row = self._get_impact_row(
                conn, change_row["change_id"]
            )
            if impact_row is not None:
                stored = _safe_json_loads(
                    impact_row["result_json"], {}
                )
                return 200, self._impact_response(
                    change_row, stored, impact_row["created_at"]
                )
        finally:
            conn.close()

        assert self.engine is not None
        impact = self.engine.compute_impact(change)
        change_row, stored, created_at = self._complete_impact(
            change, impact
        )
        return 200, self._impact_response(
            change_row, stored, created_at
        )

    def _load_change(self, change_id: str) -> sqlite3.Row:
        conn = self._connect()
        try:
            row = self._get_change_row(conn, change_id)
            if row is None:
                raise BusinessError(
                    "E_NOT_FOUND",
                    "The change identifier does not exist.",
                    {"change_id": change_id},
                    http_status=404,
                )
            return row
        finally:
            conn.close()

    def get_impact(self, change_id: str) -> tuple[int, dict[str, Any]]:
        self._ensure_ready()
        conn = self._connect()
        try:
            change_row = self._get_change_row(conn, change_id)
            if change_row is None:
                raise BusinessError(
                    "E_NOT_FOUND",
                    "The change identifier does not exist.",
                    {"change_id": change_id},
                    http_status=404,
                )
            impact_row = self._get_impact_row(conn, change_id)
            if impact_row is not None:
                stored = _safe_json_loads(
                    impact_row["result_json"], {}
                )
                return 200, self._impact_response(
                    change_row, stored, impact_row["created_at"]
                )
        finally:
            conn.close()

        change = self._change_from_row(change_row)
        assert self.engine is not None
        impact = self.engine.compute_impact(change)
        change_row, stored, created_at = self._complete_impact(
            change, impact
        )
        return 200, self._impact_response(
            change_row, stored, created_at
        )

    def _drift_row(
        self, conn: sqlite3.Connection, change_id: str, threshold: float
    ) -> sqlite3.Row | None:
        return conn.execute(
            "SELECT * FROM drift_checks WHERE change_id = ? "
            "AND threshold_pct = ?",
            (change_id, threshold),
        ).fetchone()

    def _drift_response(
        self,
        change_row: sqlite3.Row,
        drift_row: sqlite3.Row,
    ) -> dict[str, Any]:
        result = _safe_json_loads(drift_row["result_json"], {})
        return {
            "ok": True,
            "code": result.get("status", drift_row["status"]),
            "change_id": change_row["change_id"],
            "drift_check_id": drift_row["drift_id"],
            "change": _public_change(
                change_row, self._change_from_row(change_row)
            ),
            "status": drift_row["status"],
            "threshold_pct": float(drift_row["threshold_pct"]),
            "has_drift": bool(result.get("has_drift")),
            "summary": result.get("summary", {}),
            "drifted_metrics": result.get("drifted_metrics", []),
            "metric_results": result.get("metric_results", []),
            "metrics": result.get("metric_results", []),
            "replay": result.get("replay", {}),
            "created_at": drift_row["created_at"],
        }

    def run_drift(
        self, change_id: str, payload: Any
    ) -> tuple[int, dict[str, Any]]:
        self._ensure_ready()
        if payload is None:
            payload = {}
        if not isinstance(payload, dict):
            raise BusinessError(
                "E_INVALID_REQUEST",
                "The request body must be a JSON object.",
                {"field": "body"},
            )
        change_row = self._load_change(change_id)
        raw_threshold = payload.get(
            "threshold_pct",
            payload.get(
                "threshold", payload.get("drift_threshold")
            ),
        )
        threshold = self._normalize_threshold(
            raw_threshold, float(change_row["drift_threshold"])
        )
        conn = self._connect()
        try:
            existing = self._drift_row(conn, change_id, threshold)
            if existing is not None:
                return 200, self._drift_response(change_row, existing)
        finally:
            conn.close()

        change = self._change_from_row(change_row)
        assert self.engine is not None
        result = self.engine.compute_drift(change, threshold)
        drift_id = "drift_" + sha256_text(
            canonical_json(
                {"change_id": change_id, "threshold_pct": threshold}
            )
        )[:20]
        with self._write_lock:
            conn = self._connect()
            try:
                conn.execute("BEGIN IMMEDIATE")
                existing = self._drift_row(conn, change_id, threshold)
                if existing is not None:
                    conn.commit()
                    return 200, self._drift_response(
                        change_row, existing
                    )
                now = utc_now()
                conn.execute(
                    "INSERT INTO drift_checks ("
                    "drift_id, change_id, threshold_pct, status, "
                    "result_json, created_at"
                    ") VALUES (?, ?, ?, ?, ?, ?)",
                    (
                        drift_id,
                        change_id,
                        threshold,
                        result["status"],
                        canonical_json(result),
                        now,
                    ),
                )
                self._audit(
                    conn,
                    change_id,
                    "drift_checked",
                    {
                        "change_id": change_id,
                        "drift_check_id": drift_id,
                        "threshold_pct": threshold,
                        "status": result["status"],
                        "drifted_metrics": result.get(
                            "drifted_metrics", []
                        ),
                    },
                    now,
                )
                conn.commit()
                row = conn.execute(
                    "SELECT * FROM drift_checks WHERE drift_id = ?",
                    (drift_id,),
                ).fetchone()
                assert row is not None
                return 200, self._drift_response(change_row, row)
            except BaseException:
                conn.rollback()
                raise
            finally:
                conn.close()

    def latest_drift(
        self, change_id: str
    ) -> tuple[int, dict[str, Any]]:
        self._ensure_ready()
        change_row = self._load_change(change_id)
        conn = self._connect()
        try:
            row = conn.execute(
                "SELECT * FROM drift_checks WHERE change_id = ? "
                "ORDER BY created_at DESC, drift_id DESC LIMIT 1",
                (change_id,),
            ).fetchone()
            if row is None:
                raise BusinessError(
                    "E_NOT_FOUND",
                    "No drift check exists for this change.",
                    {"change_id": change_id},
                    http_status=404,
                )
            return 200, self._drift_response(change_row, row)
        finally:
            conn.close()

    def _rollback_plan_request(
        self, change_id: str
    ) -> tuple[int, dict[str, Any]]:
        self._ensure_ready()
        change_row = self._load_change(change_id)
        conn = self._connect()
        try:
            existing = conn.execute(
                "SELECT * FROM rollback_plans WHERE change_id = ?",
                (change_id,),
            ).fetchone()
            if existing is not None:
                result = _safe_json_loads(existing["result_json"], {})
                if result.get("blocked"):
                    raise BusinessError(
                        "E_ROLLBACK_BLOCKED",
                        "The rollback plan is blocked and cannot be emitted.",
                        result,
                        http_status=409,
                    )
                return 200, result
        finally:
            conn.close()

        status, impact_response = self.get_impact(change_id)
        impact = impact_response["impact"]
        affected_tasks = impact.get("affected_tasks", [])
        affected_names = {
            item["task_name"] for item in affected_tasks
        }
        assert self.engine is not None
        cycles = self.engine.find_rollback_cycles(affected_names)
        recheck_metrics = self.engine.recheck_metrics_for_tasks(
            affected_names
        )
        blockers: list[dict[str, Any]] = []
        if cycles:
            blockers.append(
                {
                    "reason": "TASK_DEPENDENCY_CYCLE",
                    "message": "Affected rollback tasks contain a dependency cycle.",
                    "cycles": cycles,
                }
            )
        if change_row["change_type"] == "delete_column":
            deleted_column = (
                f"{change_row['table_name']}.{change_row['column_name']}"
            )
            referencing_tasks = sorted(
                item["task_name"]
                for item in affected_tasks
                if item.get("impact_type") == "direct"
            )
            if referencing_tasks:
                blockers.append(
                    {
                        "reason": "TASK_REFERENCES_DELETED_COLUMN",
                        "message": (
                            "One or more rollback tasks still reference the "
                            "deleted column."
                        ),
                        "deleted_column": deleted_column,
                        "tasks": referencing_tasks,
                    }
                )

        if blockers:
            result = {
                "ok": False,
                "code": "E_ROLLBACK_BLOCKED",
                "error_code": "E_ROLLBACK_BLOCKED",
                "message": (
                    "The rollback plan is blocked and cannot be emitted."
                ),
                "blocked": True,
                "change_id": change_id,
                "rollback_plan_id": "rb_" + sha256_text(change_id)[:20],
                "rollback_tasks": [],
                "recheck_metrics": recheck_metrics,
                "blocking_reasons": blockers,
            }
            with self._write_lock:
                conn = self._connect()
                try:
                    conn.execute("BEGIN IMMEDIATE")
                    existing = conn.execute(
                        "SELECT * FROM rollback_plans WHERE change_id = ?",
                        (change_id,),
                    ).fetchone()
                    if existing is None:
                        now = utc_now()
                        conn.execute(
                            "INSERT INTO rollback_plans ("
                            "change_id, status, result_json, created_at"
                            ") VALUES (?, ?, ?, ?)",
                            (
                                change_id,
                                "E_ROLLBACK_BLOCKED",
                                canonical_json(result),
                                now,
                            ),
                        )
                        self._audit(
                            conn,
                            change_id,
                            "rollback_blocked",
                            result,
                            now,
                        )
                    conn.commit()
                except BaseException:
                    conn.rollback()
                    raise
                finally:
                    conn.close()
            raise BusinessError(
                "E_ROLLBACK_BLOCKED",
                result["message"],
                {
                    key: result[key]
                    for key in (
                        "change_id",
                        "rollback_plan_id",
                        "blocked",
                        "rollback_tasks",
                        "recheck_metrics",
                        "blocking_reasons",
                    )
                },
                http_status=409,
            )

        rollback_tasks = []
        for index, item in enumerate(
            impact.get("rollback_order", []), 1
        ):
            rollback_tasks.append(
                {
                    "rollback_step": index,
                    "task_name": item["task_name"],
                    "task_order": item["task_order"],
                    "output_table": item["output_table"],
                    "dependency_depth": item.get("dependency_depth"),
                    "impact_type": item["impact_type"],
                }
            )
        plan = {
            "ok": True,
            "code": "OK",
            "change_id": change_id,
            "rollback_plan_id": "rb_" + sha256_text(change_id)[:20],
            "status": "READY",
            "blocked": False,
            "direction": "reverse_dependency",
            "rollback_tasks": rollback_tasks,
            "rollback_order": rollback_tasks,
            "recheck_metrics": recheck_metrics,
            "affected_task_count": len(rollback_tasks),
        }
        with self._write_lock:
            conn = self._connect()
            try:
                conn.execute("BEGIN IMMEDIATE")
                existing = conn.execute(
                    "SELECT * FROM rollback_plans WHERE change_id = ?",
                    (change_id,),
                ).fetchone()
                if existing is not None:
                    stored = _safe_json_loads(
                        existing["result_json"], plan
                    )
                    conn.commit()
                    return 200, stored
                now = utc_now()
                conn.execute(
                    "INSERT INTO rollback_plans ("
                    "change_id, status, result_json, created_at"
                    ") VALUES (?, ?, ?, ?)",
                    (
                        change_id,
                        "READY",
                        canonical_json(plan),
                        now,
                    ),
                )
                self._audit(
                    conn,
                    change_id,
                    "rollback_planned",
                    {
                        "change_id": change_id,
                        "rollback_plan_id": plan["rollback_plan_id"],
                        "affected_task_count": len(rollback_tasks),
                        "recheck_metrics": [
                            metric["metric_id"]
                            for metric in recheck_metrics
                        ],
                    },
                    now,
                )
                conn.commit()
                return 200, plan
            except BaseException:
                conn.rollback()
                raise
            finally:
                conn.close()

    def get_rollback_plan(
        self, change_id: str
    ) -> tuple[int, dict[str, Any]]:
        return self._rollback_plan_request(change_id)

    def get_audit(
        self, change_id: str
    ) -> tuple[int, dict[str, Any]]:
        self._ensure_ready()
        conn = self._connect()
        try:
            change_row = self._get_change_row(conn, change_id)
            if change_row is None:
                raise BusinessError(
                    "E_NOT_FOUND",
                    "The change identifier does not exist.",
                    {"change_id": change_id},
                    http_status=404,
                )
            impact_rows = list(
                conn.execute(
                    "SELECT * FROM impact_results WHERE change_id = ? "
                    "ORDER BY created_at",
                    (change_id,),
                )
            )
            drift_rows = list(
                conn.execute(
                    "SELECT * FROM drift_checks WHERE change_id = ? "
                    "ORDER BY created_at, drift_id",
                    (change_id,),
                )
            )
            rollback_rows = list(
                conn.execute(
                    "SELECT * FROM rollback_plans WHERE change_id = ? "
                    "ORDER BY created_at",
                    (change_id,),
                )
            )
            event_rows = list(
                conn.execute(
                    "SELECT event_id, event_type, event_json, created_at "
                    "FROM audit_events WHERE change_id = ? "
                    "ORDER BY event_id",
                    (change_id,),
                )
            )
            return 200, {
                "ok": True,
                "code": "OK",
                "change_id": change_id,
                "change": _public_change(
                    change_row, self._change_from_row(change_row)
                ),
                "impact_history": [
                    {
                        "created_at": row["created_at"],
                        "result": _safe_json_loads(
                            row["result_json"], {}
                        ),
                    }
                    for row in impact_rows
                ],
                "drift_history": [
                    {
                        "drift_check_id": row["drift_id"],
                        "threshold_pct": float(row["threshold_pct"]),
                        "status": row["status"],
                        "created_at": row["created_at"],
                        "result": _safe_json_loads(
                            row["result_json"], {}
                        ),
                    }
                    for row in drift_rows
                ],
                "rollback_history": [
                    {
                        "status": row["status"],
                        "created_at": row["created_at"],
                        "result": _safe_json_loads(
                            row["result_json"], {}
                        ),
                    }
                    for row in rollback_rows
                ],
                "audit_events": [
                    {
                        "event_id": row["event_id"],
                        "event_type": row["event_type"],
                        "created_at": row["created_at"],
                        "payload": _safe_json_loads(
                            row["event_json"], {}
                        ),
                    }
                    for row in event_rows
                ],
            }
        finally:
            conn.close()

    def get_change(self, change_id: str) -> tuple[int, dict[str, Any]]:
        _, impact_response = self.get_impact(change_id)
        conn = self._connect()
        try:
            drift_row = conn.execute(
                "SELECT * FROM drift_checks WHERE change_id = ? "
                "ORDER BY created_at DESC, drift_id DESC LIMIT 1",
                (change_id,),
            ).fetchone()
            rollback_row = conn.execute(
                "SELECT * FROM rollback_plans WHERE change_id = ?",
                (change_id,),
            ).fetchone()
        finally:
            conn.close()
        result = {
            key: impact_response[key]
            for key in (
                "ok",
                "code",
                "change_id",
                "status",
                "change",
                "impact",
                "summary",
                "created_at",
            )
        }
        result["latest_drift"] = (
            _safe_json_loads(drift_row["result_json"], {})
            if drift_row is not None
            else None
        )
        if drift_row is not None:
            result["latest_drift"]["drift_check_id"] = drift_row[
                "drift_id"
            ]
        result["rollback_plan"] = (
            _safe_json_loads(rollback_row["result_json"], {})
            if rollback_row is not None
            else None
        )
        return 200, result

    def get_task(self, task_name: str) -> tuple[int, dict[str, Any]]:
        self._ensure_ready()
        assert self.metadata is not None
        task = next(
            (
                item
                for item in self.metadata.tasks
                if item.task_name.lower() == task_name.lower()
            ),
            None,
        )
        if task is None:
            raise BusinessError(
                "E_UNKNOWN_TASK",
                "The requested task does not exist.",
                {"object_type": "task", "object_name": task_name},
                http_status=400,
            )
        return 200, {
            "ok": True,
            "code": "OK",
            "task": {
                **task.public(),
                "read_columns": [
                    f"{table}.{column}"
                    for table, column in sorted(task.read_columns)
                ],
                "write_columns": sorted(task.write_columns),
                "dependencies": sorted(task.dependencies),
            },
        }

    def get_table(self, table_name: str) -> tuple[int, dict[str, Any]]:
        self._ensure_ready()
        assert self.metadata is not None
        canonical = self.metadata.find_table(table_name)
        if canonical is None:
            raise BusinessError(
                "E_UNKNOWN_TABLE",
                "The requested table does not exist.",
                {"object_type": "table", "object_name": table_name},
                http_status=400,
            )
        table = self.metadata.schema[canonical]
        writers = [
            task.task_name
            for task in self.metadata.tasks
            if task.output_table == canonical
        ]
        readers = [
            task.task_name
            for task in self.metadata.tasks
            if canonical in task.reads
        ]
        return 200, {
            "ok": True,
            "code": "OK",
            "table": {
                "table_name": canonical,
                "layer": table["layer"],
                "description": table["description"],
                "row_count": table["row_count"],
                "columns": [
                    {"column_name": name, "data_type": data_type}
                    for name, data_type in table["columns"].items()
                ],
                "writers": writers,
                "readers": readers,
            },
        }

    def api_documentation(self) -> dict[str, Any]:
        return {
            "ok": True,
            "code": "OK",
            "service": "warehouse-change-impact",
            "protocol": "HTTP/1.1",
            "endpoints": {
                "health": "GET /health",
                "submit_change": "POST /api/changes",
                "impact": "GET /api/changes/{change_id}/impact",
                "drift_check": "POST /api/changes/{change_id}/drift-check",
                "latest_drift": "GET /api/changes/{change_id}/drift-check",
                "rollback": "GET /api/changes/{change_id}/rollback-plan",
                "audit": "GET /api/changes/{change_id}/audit",
                "task": "GET /api/tasks/{task_name}",
                "table": "GET /api/tables/{table_name}",
            },
            "change_example": {
                "table_name": "detail_order_items",
                "column_name": "unit_price",
                "change_type": "type_narrow",
                "new_type": "INTEGER",
                "threshold_pct": 0.5,
            },
        }


class ServiceHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 32


class RequestHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "WarehouseImpact/1.0"
    sys_version = ""

    service: ImpactService
    request_slots = threading.BoundedSemaphore(6)

    def log_message(self, fmt: str, *args: Any) -> None:
        LOGGER.info("%s - %s", self.address_string(), fmt % args)

    def do_GET(self) -> None:
        self._handle("GET")

    def do_POST(self) -> None:
        self._handle("POST")

    def do_PUT(self) -> None:
        self._method_not_allowed()

    def do_DELETE(self) -> None:
        self._method_not_allowed()

    def do_PATCH(self) -> None:
        self._method_not_allowed()

    def do_OPTIONS(self) -> None:
        self.send_response(HTTPStatus.NO_CONTENT)
        self.send_header("Allow", "GET, POST, OPTIONS")
        self.send_header("Content-Length", "0")
        self.end_headers()

    def _method_not_allowed(self) -> None:
        self._send_json(
            HTTPStatus.METHOD_NOT_ALLOWED,
            BusinessError(
                "E_METHOD_NOT_ALLOWED",
                "HTTP method is not supported.",
                {"method": self.command},
                http_status=405,
            ).as_dict(),
        )

    def _read_body(self) -> Any:
        transfer_encoding = self.headers.get("Transfer-Encoding")
        if transfer_encoding:
            raise BusinessError(
                "E_INVALID_REQUEST",
                "Chunked request bodies are not supported.",
                {"field": "Transfer-Encoding"},
            )
        length_header = self.headers.get("Content-Length")
        if length_header is None:
            return {}
        try:
            length = int(length_header)
        except ValueError as error:
            raise BusinessError(
                "E_INVALID_REQUEST",
                "Content-Length is invalid.",
                {"field": "Content-Length"},
            ) from error
        if length < 0 or length > MAX_BODY_BYTES:
            raise BusinessError(
                "E_REQUEST_TOO_LARGE",
                "The request body is too large.",
                {"max_bytes": MAX_BODY_BYTES},
                http_status=413,
            )
        if length == 0:
            return {}
        raw = self.rfile.read(length)
        try:
            parsed = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            raise BusinessError(
                "E_INVALID_JSON",
                "The request body is not valid UTF-8 JSON.",
                {"field": "body"},
            ) from error
        return parsed

    def _handle(self, method: str) -> None:
        acquired = self.request_slots.acquire(timeout=30)
        if not acquired:
            self._send_json(
                HTTPStatus.SERVICE_UNAVAILABLE,
                BusinessError(
                    "E_BUSY",
                    "All request slots are busy.",
                    {},
                    http_status=503,
                ).as_dict(),
            )
            return
        try:
            parsed_path = urlparse(self.path)
            query = {
                key: values[-1]
                for key, values in parse_qs(
                    parsed_path.query, keep_blank_values=True
                ).items()
            }
            payload = self._read_body() if method == "POST" else {}
            status, body = self._dispatch(
                method,
                unquote(parsed_path.path),
                query,
                payload,
            )
            self._send_json(status, body)
        except BusinessError as error:
            self._send_json(error.http_status, error.as_dict())
        except (BrokenPipeError, ConnectionResetError):
            LOGGER.info("Client disconnected before the response was sent")
        except Exception:
            LOGGER.error(
                "Unhandled request error:\n%s", traceback.format_exc()
            )
            self._send_json(
                HTTPStatus.INTERNAL_SERVER_ERROR,
                BusinessError(
                    "E_INTERNAL",
                    "An internal server error occurred.",
                    {},
                    http_status=500,
                ).as_dict(),
            )
        finally:
            self.request_slots.release()

    def _send_json(self, status: int, body: dict[str, Any]) -> None:
        try:
            encoded = json.dumps(
                body,
                ensure_ascii=False,
                allow_nan=False,
                separators=(",", ":"),
            ).encode("utf-8")
        except (TypeError, ValueError):
            status = 500
            encoded = canonical_json(
                BusinessError(
                    "E_INTERNAL",
                    "An internal server error occurred.",
                    {},
                    http_status=500,
                ).as_dict()
            ).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(encoded)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(encoded)

    def _dispatch(
        self,
        method: str,
        path: str,
        query: dict[str, str],
        payload: Any,
    ) -> tuple[int, dict[str, Any]]:
        segments = [
            segment
            for segment in path.strip("/").split("/")
            if segment
        ]
        if segments and segments[0] == "api":
            segments = segments[1:]
        if segments and segments[0] == "v1":
            segments = segments[1:]

        if not segments:
            if method != "GET":
                raise BusinessError(
                    "E_METHOD_NOT_ALLOWED",
                    "HTTP method is not supported.",
                    {"method": method},
                    http_status=405,
                )
            return 200, self.service.api_documentation()
        if segments == ["health"] or segments == ["healthz"]:
            if method != "GET":
                raise BusinessError(
                    "E_METHOD_NOT_ALLOWED",
                    "HTTP method is not supported.",
                    {"method": method},
                    http_status=405,
                )
            return self.service.health()
        if segments == ["openapi.json"]:
            if method != "GET":
                raise BusinessError(
                    "E_METHOD_NOT_ALLOWED",
                    "HTTP method is not supported.",
                    {"method": method},
                    http_status=405,
                )
            return 200, self.service.api_documentation()
        if segments in (["changes"], ["change"]):
            if method == "POST":
                return self.service.submit_change(payload)
            if method == "GET":
                change_id = query.get("change_id")
                if change_id:
                    return self.service.get_change(change_id)
            raise BusinessError(
                "E_METHOD_NOT_ALLOWED",
                "Use POST /api/changes to submit a change.",
                {"method": method},
                http_status=405,
            )
        if len(segments) == 1 and segments[0] in {
            "impact",
            "drift-check",
            "drift",
            "rollback-plan",
            "rollback",
            "audit",
        }:
            change_id = query.get("change_id")
            if not change_id:
                raise BusinessError(
                    "E_INVALID_REQUEST",
                    "change_id query parameter is required.",
                    {"field": "change_id"},
                )
            return self._change_action(
                segments[0], method, change_id, payload
            )
        if segments[0] == "changes":
            if len(segments) == 2 and method == "GET":
                return self.service.get_change(segments[1])
            if len(segments) == 3:
                return self._change_action(
                    segments[2], method, segments[1], payload
                )
        if segments[0] == "change":
            if len(segments) == 2 and method == "GET":
                return self.service.get_change(segments[1])
            if len(segments) == 3:
                return self._change_action(
                    segments[2], method, segments[1], payload
                )
        if segments[0] == "impact" and len(segments) == 2:
            return self._change_action(
                "impact", method, segments[1], payload
            )
        if segments[0] in {"drift", "drift-check"} and len(segments) == 2:
            return self._change_action(
                segments[0], method, segments[1], payload
            )
        if segments[0] in {"rollback", "rollback-plan"} and len(segments) == 2:
            return self._change_action(
                segments[0], method, segments[1], payload
            )
        if segments[0] == "audit" and len(segments) == 2:
            return self._change_action(
                "audit", method, segments[1], payload
            )
        if segments[0] == "tasks" and len(segments) == 2:
            if method != "GET":
                raise BusinessError(
                    "E_METHOD_NOT_ALLOWED",
                    "HTTP method is not supported.",
                    {"method": method},
                    http_status=405,
                )
            return self.service.get_task(segments[1])
        if segments[0] == "tables" and len(segments) == 2:
            if method != "GET":
                raise BusinessError(
                    "E_METHOD_NOT_ALLOWED",
                    "HTTP method is not supported.",
                    {"method": method},
                    http_status=405,
                )
            return self.service.get_table(segments[1])
        raise BusinessError(
            "E_NOT_FOUND",
            "The requested endpoint does not exist.",
            {"path": path},
            http_status=404,
        )

    def _change_action(
        self, action: str, method: str, change_id: str, payload: Any
    ) -> tuple[int, dict[str, Any]]:
        if action == "impact":
            if method not in {"GET", "POST"}:
                raise BusinessError(
                    "E_METHOD_NOT_ALLOWED",
                    "HTTP method is not supported.",
                    {"method": method},
                    http_status=405,
                )
            return self.service.get_impact(change_id)
        if action in {"drift", "drift-check"}:
            if method == "POST":
                return self.service.run_drift(change_id, payload)
            if method == "GET":
                return self.service.latest_drift(change_id)
        if action in {"rollback", "rollback-plan"}:
            if method == "GET":
                return self.service.get_rollback_plan(change_id)
            if method == "POST":
                return self.service.get_rollback_plan(change_id)
        if action == "audit":
            if method == "GET":
                return self.service.get_audit(change_id)
        raise BusinessError(
            "E_METHOD_NOT_ALLOWED",
            "HTTP method is not supported for this endpoint.",
            {"method": method, "action": action},
            http_status=405,
        )


def build_server(
    service: ImpactService, host: str, port: int
) -> ServiceHTTPServer:
    handler_class = type(
        "BoundRequestHandler",
        (RequestHandler,),
        {"service": service},
    )
    return ServiceHTTPServer((host, port), handler_class)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", default="metadata.db")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument(
        "--port", type=int, default=int(os.environ.get("PORT", "8008"))
    )
    parser.add_argument(
        "--log-level", default=os.environ.get("LOG_LEVEL", "INFO")
    )
    args = parser.parse_args()
    if args.port < 1 or args.port > 65535:
        parser.error("--port must be between 1 and 65535")
    logging.basicConfig(
        level=getattr(logging, args.log_level.upper(), logging.INFO),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    service = ImpactService(args.db)
    server = build_server(service, args.host, args.port)
    print(
        canonical_json(
            {
                "service": "warehouse-change-impact",
                "listen": f"http://{args.host}:{args.port}",
                "database": Path(args.db).name,
                "ready": service.ready,
                "fatal_error": (
                    service.fatal_error.code
                    if service.fatal_error is not None
                    else None
                ),
            }
        ),
        flush=True,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        LOGGER.info("Shutdown requested")
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
