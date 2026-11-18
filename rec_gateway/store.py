from __future__ import annotations

import json
import sqlite3
import threading
from pathlib import Path
from typing import Any


SCHEMA = """
CREATE TABLE IF NOT EXISTS routing_policy (
    tenant_id TEXT PRIMARY KEY,
    canary_weight REAL NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS policy_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id TEXT NOT NULL,
    previous_weight REAL NOT NULL,
    new_weight REAL NOT NULL,
    source TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS route_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    client_id TEXT,
    client_keyed INTEGER NOT NULL,
    protocol_version TEXT,
    client_bucket INTEGER NOT NULL,
    canary_weight REAL NOT NULL,
    candidate_version TEXT NOT NULL,
    selected_version TEXT NOT NULL,
    served_version TEXT,
    outcome TEXT NOT NULL,
    error_code TEXT,
    fallback_reason TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_route_tenant_client
    ON route_events(tenant_id, client_id, id);
CREATE TABLE IF NOT EXISTS inference_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    version TEXT NOT NULL,
    protocol_version TEXT,
    outcome TEXT NOT NULL,
    latency_ms REAL NOT NULL,
    error_code TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_inference_tenant_version
    ON inference_events(tenant_id, version, id);
CREATE TABLE IF NOT EXISTS rollback_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    version TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    metric_name TEXT NOT NULL,
    observed REAL NOT NULL,
    threshold REAL NOT NULL,
    window_samples INTEGER NOT NULL,
    previous_weights_json TEXT NOT NULL,
    triggered_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS idempotency (
    tenant_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    status_code INTEGER NOT NULL,
    response_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (tenant_id, request_id)
);
"""


