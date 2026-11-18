from __future__ import annotations

import json
import sqlite3
from pathlib import Path
from typing import Any, Iterable

from .util import canonical_json


class DuplicateBatchError(Exception):
    code = "BATCH_ALREADY_EXISTS"
    http_status = 409
    message = "batch id already committed; committed batches are immutable"


class BatchNotFoundError(Exception):
    code = "BATCH_NOT_FOUND"
    http_status = 404
    message = "batch id was not found"


SCHEMA = """
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;

CREATE TABLE IF NOT EXISTS batches (
    batch_id TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    target_profile TEXT NOT NULL,
    target_json TEXT NOT NULL,
    batch_config_json TEXT NOT NULL,
    injection_json TEXT NOT NULL,
    summary_json TEXT NOT NULL,
    request_count INTEGER NOT NULL,
    completed_count INTEGER NOT NULL,
    unfinished_count INTEGER NOT NULL,
    error_json TEXT
);

CREATE TABLE IF NOT EXISTS requests (
    batch_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    seq INTEGER NOT NULL,
    path TEXT NOT NULL,
    method TEXT NOT NULL,
    body_size INTEGER NOT NULL,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    client_connection_id TEXT,
    proxy_downstream_connection_id TEXT,
    proxy_upstream_connection_id TEXT,
    upstream_connection_id TEXT,
    outcome TEXT NOT NULL,
    status_code INTEGER,
    attribution_type TEXT,
    attribution_hop TEXT,
    evidence_event_id INTEGER,
    injection_applied INTEGER NOT NULL,
    reuse_attempted INTEGER NOT NULL,
    reuse_failure INTEGER NOT NULL,
    missing_hops_json TEXT NOT NULL,
    coverage_json TEXT NOT NULL,
    details_json TEXT NOT NULL,
    PRIMARY KEY (batch_id, request_id),
    FOREIGN KEY (batch_id) REFERENCES batches(batch_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS events (
    event_id INTEGER NOT NULL,
    batch_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    hop TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    peer_connection_id TEXT,
    correlation_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    initiator TEXT NOT NULL,
    ts TEXT NOT NULL,
    ts_mono_ms INTEGER NOT NULL,
    details_json TEXT NOT NULL,
    PRIMARY KEY (batch_id, event_id),
    FOREIGN KEY (batch_id) REFERENCES batches(batch_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_requests_batch_seq
    ON requests(batch_id, seq);
CREATE INDEX IF NOT EXISTS idx_events_batch_request_ts
    ON events(batch_id, request_id, ts_mono_ms, event_id);
CREATE INDEX IF NOT EXISTS idx_events_connection
    ON events(batch_id, hop, connection_id, ts_mono_ms, event_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_one_close_per_hop
    ON events(batch_id, hop, connection_id, kind)
    WHERE kind IN ('close', 'reset');
"""


