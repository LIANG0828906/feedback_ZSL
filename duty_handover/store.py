"""SQLite persistence for immutable handover snapshots."""

from __future__ import annotations

import json
import sqlite3
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterator

from .errors import (
    HandoverError,
    duplicate_id_error,
    duplicate_timestamp_error,
)


def stable_json(payload: Any) -> str:
    return json.dumps(
        payload,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    )


def _utc_now() -> str:
    text = datetime.now(timezone.utc).isoformat(timespec="microseconds")
    return text.replace("+00:00", "Z").replace(".000000Z", "Z")


class SnapshotStore:
    def __init__(self, db_path: str | Path) -> None:
        self.db_path = str(db_path)
        self._memory_connection: sqlite3.Connection | None = None
        if self.db_path != ":memory:":
            parent = Path(self.db_path).expanduser().resolve().parent
            parent.mkdir(parents=True, exist_ok=True)
        self.initialize()

    def close(self) -> None:
        if self._memory_connection is not None:
            self._memory_connection.close()
            self._memory_connection = None

    def __enter__(self) -> "SnapshotStore":
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        self.close()

    def _connect(self) -> sqlite3.Connection:
        if self.db_path == ":memory:":
            if self._memory_connection is None:
                self._memory_connection = sqlite3.connect(
                    ":memory:", timeout=10.0, check_same_thread=False
                )
                self._memory_connection.row_factory = sqlite3.Row
                self._memory_connection.execute("PRAGMA foreign_keys = ON")
                self._memory_connection.execute("PRAGMA busy_timeout = 10000")
            return self._memory_connection
        connection = sqlite3.connect(self.db_path, timeout=10.0)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA foreign_keys = ON")
        connection.execute("PRAGMA busy_timeout = 10000")
        return connection

    @contextmanager
    def _connection(self) -> Iterator[sqlite3.Connection]:
        connection = self._connect()
        try:
            yield connection
        finally:
            if self.db_path != ":memory:":
                connection.close()

    def initialize(self) -> None:
        with self._connection() as connection:
            connection.execute("PRAGMA journal_mode = WAL")
            connection.executescript(
                """
                CREATE TABLE IF NOT EXISTS schema_version (
                    version INTEGER NOT NULL
                );

                CREATE TABLE IF NOT EXISTS snapshots (
                    snapshot_id INTEGER PRIMARY KEY AUTOINCREMENT,
                    handover_id TEXT NOT NULL UNIQUE,
                    handover_at TEXT NOT NULL UNIQUE,
                    snapshot_json TEXT NOT NULL,
                    created_at TEXT NOT NULL
                );

                CREATE INDEX IF NOT EXISTS idx_snapshots_handover_at
                    ON snapshots(handover_at DESC, snapshot_id DESC);

                CREATE TABLE IF NOT EXISTS rejected_submissions (
                    rejection_id INTEGER PRIMARY KEY AUTOINCREMENT,
                    handover_id TEXT,
                    handover_at TEXT,
                    error_code TEXT NOT NULL,
                    message TEXT NOT NULL,
                    details_json TEXT NOT NULL,
                    rejected_at TEXT NOT NULL
                );

                CREATE INDEX IF NOT EXISTS idx_rejections_time
                    ON rejected_submissions(rejected_at DESC, rejection_id DESC);

                CREATE TRIGGER IF NOT EXISTS snapshots_no_update
                BEFORE UPDATE ON snapshots
                BEGIN
                    SELECT RAISE(ABORT, 'snapshot_immutable');
                END;

                CREATE TRIGGER IF NOT EXISTS snapshots_no_delete
                BEFORE DELETE ON snapshots
                BEGIN
                    SELECT RAISE(ABORT, 'snapshot_immutable');
                END;
                """
            )
            existing = connection.execute(
                "SELECT version FROM schema_version LIMIT 1"
            ).fetchone()
            if existing is None:
                connection.execute("INSERT INTO schema_version(version) VALUES (1)")
            connection.commit()

    def _record_from_row(self, row: sqlite3.Row) -> dict[str, Any]:
        snapshot = json.loads(row["snapshot_json"])
        return {
            "snapshot_id": int(row["snapshot_id"]),
            "handover_id": row["handover_id"],
            "handover_at": row["handover_at"],
            "created_at": row["created_at"],
            "snapshot": snapshot,
        }

    def record_rejection(
        self,
        *,
        handover_id: str | None,
        handover_at: str | None,
        error: HandoverError,
    ) -> None:
        try:
            with self._connection() as connection:
                connection.execute(
                    """
                    INSERT INTO rejected_submissions(
                        handover_id, handover_at, error_code, message,
                        details_json, rejected_at
                    ) VALUES (?, ?, ?, ?, ?, ?)
                    """,
                    (
                        handover_id,
                        handover_at,
                        error.code,
                        error.message,
                        stable_json(error.to_dict()),
                        _utc_now(),
                    ),
                )
                connection.commit()
        except sqlite3.Error:
            return

    def insert_snapshot(self, snapshot: dict[str, Any]) -> dict[str, Any]:
        handover_id = str(snapshot["handover_id"])
        handover_at = str(snapshot["handover_at"])
        created_at = _utc_now()
        connection = self._connect()
        try:
            connection.execute("BEGIN IMMEDIATE")
            timestamp_row = connection.execute(
                "SELECT handover_id FROM snapshots WHERE handover_at = ?",
                (handover_at,),
            ).fetchone()
            if timestamp_row is not None:
                error = duplicate_timestamp_error(str(timestamp_row["handover_id"]))
                connection.rollback()
                self.record_rejection(
                    handover_id=handover_id,
                    handover_at=handover_at,
                    error=error,
                )
                raise error
            id_row = connection.execute(
                "SELECT handover_at FROM snapshots WHERE handover_id = ?",
                (handover_id,),
            ).fetchone()
            if id_row is not None:
                error = duplicate_id_error(str(id_row["handover_at"]))
                connection.rollback()
                self.record_rejection(
                    handover_id=handover_id,
                    handover_at=handover_at,
                    error=error,
                )
                raise error
            cursor = connection.execute(
                """
                INSERT INTO snapshots(
                    handover_id, handover_at, snapshot_json, created_at
                ) VALUES (?, ?, ?, ?)
                """,
                (handover_id, handover_at, stable_json(snapshot), created_at),
            )
            snapshot_id = int(cursor.lastrowid)
            connection.commit()
            return {
                "snapshot_id": snapshot_id,
                "handover_id": handover_id,
                "handover_at": handover_at,
                "created_at": created_at,
                "snapshot": snapshot,
            }
        except HandoverError:
            raise
        except sqlite3.IntegrityError as exc:
            connection.rollback()
            timestamp_row = connection.execute(
                "SELECT handover_id FROM snapshots WHERE handover_at = ?",
                (handover_at,),
            ).fetchone()
            if timestamp_row is not None:
                error = duplicate_timestamp_error(str(timestamp_row["handover_id"]))
            else:
                error = duplicate_id_error(handover_at)
            self.record_rejection(
                handover_id=handover_id,
                handover_at=handover_at,
                error=error,
            )
            raise error from exc
        finally:
            if self.db_path != ":memory:":
                connection.close()

    def get_snapshot(self, handover_id: str) -> dict[str, Any]:
        with self._connection() as connection:
            row = connection.execute(
                "SELECT * FROM snapshots WHERE handover_id = ?",
                (handover_id,),
            ).fetchone()
        if row is None:
            raise HandoverError(
                "SNAPSHOT_NOT_FOUND",
                "No committed snapshot exists for this handover identifier.",
                http_status=404,
                details={"handover_id": handover_id},
            )
        return self._record_from_row(row)

    def get_latest_snapshot(self) -> dict[str, Any] | None:
        with self._connection() as connection:
            row = connection.execute(
                """
                SELECT * FROM snapshots
                ORDER BY handover_at DESC, snapshot_id DESC
                LIMIT 1
                """
            ).fetchone()
        return self._record_from_row(row) if row is not None else None

    def list_snapshots(self, limit: int = 100) -> list[dict[str, Any]]:
        safe_limit = max(1, min(int(limit), 1000))
        with self._connection() as connection:
            rows = connection.execute(
                """
                SELECT snapshot_id, handover_id, handover_at, created_at,
                       snapshot_json
                FROM snapshots
                ORDER BY handover_at DESC, snapshot_id DESC
                LIMIT ?
                """,
                (safe_limit,),
            ).fetchall()
        result: list[dict[str, Any]] = []
        for row in rows:
            snapshot = json.loads(row["snapshot_json"])
            result.append(
                {
                    "snapshot_id": int(row["snapshot_id"]),
                    "handover_id": row["handover_id"],
                    "handover_at": row["handover_at"],
                    "created_at": row["created_at"],
                    "overall_grade": snapshot["decision"]["summary"][
                        "overall_grade"
                    ],
                    "actual_grade": snapshot["decision"]["summary"]["actual_grade"],
                    "affected_services": snapshot["decision"]["summary"][
                        "affected_services"
                    ],
                    "silence_decision": snapshot["decision"]["summary"][
                        "silence_decision"
                    ],
                }
            )
        return result

    def snapshot_count(self) -> int:
        with self._connection() as connection:
            row = connection.execute("SELECT COUNT(*) AS count FROM snapshots").fetchone()
        return int(row["count"])

    def iter_snapshots(self) -> Iterator[dict[str, Any]]:
        with self._connection() as connection:
            rows = connection.execute(
                """
                SELECT * FROM snapshots
                ORDER BY handover_at ASC, snapshot_id ASC
                """
            ).fetchall()
        for row in rows:
            yield self._record_from_row(row)

    def latest_rejection(self) -> dict[str, Any] | None:
        with self._connection() as connection:
            row = connection.execute(
                """
                SELECT * FROM rejected_submissions
                ORDER BY rejected_at DESC, rejection_id DESC
                LIMIT 1
                """
            ).fetchone()
        if row is None:
            return None
        details = json.loads(row["details_json"])
        return {
            "handover_id": row["handover_id"],
            "handover_at": row["handover_at"],
            "error_code": row["error_code"],
            "message": row["message"],
            "details": details.get("details", {}),
            "rejected_at": row["rejected_at"],
        }
