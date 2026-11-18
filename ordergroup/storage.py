from __future__ import annotations

import json
import sqlite3
import threading
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Iterator


SCHEMA = """
CREATE TABLE IF NOT EXISTS members (
    member_id TEXT PRIMARY KEY,
    session_timeout_ms INTEGER NOT NULL,
    heartbeat_interval_ms INTEGER NOT NULL,
    status TEXT NOT NULL,
    joined_at_ms INTEGER NOT NULL,
    last_heartbeat_ms INTEGER NOT NULL,
    removed_at_ms INTEGER,
    removal_reason TEXT,
    incarnation INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS assignments (
    partition INTEGER PRIMARY KEY,
    member_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    assigned_at_ms INTEGER NOT NULL,
    state TEXT NOT NULL DEFAULT 'active',
    paused INTEGER NOT NULL DEFAULT 0,
    paused_at_ms INTEGER
);
CREATE INDEX IF NOT EXISTS idx_assignments_member ON assignments(member_id);

CREATE TABLE IF NOT EXISTS commits (
    partition INTEGER PRIMARY KEY,
    offset INTEGER NOT NULL,
    member_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS quarantine (
    partition INTEGER PRIMARY KEY,
    former_member_id TEXT,
    reason TEXT NOT NULL,
    since_ms INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS event_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,
    member_id TEXT,
    partition INTEGER,
    at_ms INTEGER NOT NULL,
    details_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_event_log_at ON event_log(at_ms);

CREATE TABLE IF NOT EXISTS rebalances (
    id TEXT PRIMARY KEY,
    reason TEXT NOT NULL,
    trigger_member TEXT,
    involved_json TEXT NOT NULL,
    suppression_reason TEXT,
    status TEXT NOT NULL,
    requested_at_ms INTEGER NOT NULL,
    scheduled_at_ms INTEGER,
    started_at_ms INTEGER,
    completed_at_ms INTEGER,
    generation INTEGER
);

CREATE TABLE IF NOT EXISTS counters (
    scope TEXT NOT NULL,
    name TEXT NOT NULL,
    labels_json TEXT NOT NULL,
    value INTEGER NOT NULL,
    PRIMARY KEY (scope, name, labels_json)
);

CREATE TABLE IF NOT EXISTS metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
"""


class Store:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.lock = threading.RLock()
        self.conn = sqlite3.connect(
            str(self.path),
            timeout=10.0,
            check_same_thread=False,
            isolation_level=None,
        )
        self.conn.row_factory = sqlite3.Row
        self.conn.execute("PRAGMA journal_mode=WAL")
        self.conn.execute("PRAGMA synchronous=NORMAL")
        self.conn.execute("PRAGMA busy_timeout=10000")
        self.conn.executescript(SCHEMA)

    def execute(self, sql: str, parameters: tuple[Any, ...] = ()) -> sqlite3.Cursor:
        with self.lock:
            return self.conn.execute(sql, parameters)

    def executemany(
        self, sql: str, parameters: list[tuple[Any, ...]]
    ) -> sqlite3.Cursor:
        with self.lock:
            return self.conn.executemany(sql, parameters)

    def fetchone(
        self, sql: str, parameters: tuple[Any, ...] = ()
    ) -> sqlite3.Row | None:
        with self.lock:
            return self.conn.execute(sql, parameters).fetchone()

    def fetchall(
        self, sql: str, parameters: tuple[Any, ...] = ()
    ) -> list[sqlite3.Row]:
        with self.lock:
            return list(self.conn.execute(sql, parameters).fetchall())

    @contextmanager
    def transaction(self) -> Iterator[sqlite3.Connection]:
        with self.lock:
            self.conn.execute("BEGIN IMMEDIATE")
            try:
                yield self.conn
            except BaseException:
                self.conn.rollback()
                raise
            else:
                self.conn.commit()

    def get_metadata(self, key: str, default: str | None = None) -> str | None:
        row = self.fetchone("SELECT value FROM metadata WHERE key = ?", (key,))
        return row["value"] if row else default

    def set_metadata(self, key: str, value: str) -> None:
        self.execute(
            """
            INSERT INTO metadata(key, value) VALUES(?, ?)
            ON CONFLICT(key) DO UPDATE SET value = excluded.value
            """,
            (key, value),
        )

    def append_event(
        self,
        kind: str,
        at_ms: int,
        member_id: str | None = None,
        partition: int | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        self.execute(
            """
            INSERT INTO event_log(kind, member_id, partition, at_ms, details_json)
            VALUES(?, ?, ?, ?, ?)
            """,
            (
                kind,
                member_id,
                partition,
                at_ms,
                json.dumps(details or {}, ensure_ascii=False, sort_keys=True),
            ),
        )

    def recent_events(self, limit: int = 100) -> list[dict[str, Any]]:
        rows = self.fetchall(
            """
            SELECT id, kind, member_id, partition, at_ms, details_json
            FROM event_log ORDER BY id DESC LIMIT ?
            """,
            (limit,),
        )
        result = []
        for row in rows:
            result.append(
                {
                    "id": row["id"],
                    "kind": row["kind"],
                    "member_id": row["member_id"],
                    "partition": row["partition"],
                    "at_ms": row["at_ms"],
                    "details": json.loads(row["details_json"]),
                }
            )
        return result

    def close(self) -> None:
        with self.lock:
            self.conn.close()

