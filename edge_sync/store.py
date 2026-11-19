"""Persistence for the sync service (shards, signals and switch events)."""

from __future__ import annotations

import json
import sqlite3
import threading
from pathlib import Path

SCHEMA = """
CREATE TABLE IF NOT EXISTS tasks (
    task_id      TEXT PRIMARY KEY,
    total_shards INTEGER NOT NULL,
    total_bytes  INTEGER NOT NULL,
    status       TEXT NOT NULL,
    created_ms   INTEGER NOT NULL,
    updated_ms   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS shards (
    task_id       TEXT NOT NULL,
    shard_seq     INTEGER NOT NULL,
    start_byte    INTEGER NOT NULL,
    end_byte      INTEGER NOT NULL,
    length        INTEGER NOT NULL,
    checksum      TEXT NOT NULL,
    planned_egress TEXT NOT NULL,
    final_egress  TEXT,
    status        TEXT NOT NULL,
    attempts      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (task_id, shard_seq)
);
CREATE TABLE IF NOT EXISTS attempts (
    task_id      TEXT NOT NULL,
    shard_seq    INTEGER NOT NULL,
    attempt_no   INTEGER NOT NULL,
    egress       TEXT NOT NULL,
    started_ms   INTEGER NOT NULL,
    ended_ms     INTEGER,
    success      INTEGER NOT NULL DEFAULT 0,
    duplicate    INTEGER NOT NULL DEFAULT 0,
    error_code   TEXT,
    signal_json  TEXT,
    link_json    TEXT,
    PRIMARY KEY (task_id, shard_seq, attempt_no)
);
CREATE TABLE IF NOT EXISTS switches (
    event_seq       INTEGER PRIMARY KEY AUTOINCREMENT,
    egress          TEXT NOT NULL,
    target_egress   TEXT,
    reason_metric   TEXT NOT NULL,
    observed_value  REAL NOT NULL,
    threshold_value REAL NOT NULL,
    shard_count     INTEGER NOT NULL,
    bad_samples     INTEGER NOT NULL,
    emulated_bad_ms REAL NOT NULL,
    wall_ms         INTEGER NOT NULL
);
"""


