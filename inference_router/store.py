from __future__ import annotations

import json
import sqlite3
import struct
import threading
import time
from pathlib import Path
from typing import Any


def _now_ms() -> int:
    return int(time.time() * 1000)


def _canonical(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def encode_features(features: list[float] | tuple[float, ...]) -> bytes:
    return struct.pack(f"<{len(features)}f", *[float(value) for value in features])


def decode_features(payload: bytes) -> list[float]:
    if len(payload) % 4 != 0:
        raise ValueError("invalid feature blob length")
    return list(struct.unpack(f"<{len(payload) // 4}f", payload))


class RequestStore:
    def __init__(self, db_path: str) -> None:
        Path(db_path).parent.mkdir(parents=True, exist_ok=True)
        self.db_path = db_path
        self._lock = threading.RLock()
        self._conn = sqlite3.connect(
            db_path,
            timeout=10.0,
            isolation_level=None,
            check_same_thread=False,
        )
        self._conn.row_factory = sqlite3.Row
        with self._lock:
            self._conn.execute("PRAGMA journal_mode=WAL")
            self._conn.execute("PRAGMA synchronous=NORMAL")
            self._conn.execute("PRAGMA busy_timeout=10000")
            self._conn.execute("PRAGMA temp_store=FILE")
            self._conn.execute("PRAGMA user_version=1")
            self._create_schema()

    def _create_schema(self) -> None:
        self._conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS requests (
                request_id TEXT PRIMARY KEY,
                session_id TEXT NOT NULL,
                features BLOB NOT NULL,
                status TEXT NOT NULL CHECK (status IN ('in_progress', 'succeeded', 'failed')),
                response_json TEXT,
                error_code TEXT,
                l1_replica_id TEXT,
                l2_replica_id TEXT,
                e2e_ms REAL,
                retry_switches INTEGER NOT NULL DEFAULT 0,
                created_at_ms INTEGER NOT NULL,
                updated_at_ms INTEGER NOT NULL
            );

            CREATE TABLE IF NOT EXISTS request_calls (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                request_id TEXT NOT NULL,
                layer_id TEXT NOT NULL,
                replica_id TEXT NOT NULL,
                session_id TEXT NOT NULL,
                attempt INTEGER NOT NULL,
                latency_ms REAL NOT NULL,
                timed_out INTEGER NOT NULL,
                outcome TEXT NOT NULL,
                error_code TEXT,
                created_at_ms INTEGER NOT NULL
            );

            CREATE INDEX IF NOT EXISTS idx_request_calls_request
                ON request_calls(request_id, id);

            CREATE TABLE IF NOT EXISTS events (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                event_type TEXT NOT NULL,
                layer_id TEXT,
                replica_id TEXT,
                request_id TEXT,
                details_json TEXT NOT NULL,
                created_at_ms INTEGER NOT NULL
            );

            CREATE INDEX IF NOT EXISTS idx_events_created
                ON events(created_at_ms, id);
            """
        )

    def close(self) -> None:
        with self._lock:
            self._conn.close()

    def claim_request(
        self,
        request_id: str,
        session_id: str,
        features: list[float],
    ) -> tuple[str, dict[str, Any] | None]:
        now = _now_ms()
        with self._lock:
            self._conn.execute("BEGIN IMMEDIATE")
            try:
                row = self._conn.execute(
                    "SELECT * FROM requests WHERE request_id = ?", (request_id,)
                ).fetchone()
                if row is None:
                    self._conn.execute(
                        """
                        INSERT INTO requests (
                            request_id, session_id, features, status,
                            created_at_ms, updated_at_ms
                        ) VALUES (?, ?, ?, 'in_progress', ?, ?)
                        """,
                        (request_id, session_id, encode_features(features), now, now),
                    )
                    self._conn.execute("COMMIT")
                    return "new", None
                self._conn.execute("COMMIT")
                state = "final" if row["status"] in ("succeeded", "failed") else "in_progress"
                return state, self._row_to_request(row)
            except Exception:
                self._conn.execute("ROLLBACK")
                raise

    def _row_to_request(self, row: sqlite3.Row) -> dict[str, Any]:
        response = json.loads(row["response_json"]) if row["response_json"] else None
        return {
            "request_id": row["request_id"],
            "session_id": row["session_id"],
            "features": decode_features(row["features"]),
            "status": row["status"],
            "response": response,
            "error_code": row["error_code"],
            "l1_replica_id": row["l1_replica_id"],
            "l2_replica_id": row["l2_replica_id"],
            "e2e_ms": row["e2e_ms"],
            "retry_switches": row["retry_switches"],
            "created_at_ms": row["created_at_ms"],
            "updated_at_ms": row["updated_at_ms"],
        }

    def get_request(self, request_id: str) -> dict[str, Any] | None:
        with self._lock:
            row = self._conn.execute(
                "SELECT * FROM requests WHERE request_id = ?", (request_id,)
            ).fetchone()
            if row is None:
                return None
            request = self._row_to_request(row)
            calls = [
                dict(item)
                for item in self._conn.execute(
                    "SELECT * FROM request_calls WHERE request_id = ? ORDER BY id",
                    (request_id,),
                ).fetchall()
            ]
            events = [
                {
                    "event_type": item["event_type"],
                    "layer_id": item["layer_id"],
                    "replica_id": item["replica_id"],
                    "request_id": item["request_id"],
                    "details": json.loads(item["details_json"]),
                    "created_at_ms": item["created_at_ms"],
                }
                for item in self._conn.execute(
                    "SELECT * FROM events WHERE request_id = ? ORDER BY id",
                    (request_id,),
                ).fetchall()
            ]
        for call in calls:
            call["timed_out"] = bool(call["timed_out"])
        request["calls"] = calls
        request["events"] = events
        return request

    def complete_request(
        self,
        request_id: str,
        *,
        ok: bool,
        response: dict[str, Any],
        calls: list[dict[str, Any]],
        events: list[dict[str, Any]],
    ) -> bool:
        now = _now_ms()
        status = "succeeded" if ok else "failed"
        response_json = _canonical(response)
        with self._lock:
            self._conn.execute("BEGIN IMMEDIATE")
            try:
                row = self._conn.execute(
                    "SELECT status FROM requests WHERE request_id = ?", (request_id,)
                ).fetchone()
                if row is None:
                    self._conn.execute("ROLLBACK")
                    return False
                if row["status"] != "in_progress":
                    self._conn.execute("ROLLBACK")
                    return False
                for call in calls:
                    self._conn.execute(
                        """
                        INSERT INTO request_calls (
                            request_id, layer_id, replica_id, session_id, attempt,
                            latency_ms, timed_out, outcome, error_code, created_at_ms
                        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                        """,
                        (
                            request_id,
                            call["layer_id"],
                            call["replica_id"],
                            call["session_id"],
                            int(call["attempt"]),
                            float(call["latency_ms"]),
                            1 if call["timed_out"] else 0,
                            call["outcome"],
                            call.get("error_code"),
                            now,
                        ),
                    )
                for event in events:
                    self._conn.execute(
                        """
                        INSERT INTO events (
                            event_type, layer_id, replica_id, request_id,
                            details_json, created_at_ms
                        ) VALUES (?, ?, ?, ?, ?, ?)
                        """,
                        (
                            event["event_type"],
                            event.get("layer_id"),
                            event.get("replica_id"),
                            request_id,
                            _canonical(event.get("details", {})),
                            int(event.get("created_at_ms", now)),
                        ),
                    )
                self._conn.execute(
                    """
                    UPDATE requests SET
                        status = ?, response_json = ?, error_code = ?,
                        l1_replica_id = ?, l2_replica_id = ?, e2e_ms = ?,
                        retry_switches = ?, updated_at_ms = ?
                    WHERE request_id = ? AND status = 'in_progress'
                    """,
                    (
                        status,
                        response_json,
                        None if ok else response.get("error_code"),
                        response.get("l1_replica_id"),
                        response.get("l2_replica_id"),
                        float(response.get("e2e_ms", 0.0)),
                        int(response.get("retry_switches", 0)),
                        now,
                        request_id,
                    ),
                )
                self._conn.execute("COMMIT")
                return True
            except Exception:
                self._conn.execute("ROLLBACK")
                raise

    def append_event(self, event: dict[str, Any]) -> None:
        now = _now_ms()
        with self._lock:
            self._conn.execute("BEGIN IMMEDIATE")
            try:
                self._conn.execute(
                    """
                    INSERT INTO events (
                        event_type, layer_id, replica_id, request_id,
                        details_json, created_at_ms
                    ) VALUES (?, ?, ?, ?, ?, ?)
                    """,
                    (
                        event["event_type"],
                        event.get("layer_id"),
                        event.get("replica_id"),
                        event.get("request_id"),
                        _canonical(event.get("details", {})),
                        int(event.get("created_at_ms", now)),
                    ),
                )
                self._conn.execute("COMMIT")
            except Exception:
                self._conn.execute("ROLLBACK")
                raise

    def pending_requests(self) -> list[dict[str, Any]]:
        with self._lock:
            rows = self._conn.execute(
                "SELECT * FROM requests WHERE status = 'in_progress' ORDER BY created_at_ms, request_id"
            ).fetchall()
            return [self._row_to_request(row) for row in rows]

    def schema_version(self) -> int:
        with self._lock:
            row = self._conn.execute("PRAGMA user_version").fetchone()
            return int(row[0])