class Store:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path).resolve()
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._connection = sqlite3.connect(
            str(self.path),
            check_same_thread=False,
            timeout=30.0,
        )
        self._connection.row_factory = sqlite3.Row
        with self._lock:
            self._connection.execute("PRAGMA journal_mode=WAL")
            self._connection.execute("PRAGMA synchronous=NORMAL")
            self._connection.execute("PRAGMA temp_store=MEMORY")
            self._connection.execute("PRAGMA busy_timeout=5000")
            self._connection.executescript(SCHEMA)
            self._connection.execute("PRAGMA user_version=1")
            self._connection.commit()

    def close(self) -> None:
        with self._lock:
            self._connection.close()

    def ensure_tenants(
        self, defaults: dict[str, float], now: str
    ) -> dict[str, float]:
        weights: dict[str, float] = {}
        with self._lock, self._connection:
            for tenant_id in sorted(defaults):
                row = self._connection.execute(
                    "SELECT canary_weight FROM routing_policy WHERE tenant_id = ?",
                    (tenant_id,),
                ).fetchone()
                if row is None:
                    weight = float(defaults[tenant_id])
                    self._connection.execute(
                        "INSERT INTO routing_policy(tenant_id, canary_weight, updated_at) "
                        "VALUES (?, ?, ?)",
                        (tenant_id, weight, now),
                    )
                    self._connection.execute(
                        "INSERT INTO policy_events("
                        "tenant_id, previous_weight, new_weight, source, created_at"
                        ") VALUES (?, ?, ?, ?, ?)",
                        (tenant_id, 0.0, weight, "bootstrap", now),
                    )
                else:
                    weight = float(row["canary_weight"])
                weights[tenant_id] = weight
        return weights

    def list_policies(self) -> list[dict[str, Any]]:
        with self._lock:
            rows = self._connection.execute(
                "SELECT tenant_id, canary_weight, updated_at "
                "FROM routing_policy ORDER BY tenant_id"
            ).fetchall()
        return [dict(row) for row in rows]

    def update_policy(
        self,
        tenant_id: str,
        previous_weight: float,
        new_weight: float,
        source: str,
        now: str,
    ) -> None:
        with self._lock, self._connection:
            self._connection.execute(
                "UPDATE routing_policy SET canary_weight = ?, updated_at = ? "
                "WHERE tenant_id = ?",
                (new_weight, now, tenant_id),
            )
            self._connection.execute(
                "INSERT INTO policy_events("
                "tenant_id, previous_weight, new_weight, source, created_at"
                ") VALUES (?, ?, ?, ?, ?)",
                (tenant_id, previous_weight, new_weight, source, now),
            )

    def update_all_policies(
        self,
        previous_weights: dict[str, float],
        new_weight: float,
        source: str,
        now: str,
    ) -> None:
        with self._lock, self._connection:
            for tenant_id in sorted(previous_weights):
                self._connection.execute(
                    "UPDATE routing_policy SET canary_weight = ?, updated_at = ? "
                    "WHERE tenant_id = ?",
                    (new_weight, now, tenant_id),
                )
                self._connection.execute(
                    "INSERT INTO policy_events("
                    "tenant_id, previous_weight, new_weight, source, created_at"
                    ") VALUES (?, ?, ?, ?, ?)",
                    (
                        tenant_id,
                        float(previous_weights[tenant_id]),
                        new_weight,
                        source,
                        now,
                    ),
                )

    def record_request(
        self,
        route_event: dict[str, Any],
        inference_event: dict[str, Any],
        idempotency: dict[str, Any] | None = None,
    ) -> None:
        with self._lock, self._connection:
            self._connection.execute(
                "INSERT INTO route_events("
                "request_id, tenant_id, client_id, client_keyed, protocol_version, "
                "client_bucket, canary_weight, candidate_version, selected_version, "
                "served_version, outcome, error_code, fallback_reason, created_at"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    route_event["request_id"],
                    route_event["tenant_id"],
                    route_event.get("client_id"),
                    int(bool(route_event.get("client_keyed", False))),
                    route_event.get("protocol_version"),
                    int(route_event["client_bucket"]),
                    float(route_event["canary_weight"]),
                    route_event["candidate_version"],
                    route_event["selected_version"],
                    route_event.get("served_version"),
                    route_event["outcome"],
                    route_event.get("error_code"),
                    route_event.get("fallback_reason"),
                    route_event["created_at"],
                ),
            )
            self._connection.execute(
                "INSERT INTO inference_events("
                "request_id, tenant_id, version, protocol_version, outcome, "
                "latency_ms, error_code, created_at"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    inference_event["request_id"],
                    inference_event["tenant_id"],
                    inference_event["version"],
                    inference_event.get("protocol_version"),
                    inference_event["outcome"],
                    float(inference_event["latency_ms"]),
                    inference_event.get("error_code"),
                    inference_event["created_at"],
                ),
            )
            if idempotency is not None:
                self._connection.execute(
                    "INSERT OR IGNORE INTO idempotency("
                    "tenant_id, request_id, request_hash, status_code, "
                    "response_json, created_at"
                    ") VALUES (?, ?, ?, ?, ?, ?)",
                    (
                        idempotency["tenant_id"],
                        idempotency["request_id"],
                        idempotency["request_hash"],
                        int(idempotency["status_code"]),
                        idempotency["response_json"],
                        idempotency["created_at"],
                    ),
                )

    def get_idempotency(
        self, tenant_id: str, request_id: str
    ) -> dict[str, Any] | None:
        with self._lock:
            row = self._connection.execute(
                "SELECT tenant_id, request_id, request_hash, status_code, "
                "response_json, created_at FROM idempotency "
                "WHERE tenant_id = ? AND request_id = ?",
                (tenant_id, request_id),
            ).fetchone()
        return dict(row) if row else None

    def route_history(
        self,
        tenant_id: str | None = None,
        client_id: str | None = None,
        limit: int = 50,
    ) -> list[dict[str, Any]]:
        clauses: list[str] = []
        arguments: list[Any] = []
        if tenant_id is not None:
            clauses.append("tenant_id = ?")
            arguments.append(tenant_id)
        if client_id is not None:
            clauses.append("client_id = ?")
            arguments.append(client_id)
        where = f"WHERE {' AND '.join(clauses)}" if clauses else ""
        arguments.append(max(1, min(int(limit), 500)))
        with self._lock:
            rows = self._connection.execute(
                "SELECT request_id, tenant_id, client_id, client_keyed, "
                "protocol_version, client_bucket, canary_weight, candidate_version, "
                "selected_version, served_version, outcome, error_code, "
                "fallback_reason, created_at "
                f"FROM route_events {where} ORDER BY id DESC LIMIT ?",
                arguments,
            ).fetchall()
        output = []
        for row in rows:
            item = dict(row)
            item["client_keyed"] = bool(item["client_keyed"])
            output.append(item)
        return output

    def aggregate_counts(self) -> list[dict[str, Any]]:
        with self._lock:
            rows = self._connection.execute(
                "SELECT version, tenant_id, COUNT(*) AS requests, "
                "SUM(CASE WHEN outcome != 'success' THEN 1 ELSE 0 END) AS errors, "
                "SUM(CASE WHEN outcome = 'timeout' THEN 1 ELSE 0 END) AS timeouts, "
                "SUM(CASE WHEN error_code = 'FEATURE_PROTOCOL_MISMATCH' "
                "THEN 1 ELSE 0 END) AS protocol_mismatches "
                "FROM inference_events GROUP BY version, tenant_id"
            ).fetchall()
        return [dict(row) for row in rows]

    def recent_window(
        self, version: str, tenant_id: str, limit: int
    ) -> list[dict[str, Any]]:
        with self._lock:
            rows = self._connection.execute(
                "SELECT outcome, latency_ms FROM inference_events "
                "WHERE version = ? AND tenant_id = ? ORDER BY id DESC LIMIT ?",
                (version, tenant_id, max(1, int(limit))),
            ).fetchall()
        return [dict(row) for row in reversed(rows)]

    def record_rollback(
        self,
        version: str,
        tenant_id: str,
        reason: str,
        metric_name: str,
        observed: float,
        threshold: float,
        window_samples: int,
        previous_weights: dict[str, float],
        triggered_at: str,
    ) -> dict[str, Any]:
        previous_json = json.dumps(previous_weights, sort_keys=True)
        with self._lock, self._connection:
            cursor = self._connection.execute(
                "INSERT INTO rollback_events("
                "version, tenant_id, reason, metric_name, observed, threshold, "
                "window_samples, previous_weights_json, triggered_at"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    version,
                    tenant_id,
                    reason,
                    metric_name,
                    float(observed),
                    float(threshold),
                    int(window_samples),
                    previous_json,
                    triggered_at,
                ),
            )
            event_id = int(cursor.lastrowid)
        return {
            "id": event_id,
            "version": version,
            "tenant_id": tenant_id,
            "reason": reason,
            "metric": metric_name,
            "observed": float(observed),
            "threshold": float(threshold),
            "window_samples": int(window_samples),
            "previous_weights": previous_weights,
            "weights_after": {tenant: 0.0 for tenant in sorted(previous_weights)},
            "triggered_at": triggered_at,
        }

    def recent_rollbacks(self, limit: int = 20) -> list[dict[str, Any]]:
        with self._lock:
            rows = self._connection.execute(
                "SELECT id, version, tenant_id, reason, metric_name, observed, "
                "threshold, window_samples, previous_weights_json, triggered_at "
                "FROM rollback_events ORDER BY id DESC LIMIT ?",
                (max(1, min(int(limit), 200)),),
            ).fetchall()
        events = []
        for row in rows:
            previous = json.loads(row["previous_weights_json"])
            events.append(
                {
                    "id": row["id"],
                    "version": row["version"],
                    "tenant_id": row["tenant_id"],
                    "reason": row["reason"],
                    "metric": row["metric_name"],
                    "observed": row["observed"],
                    "threshold": row["threshold"],
                    "window_samples": row["window_samples"],
                    "previous_weights": previous,
                    "weights_after": {tenant: 0.0 for tenant in sorted(previous)},
                    "triggered_at": row["triggered_at"],
                }
            )
        return events
