from __future__ import annotations

import json
import sqlite3
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


def _json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), default=str)


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds")


@dataclass(frozen=True)
class AuditEvent:
    event_type: str
    business_code: str
    reason: str
    tenant_id: str | None = None
    from_state: str | None = None
    to_state: str | None = None
    metrics_snapshot: dict[str, Any] = field(default_factory=dict)
    thresholds: dict[str, Any] = field(default_factory=dict)
    sli_snapshot: dict[str, Any] = field(default_factory=dict)
    decision: dict[str, Any] = field(default_factory=dict)


class AuditWriter:
    """Append-only audit store.

    State transitions call append_many() and publish their in-memory state only
    after the transaction commits. A failed audit write therefore never leaves
    a state change without an audit record.
    """

    def __init__(self, path: Path, run_id: str) -> None:
        self.path = path
        self.run_id = run_id
        self._lock = threading.RLock()
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.ensure_schema()

    def _connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(
            self.path,
            timeout=5.0,
            isolation_level=None,
            check_same_thread=False,
        )
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA busy_timeout=5000")
        connection.execute("PRAGMA journal_mode=WAL")
        return connection

    def ensure_schema(self) -> None:
        with self._lock:
            connection = self._connect()
            try:
                connection.execute(
                    """
                    CREATE TABLE IF NOT EXISTS audit_events (
                        seq INTEGER PRIMARY KEY AUTOINCREMENT,
                        run_id TEXT NOT NULL,
                        event_type TEXT NOT NULL,
                        tenant_id TEXT,
                        from_state TEXT,
                        to_state TEXT,
                        business_code TEXT NOT NULL,
                        reason TEXT NOT NULL,
                        metrics_snapshot_json TEXT NOT NULL,
                        thresholds_json TEXT NOT NULL,
                        sli_snapshot_json TEXT NOT NULL,
                        decision_json TEXT NOT NULL,
                        created_at TEXT NOT NULL
                    )
                    """
                )
                connection.execute(
                    "CREATE INDEX IF NOT EXISTS idx_audit_run_seq ON audit_events(run_id, seq)"
                )
                connection.execute(
                    "CREATE INDEX IF NOT EXISTS idx_audit_type_time ON audit_events(event_type, created_at)"
                )
            finally:
                connection.close()

    def append_many(self, events: list[AuditEvent]) -> list[int]:
        if not events:
            return []
        last_error: sqlite3.OperationalError | None = None
        for attempt in range(12):
            with self._lock:
                connection = self._connect()
                try:
                    connection.execute("BEGIN IMMEDIATE")
                    sequence: list[int] = []
                    created_at = utc_now()
                    for event in events:
                        cursor = connection.execute(
                            """
                            INSERT INTO audit_events (
                                run_id, event_type, tenant_id, from_state, to_state,
                                business_code, reason, metrics_snapshot_json,
                                thresholds_json, sli_snapshot_json, decision_json,
                                created_at
                            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                            """,
                            (
                                self.run_id,
                                event.event_type,
                                event.tenant_id,
                                event.from_state,
                                event.to_state,
                                event.business_code,
                                event.reason,
                                _json(event.metrics_snapshot),
                                _json(event.thresholds),
                                _json(event.sli_snapshot),
                                _json(event.decision),
                                created_at,
                            ),
                        )
                        sequence.append(int(cursor.lastrowid))
                    connection.execute("COMMIT")
                    return sequence
                except sqlite3.OperationalError as exc:
                    try:
                        connection.execute("ROLLBACK")
                    except sqlite3.Error:
                        pass
                    last_error = exc
                    if "locked" not in str(exc).lower():
                        raise
                except Exception:
                    try:
                        connection.execute("ROLLBACK")
                    except sqlite3.Error:
                        pass
                    raise
                finally:
                    connection.close()
            time.sleep(min(0.2, 0.01 * (attempt + 1)))
        assert last_error is not None
        raise last_error

    def query(
        self,
        *,
        run_id: str | None = None,
        after_seq: int = 0,
        limit: int = 500,
    ) -> list[dict[str, Any]]:
        effective_run_id = run_id or self.run_id
        limit = max(1, min(int(limit), 5000))
        with self._lock:
            connection = self._connect()
            try:
                rows = connection.execute(
                    """
                    SELECT * FROM audit_events
                    WHERE run_id = ? AND seq > ?
                    ORDER BY seq ASC
                    LIMIT ?
                    """,
                    (effective_run_id, max(0, int(after_seq)), limit),
                ).fetchall()
            finally:
                connection.close()
        result: list[dict[str, Any]] = []
        for row in rows:
            item = dict(row)
            for key in (
                "metrics_snapshot_json",
                "thresholds_json",
                "sli_snapshot_json",
                "decision_json",
            ):
                raw = item.pop(key)
                item[key.removesuffix("_json")] = json.loads(raw)
            result.append(item)
        return result

    def count(self, run_id: str | None = None) -> int:
        with self._lock:
            connection = self._connect()
            try:
                row = connection.execute(
                    "SELECT COUNT(*) AS count FROM audit_events WHERE run_id = ?",
                    (run_id or self.run_id,),
                ).fetchone()
            finally:
                connection.close()
        return int(row["count"])