class SyncStore:
    def __init__(self, db_path: str):
        Path(db_path).parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self._connection = sqlite3.connect(db_path, check_same_thread=False)
        self._connection.execute("PRAGMA journal_mode=WAL")
        self._connection.execute("PRAGMA synchronous=NORMAL")
        self._connection.executescript(SCHEMA)
        self._connection.commit()

    def reset(self) -> None:
        with self._lock:
            for table in ("attempts", "shards", "switches", "tasks"):
                self._connection.execute(f"DELETE FROM {table}")
            self._connection.execute("DELETE FROM sqlite_sequence WHERE name='switches'")
            self._connection.commit()

    def upsert_task(self, task_id: str, total_shards: int, total_bytes: int, now_ms: int, status: str) -> None:
        with self._lock:
            self._connection.execute(
                "INSERT INTO tasks VALUES (?, ?, ?, ?, ?, ?) "
                "ON CONFLICT(task_id) DO UPDATE SET status=excluded.status, updated_ms=excluded.updated_ms",
                (task_id, total_shards, total_bytes, status, now_ms, now_ms),
            )
            self._connection.commit()

    def set_task_status(self, task_id: str, status: str, now_ms: int) -> None:
        with self._lock:
            self._connection.execute(
                "UPDATE tasks SET status=?, updated_ms=? WHERE task_id=?", (status, now_ms, task_id)
            )
            self._connection.commit()

    def insert_shard(self, shard: dict, planned_egress: str) -> None:
        with self._lock:
            self._connection.execute(
                "INSERT OR IGNORE INTO shards "
                "(task_id, shard_seq, start_byte, end_byte, length, checksum, planned_egress, status, attempts) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', 0)",
                (
                    shard["task_id"],
                    shard["shard_seq"],
                    shard["start_byte"],
                    shard["end_byte"],
                    shard["length"],
                    shard["checksum"],
                    planned_egress,
                ),
            )
            self._connection.commit()

    def claim_next(self, task_id: str, now_ms: int) -> dict | None:
        with self._lock:
            row = self._connection.execute(
                "SELECT task_id, shard_seq, start_byte, end_byte, length, checksum, planned_egress "
                "FROM shards WHERE task_id=? AND status='pending' ORDER BY shard_seq LIMIT 1",
                (task_id,),
            ).fetchone()
            if row is None:
                return None
            self._connection.execute(
                "UPDATE shards SET status='running', attempts=attempts+1 WHERE task_id=? AND shard_seq=?",
                (row[0], row[1]),
            )
            self._connection.commit()
        return {
            "task_id": row[0], "shard_seq": row[1], "start_byte": row[2], "end_byte": row[3],
            "length": row[4], "checksum": row[5], "planned_egress": row[6],
        }

    def requeue(self, task_id: str, shard_seq: int) -> None:
        with self._lock:
            self._connection.execute(
                "UPDATE shards SET status='pending' WHERE task_id=? AND shard_seq=? AND status='running'",
                (task_id, shard_seq),
            )
            self._connection.commit()

    def update_planned_egress(self, egress: str, target_egress: str) -> int:
        with self._lock:
            cursor = self._connection.execute(
                "UPDATE shards SET planned_egress=? WHERE planned_egress=? AND status='pending'",
                (target_egress, egress),
            )
            count = cursor.rowcount
            self._connection.commit()
            return count

    def finish_shard(self, task_id: str, shard_seq: int, egress: str, success: bool, error_code: str | None) -> None:
        with self._lock:
            self._connection.execute(
                "UPDATE shards SET status=?, final_egress=? WHERE task_id=? AND shard_seq=?",
                ("succeeded" if success else "failed", egress, task_id, shard_seq),
            )
            self._connection.commit()

    def record_attempt(self, attempt: dict) -> None:
        with self._lock:
            self._connection.execute(
                "INSERT OR REPLACE INTO attempts VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    attempt["task_id"],
                    attempt["shard_seq"],
                    attempt["attempt_no"],
                    attempt["egress"],
                    attempt["started_ms"],
                    attempt["ended_ms"],
                    1 if attempt.get("success") else 0,
                    1 if attempt.get("duplicate") else 0,
                    attempt.get("error_code"),
                    json.dumps(attempt.get("signal"), sort_keys=True) if attempt.get("signal") else None,
                    json.dumps(attempt.get("link"), sort_keys=True) if attempt.get("link") else None,
                ),
            )
            self._connection.commit()

    def record_switch(self, event: dict) -> int:
        with self._lock:
            cursor = self._connection.execute(
                "INSERT INTO switches "
                "(egress, target_egress, reason_metric, observed_value, threshold_value, "
                "shard_count, bad_samples, emulated_bad_ms, wall_ms) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    event["egress"],
                    event.get("target_egress"),
                    event["reason_metric"],
                    event["observed_value"],
                    event["threshold_value"],
                    event["shard_count"],
                    event["bad_samples"],
                    event["emulated_bad_ms"],
                    event["wall_ms"],
                ),
            )
            self._connection.commit()
            return int(cursor.lastrowid)

    def fetch_all(self, query: str, params=()):
        with self._lock:
            return self._connection.execute(query, params).fetchall()

    def task_shard_counts(self) -> dict[str, dict[str, int]]:
        rows = self.fetch_all("SELECT task_id, status, COUNT(*) FROM shards GROUP BY task_id, status")
        result: dict[str, dict[str, int]] = {}
        for task_id, status, count in rows:
            result.setdefault(task_id, {})[status] = count
        return result

    def close(self) -> None:
        with self._lock:
            self._connection.close()