class Database:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.initialize()

    def connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(
            str(self.path),
            timeout=10,
            isolation_level=None,
            check_same_thread=False,
        )
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA foreign_keys=ON")
        connection.execute("PRAGMA busy_timeout=5000")
        return connection

    def initialize(self) -> None:
        connection = self.connect()
        try:
            connection.executescript(SCHEMA)
        finally:
            connection.close()

    def batch_exists(self, batch_id: str) -> bool:
        connection = self.connect()
        try:
            row = connection.execute(
                "SELECT 1 FROM batches WHERE batch_id = ?", (batch_id,)
            ).fetchone()
            return row is not None
        finally:
            connection.close()

    def commit_batch(
        self,
        *,
        batch: dict[str, Any],
        requests: Iterable[dict[str, Any]],
        events: Iterable[dict[str, Any]],
    ) -> None:
        connection = self.connect()
        try:
            connection.execute("BEGIN IMMEDIATE")
            existing = connection.execute(
                "SELECT 1 FROM batches WHERE batch_id = ?", (batch["batch_id"],)
            ).fetchone()
            if existing is not None:
                connection.rollback()
                raise DuplicateBatchError()

            connection.execute(
                """
                INSERT INTO batches (
                    batch_id, status, started_at, finished_at, target_profile,
                    target_json, batch_config_json, injection_json, summary_json,
                    request_count, completed_count, unfinished_count, error_json
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    batch["batch_id"],
                    batch["status"],
                    batch["started_at"],
                    batch.get("finished_at"),
                    batch["target_profile"],
                    canonical_json(batch["target"]),
                    canonical_json(batch["batch_config"]),
                    canonical_json(batch["injection"]),
                    canonical_json(batch["summary"]),
                    batch["request_count"],
                    batch["completed_count"],
                    batch["unfinished_count"],
                    canonical_json(batch.get("error")) if batch.get("error") else None,
                ),
            )
            for item in requests:
                connection.execute(
                    """
                    INSERT INTO requests (
                        batch_id, request_id, seq, path, method, body_size,
                        started_at, finished_at, client_connection_id,
                        proxy_downstream_connection_id,
                        proxy_upstream_connection_id, upstream_connection_id,
                        outcome, status_code, attribution_type, attribution_hop,
                        evidence_event_id, injection_applied, reuse_attempted,
                        reuse_failure, missing_hops_json, coverage_json,
                        details_json
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        item["batch_id"],
                        item["request_id"],
                        item["seq"],
                        item["path"],
                        item["method"],
                        item["body_size"],
                        item["started_at"],
                        item.get("finished_at"),
                        item.get("client_connection_id"),
                        item.get("proxy_downstream_connection_id"),
                        item.get("proxy_upstream_connection_id"),
                        item.get("upstream_connection_id"),
                        item["outcome"],
                        item.get("status_code"),
                        item.get("attribution_type"),
                        item.get("attribution_hop"),
                        item.get("evidence_event_id"),
                        1 if item.get("injection_applied") else 0,
                        1 if item.get("reuse_attempted") else 0,
                        1 if item.get("reuse_failure") else 0,
                        canonical_json(item["missing_hops"]),
                        canonical_json(item["coverage"]),
                        canonical_json(item.get("details", {})),
                    ),
                )
            for event in events:
                connection.execute(
                    """
                    INSERT INTO events (
                        event_id, batch_id, request_id, hop, connection_id,
                        peer_connection_id, correlation_id, kind, initiator,
                        ts, ts_mono_ms, details_json
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        event["event_id"],
                        event["batch_id"],
                        event["request_id"],
                        event["hop"],
                        event["connection_id"],
                        event.get("peer_connection_id"),
                        event["correlation_id"],
                        event["kind"],
                        event["initiator"],
                        event["ts"],
                        event["ts_mono_ms"],
                        event["details_json"],
                    ),
                )
            connection.commit()
        except Exception:
            if connection.in_transaction:
                connection.rollback()
            raise
        finally:
            connection.close()

    def get_batch(self, batch_id: str) -> dict[str, Any]:
        connection = self.connect()
        try:
            row = connection.execute(
                "SELECT * FROM batches WHERE batch_id = ?", (batch_id,)
            ).fetchone()
            if row is None:
                raise BatchNotFoundError()
            result = dict(row)
            for key in ("target_json", "batch_config_json", "injection_json", "summary_json"):
                result[key.removesuffix("_json")] = json.loads(result.pop(key))
            if result.get("error_json"):
                result["error"] = json.loads(result.pop("error_json"))
            else:
                result.pop("error_json", None)
            return result
        finally:
            connection.close()

    def get_requests(self, batch_id: str) -> list[dict[str, Any]]:
        connection = self.connect()
        try:
            rows = connection.execute(
                "SELECT * FROM requests WHERE batch_id = ? ORDER BY seq",
                (batch_id,),
            ).fetchall()
            result = []
            for row in rows:
                item = dict(row)
                for key in (
                    "missing_hops_json",
                    "coverage_json",
                    "details_json",
                ):
                    item[key.removesuffix("_json")] = json.loads(item.pop(key))
                for key in (
                    "injection_applied",
                    "reuse_attempted",
                    "reuse_failure",
                ):
                    item[key] = bool(item[key])
                result.append(item)
            return result
        finally:
            connection.close()

    def get_events(self, batch_id: str, request_ids: set[str] | None = None) -> list[dict[str, Any]]:
        connection = self.connect()
        try:
            if request_ids:
                placeholders = ",".join("?" for _ in request_ids)
                rows = connection.execute(
                    f"""
                    SELECT * FROM events
                    WHERE batch_id = ? AND request_id IN ({placeholders})
                    ORDER BY ts_mono_ms, event_id
                    """,
                    (batch_id, *sorted(request_ids)),
                ).fetchall()
            else:
                rows = connection.execute(
                    "SELECT * FROM events WHERE batch_id = ? ORDER BY ts_mono_ms, event_id",
                    (batch_id,),
                ).fetchall()
            result = []
            for row in rows:
                item = dict(row)
                item["details"] = json.loads(item.pop("details_json"))
                result.append(item)
            return result
        finally:
            connection.close()

    def list_batches(self, limit: int = 100) -> list[dict[str, Any]]:
        connection = self.connect()
        try:
            rows = connection.execute(
                """
                SELECT batch_id, status, started_at, finished_at, request_count,
                       completed_count, unfinished_count
                FROM batches ORDER BY started_at DESC LIMIT ?
                """,
                (limit,),
            ).fetchall()
            return [dict(row) for row in rows]
        finally:
            connection.close()
