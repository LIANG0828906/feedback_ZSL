from __future__ import annotations

import contextlib
import json
import sqlite3
import threading
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds")


def _json_default(value: Any) -> str:
    if isinstance(value, datetime):
        return value.astimezone(timezone.utc).isoformat(timespec="milliseconds")
    return str(value)


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        default=_json_default,
    )


def parse_json(value: str | None, default: Any) -> Any:
    if value is None:
        return default
    try:
        return json.loads(value)
    except (TypeError, json.JSONDecodeError):
        return default


SCHEMA = r"""
CREATE TABLE IF NOT EXISTS sync_cursor (
    source_site TEXT NOT NULL,
    target_site TEXT NOT NULL,
    last_change_id INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (source_site, target_site)
);

CREATE TABLE IF NOT EXISTS sync_ack_gap (
    source_site TEXT NOT NULL,
    target_site TEXT NOT NULL,
    change_id INTEGER NOT NULL,
    acked_at TEXT NOT NULL,
    PRIMARY KEY (source_site, target_site, change_id)
);

CREATE TABLE IF NOT EXISTS deliveries (
    delivery_id TEXT PRIMARY KEY,
    change_key TEXT NOT NULL,
    source_site TEXT NOT NULL,
    target_site TEXT NOT NULL,
    source_change_id INTEGER NOT NULL,
    table_name TEXT NOT NULL,
    row_id INTEGER NOT NULL,
    row_version INTEGER NOT NULL,
    duplicate_of TEXT,
    received_at TEXT NOT NULL,
    processed_at TEXT,
    outcome TEXT NOT NULL DEFAULT 'received'
);

CREATE INDEX IF NOT EXISTS deliveries_change_idx
    ON deliveries (change_key, received_at DESC);
CREATE INDEX IF NOT EXISTS deliveries_row_idx
    ON deliveries (table_name, row_id, received_at DESC);

CREATE TABLE IF NOT EXISTS applied_changes (
    audit_id INTEGER PRIMARY KEY AUTOINCREMENT,
    change_key TEXT NOT NULL UNIQUE,
    source_site TEXT NOT NULL,
    target_site TEXT NOT NULL,
    source_change_id INTEGER NOT NULL,
    table_name TEXT NOT NULL,
    row_id INTEGER NOT NULL,
    row_version INTEGER NOT NULL,
    outcome TEXT NOT NULL,
    stale_fields_json TEXT NOT NULL,
    field_decisions_json TEXT NOT NULL,
    old_row_json TEXT,
    new_row_json TEXT,
    points_delta INTEGER,
    points_merged INTEGER NOT NULL DEFAULT 0,
    status_overridden INTEGER NOT NULL DEFAULT 0,
    applied_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS applied_changes_row_idx
    ON applied_changes (table_name, row_id, audit_id DESC);
CREATE INDEX IF NOT EXISTS applied_changes_time_idx
    ON applied_changes (applied_at);

CREATE TABLE IF NOT EXISTS conflicts (
    conflict_id INTEGER PRIMARY KEY AUTOINCREMENT,
    change_key TEXT NOT NULL,
    source_site TEXT NOT NULL,
    target_site TEXT NOT NULL,
    table_name TEXT NOT NULL,
    row_id INTEGER NOT NULL,
    column_name TEXT NOT NULL,
    incoming_version INTEGER NOT NULL,
    current_version INTEGER NOT NULL,
    incoming_value_json TEXT,
    current_value_json TEXT,
    adopted_value_json TEXT,
    basis TEXT NOT NULL,
    basis_detail TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (change_key, column_name)
);

CREATE INDEX IF NOT EXISTS conflicts_row_time_idx
    ON conflicts (table_name, row_id, created_at DESC);

CREATE TABLE IF NOT EXISTS row_field_state (
    table_name TEXT NOT NULL,
    row_id INTEGER NOT NULL,
    column_name TEXT NOT NULL,
    value_json TEXT,
    last_change_key TEXT,
    basis TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (table_name, row_id, column_name)
);

CREATE TABLE IF NOT EXISTS validation_batches (
    batch_id TEXT PRIMARY KEY,
    scope TEXT NOT NULL,
    status TEXT NOT NULL,
    phase TEXT NOT NULL,
    started_at TEXT NOT NULL,
    completed_at TEXT,
    snapshot_at TEXT,
    watermarks_json TEXT NOT NULL,
    summary_json TEXT NOT NULL,
    last_row_type TEXT,
    last_row_id INTEGER,
    error TEXT
);

CREATE INDEX IF NOT EXISTS validation_batches_started_idx
    ON validation_batches (started_at DESC);

CREATE TABLE IF NOT EXISTS validation_snapshot (
    batch_id TEXT NOT NULL,
    site TEXT NOT NULL,
    row_type TEXT NOT NULL,
    row_id INTEGER NOT NULL,
    row_version INTEGER NOT NULL,
    points INTEGER,
    status TEXT,
    updated_at TEXT,
    PRIMARY KEY (batch_id, site, row_type, row_id)
);

CREATE TABLE IF NOT EXISTS validation_pending (
    batch_id TEXT NOT NULL,
    row_type TEXT NOT NULL,
    row_id INTEGER NOT NULL,
    source_site TEXT NOT NULL,
    min_change_id INTEGER NOT NULL,
    max_change_id INTEGER NOT NULL,
    change_count INTEGER NOT NULL,
    PRIMARY KEY (batch_id, row_type, row_id)
);

CREATE TABLE IF NOT EXISTS validation_mismatches (
    mismatch_id INTEGER PRIMARY KEY AUTOINCREMENT,
    batch_id TEXT NOT NULL,
    row_type TEXT NOT NULL,
    row_id INTEGER NOT NULL,
    column_name TEXT NOT NULL,
    east_value_json TEXT,
    south_value_json TEXT,
    category TEXT NOT NULL,
    reason TEXT NOT NULL,
    details_json TEXT NOT NULL,
    UNIQUE (batch_id, row_type, row_id, column_name)
);

CREATE INDEX IF NOT EXISTS validation_mismatches_batch_idx
    ON validation_mismatches (batch_id, category, row_type, row_id);
"""


class AuditStore:
    def __init__(self, path: str):
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._local = threading.local()
        self._init_lock = threading.Lock()
        self._initialized = False
        self._initialize()

    def _new_connection(self) -> sqlite3.Connection:
        conn = sqlite3.connect(
            self.path,
            timeout=5.0,
            isolation_level=None,
            check_same_thread=False,
        )
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA synchronous=NORMAL")
        conn.execute("PRAGMA busy_timeout=5000")
        conn.execute("PRAGMA foreign_keys=ON")
        conn.execute("PRAGMA temp_store=MEMORY")
        return conn

    def _initialize(self) -> None:
        with self._init_lock:
            if self._initialized:
                return
            conn = self._new_connection()
            try:
                conn.executescript(SCHEMA)
            finally:
                conn.close()
            self._initialized = True

    @property
    def conn(self) -> sqlite3.Connection:
        conn = getattr(self._local, "conn", None)
        if conn is None:
            conn = self._new_connection()
            self._local.conn = conn
        return conn

    @contextlib.contextmanager
    def transaction(self):
        conn = self.conn
        conn.execute("BEGIN IMMEDIATE")
        try:
            yield conn
        except BaseException:
            conn.rollback()
            raise
        else:
            conn.commit()

    def cursor_position(self, source_site: str, target_site: str) -> int:
        row = self.conn.execute(
            """
            SELECT last_change_id FROM sync_cursor
            WHERE source_site = ? AND target_site = ?
            """,
            (source_site, target_site),
        ).fetchone()
        return int(row["last_change_id"]) if row else 0

    def cursors(self) -> dict[str, int]:
        rows = self.conn.execute(
            "SELECT source_site, target_site, last_change_id FROM sync_cursor"
        ).fetchall()
        return {
            f"{row['source_site']}->{row['target_site']}": int(row["last_change_id"])
            for row in rows
        }

    def _ack_change_in_tx(
        self, conn: sqlite3.Connection, source_site: str, target_site: str, change_id: int
    ) -> int:
        now = utc_now()
        conn.execute(
            """
            INSERT INTO sync_cursor (source_site, target_site, last_change_id, updated_at)
            VALUES (?, ?, 0, ?)
            ON CONFLICT (source_site, target_site) DO NOTHING
            """,
            (source_site, target_site, now),
        )
        row = conn.execute(
            """
            SELECT last_change_id FROM sync_cursor
            WHERE source_site = ? AND target_site = ?
            """,
            (source_site, target_site),
        ).fetchone()
        last = int(row["last_change_id"])
        if change_id <= last:
            return last
        conn.execute(
            """
            INSERT OR IGNORE INTO sync_ack_gap
                (source_site, target_site, change_id, acked_at)
            VALUES (?, ?, ?, ?)
            """,
            (source_site, target_site, change_id, now),
        )
        while True:
            next_id = last + 1
            exists = conn.execute(
                """
                SELECT 1 FROM sync_ack_gap
                WHERE source_site = ? AND target_site = ? AND change_id = ?
                """,
                (source_site, target_site, next_id),
            ).fetchone()
            if not exists:
                break
            conn.execute(
                """
                DELETE FROM sync_ack_gap
                WHERE source_site = ? AND target_site = ? AND change_id = ?
                """,
                (source_site, target_site, next_id),
            )
            last = next_id
        conn.execute(
            """
            UPDATE sync_cursor
            SET last_change_id = ?, updated_at = ?
            WHERE source_site = ? AND target_site = ?
            """,
            (last, now, source_site, target_site),
        )
        return last

    def ack_change(
        self, source_site: str, target_site: str, change_id: int
    ) -> int:
        with self.transaction() as conn:
            return self._ack_change_in_tx(conn, source_site, target_site, change_id)

    def record_delivery(
        self,
        *,
        delivery_id: str,
        change_key: str,
        source_site: str,
        target_site: str,
        source_change_id: int,
        table_name: str,
        row_id: int,
        row_version: int,
        duplicate_of: str | None = None,
    ) -> None:
        self.conn.execute(
            """
            INSERT OR IGNORE INTO deliveries (
                delivery_id, change_key, source_site, target_site,
                source_change_id, table_name, row_id, row_version,
                duplicate_of, received_at, outcome
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'received')
            """,
            (
                delivery_id,
                change_key,
                source_site,
                target_site,
                source_change_id,
                table_name,
                row_id,
                row_version,
                duplicate_of,
                utc_now(),
            ),
        )

    def record_apply_bundle(
        self,
        *,
        delivery: dict[str, Any],
        event: dict[str, Any],
        conflicts: list[dict[str, Any]],
        field_updates: list[dict[str, Any]],
        duplicate: bool,
    ) -> None:
        now = utc_now()
        source_change_id = int(delivery["source_change_id"])
        source_site = str(delivery["source_site"])
        target_site = str(delivery["target_site"])
        with self.transaction() as conn:
            inserted = conn.execute(
                """
                INSERT OR IGNORE INTO applied_changes (
                    change_key, source_site, target_site, source_change_id,
                    table_name, row_id, row_version, outcome,
                    stale_fields_json, field_decisions_json,
                    old_row_json, new_row_json, points_delta,
                    points_merged, status_overridden, applied_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    event["change_key"],
                    event["origin_site"],
                    target_site,
                    int(event["source_change_id"]),
                    event["table_name"],
                    int(event["row_id"]),
                    int(event["row_version"]),
                    event["outcome"],
                    canonical_json(event.get("stale_fields", [])),
                    canonical_json(event.get("field_decisions", {})),
                    (
                        canonical_json(event["old_row"])
                        if event.get("old_row") is not None
                        else None
                    ),
                    (
                        canonical_json(event["new_row"])
                        if event.get("new_row") is not None
                        else None
                    ),
                    event.get("points_delta"),
                    1 if event.get("points_merged") else 0,
                    1 if event.get("status_overridden") else 0,
                    event.get("applied_at") or now,
                ),
            ).rowcount
            if inserted:
                for conflict in conflicts:
                    conn.execute(
                        """
                        INSERT OR IGNORE INTO conflicts (
                            change_key, source_site, target_site, table_name,
                            row_id, column_name, incoming_version, current_version,
                            incoming_value_json, current_value_json,
                            adopted_value_json, basis, basis_detail, created_at
                        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                        """,
                        (
                            conflict["change_key"],
                            conflict["source_site"],
                            target_site,
                            conflict["table_name"],
                            int(conflict["row_id"]),
                            conflict["column_name"],
                            int(conflict["incoming_version"]),
                            int(conflict["current_version"]),
                            canonical_json(conflict.get("incoming_value")),
                            canonical_json(conflict.get("current_value")),
                            canonical_json(conflict.get("adopted_value")),
                            conflict["basis"],
                            conflict["basis_detail"],
                            conflict.get("created_at") or now,
                        ),
                    )
                for update in field_updates:
                    conn.execute(
                        """
                        INSERT INTO row_field_state (
                            table_name, row_id, column_name, value_json,
                            last_change_key, basis, updated_at
                        ) VALUES (?, ?, ?, ?, ?, ?, ?)
                        ON CONFLICT (table_name, row_id, column_name) DO UPDATE
                        SET value_json = excluded.value_json,
                            last_change_key = excluded.last_change_key,
                            basis = excluded.basis,
                            updated_at = excluded.updated_at
                        """,
                        (
                            update["table_name"],
                            int(update["row_id"]),
                            update["column_name"],
                            canonical_json(update.get("value")),
                            update.get("last_change_key"),
                            update["basis"],
                            now,
                        ),
                    )
            conn.execute(
                """
                UPDATE deliveries
                SET processed_at = ?, outcome = ?,
                    duplicate_of = COALESCE(duplicate_of, ?)
                WHERE delivery_id = ?
                """,
                (
                    now,
                    "duplicate" if (duplicate or not inserted) else "applied",
                    delivery.get("duplicate_of") if duplicate else None,
                    delivery["delivery_id"],
                ),
            )
            self._ack_change_in_tx(conn, source_site, target_site, source_change_id)

    def recent_changes(
        self, table_name: str, row_id: int, limit: int = 30
    ) -> list[dict[str, Any]]:
        rows = self.conn.execute(
            """
            SELECT * FROM applied_changes
            WHERE table_name = ? AND row_id = ?
            ORDER BY audit_id DESC LIMIT ?
            """,
            (table_name, row_id, limit),
        ).fetchall()
        result: list[dict[str, Any]] = []
        for row in rows:
            result.append(
                {
                    "change_key": row["change_key"],
                    "source_site": row["source_site"],
                    "target_site": row["target_site"],
                    "source_change_id": row["source_change_id"],
                    "row_version": row["row_version"],
                    "outcome": row["outcome"],
                    "stale_fields": parse_json(row["stale_fields_json"], []),
                    "field_decisions": parse_json(row["field_decisions_json"], {}),
                    "old_row": parse_json(row["old_row_json"], None),
                    "new_row": parse_json(row["new_row_json"], None),
                    "points_delta": row["points_delta"],
                    "points_merged": bool(row["points_merged"]),
                    "status_overridden": bool(row["status_overridden"]),
                    "applied_at": row["applied_at"],
                }
            )
        return result

    def deliveries_for_row(
        self, table_name: str, row_id: int, limit: int = 50
    ) -> list[dict[str, Any]]:
        rows = self.conn.execute(
            """
            SELECT * FROM deliveries
            WHERE table_name = ? AND row_id = ?
            ORDER BY received_at DESC LIMIT ?
            """,
            (table_name, row_id, limit),
        ).fetchall()
        return [dict(row) for row in rows]

    def conflicts_for_row(
        self,
        table_name: str,
        row_id: int,
        *,
        from_time: str | None = None,
        to_time: str | None = None,
        limit: int = 50,
    ) -> list[dict[str, Any]]:
        clauses = ["table_name = ?", "row_id = ?"]
        params: list[Any] = [table_name, row_id]
        if from_time:
            clauses.append("created_at >= ?")
            params.append(from_time)
        if to_time:
            clauses.append("created_at <= ?")
            params.append(to_time)
        params.append(limit)
        rows = self.conn.execute(
            f"""
            SELECT * FROM conflicts WHERE {' AND '.join(clauses)}
            ORDER BY conflict_id DESC LIMIT ?
            """,
            params,
        ).fetchall()
        result = []
        for row in rows:
            item = dict(row)
            item["incoming_value"] = parse_json(item.pop("incoming_value_json"), None)
            item["current_value"] = parse_json(item.pop("current_value_json"), None)
            item["adopted_value"] = parse_json(item.pop("adopted_value_json"), None)
            result.append(item)
        return result

    def query_conflicts(
        self,
        *,
        table_name: str | None = None,
        row_id: int | None = None,
        from_time: str | None = None,
        to_time: str | None = None,
        limit: int = 100,
    ) -> list[dict[str, Any]]:
        clauses: list[str] = []
        params: list[Any] = []
        if table_name:
            clauses.append("table_name = ?")
            params.append(table_name)
        if row_id is not None:
            clauses.append("row_id = ?")
            params.append(row_id)
        if from_time:
            clauses.append("created_at >= ?")
            params.append(from_time)
        if to_time:
            clauses.append("created_at <= ?")
            params.append(to_time)
        where = f"WHERE {' AND '.join(clauses)}" if clauses else ""
        params.append(min(1000, max(1, limit)))
        rows = self.conn.execute(
            f"SELECT * FROM conflicts {where} ORDER BY conflict_id DESC LIMIT ?",
            params,
        ).fetchall()
        result = []
        for row in rows:
            item = dict(row)
            item["incoming_value"] = parse_json(item.pop("incoming_value_json"), None)
            item["current_value"] = parse_json(item.pop("current_value_json"), None)
            item["adopted_value"] = parse_json(item.pop("adopted_value_json"), None)
            result.append(item)
        return result

    def field_state(self, table_name: str, row_id: int) -> dict[str, Any]:
        rows = self.conn.execute(
            """
            SELECT * FROM row_field_state
            WHERE table_name = ? AND row_id = ?
            ORDER BY column_name
            """,
            (table_name, row_id),
        ).fetchall()
        return {
            row["column_name"]: {
                "value": parse_json(row["value_json"], None),
                "last_change_key": row["last_change_key"],
                "basis": row["basis"],
                "updated_at": row["updated_at"],
            }
            for row in rows
        }

    def counters(self) -> dict[str, int]:
        applied = self.conn.execute("SELECT COUNT(*) AS n FROM applied_changes").fetchone()["n"]
        stale = self.conn.execute(
            "SELECT COUNT(*) AS n FROM applied_changes WHERE outcome = 'partial_stale'"
        ).fetchone()["n"]
        conflicts = self.conn.execute("SELECT COUNT(*) AS n FROM conflicts").fetchone()["n"]
        merges = self.conn.execute(
            "SELECT COUNT(*) AS n FROM applied_changes WHERE points_merged = 1"
        ).fetchone()["n"]
        overrides = self.conn.execute(
            "SELECT COUNT(*) AS n FROM applied_changes WHERE status_overridden = 1"
        ).fetchone()["n"]
        return {
            "applied": int(applied),
            "stale": int(stale),
            "conflicts": int(conflicts),
            "point_merges": int(merges),
            "status_overrides": int(overrides),
        }

    def counters_by_site(self) -> dict[str, dict[str, int]]:
        result: dict[str, dict[str, int]] = {}
        rows = self.conn.execute(
            """
            SELECT target_site,
                   COUNT(*) AS applied,
                   SUM(CASE WHEN outcome = 'partial_stale' THEN 1 ELSE 0 END) AS stale,
                   SUM(CASE WHEN points_merged = 1 THEN 1 ELSE 0 END) AS point_merges,
                   SUM(CASE WHEN status_overridden = 1 THEN 1 ELSE 0 END)
                       AS status_overrides
            FROM applied_changes GROUP BY target_site
            """
        ).fetchall()
        for row in rows:
            result[row["target_site"]] = {
                "applied": int(row["applied"] or 0),
                "stale": int(row["stale"] or 0),
                "point_merges": int(row["point_merges"] or 0),
                "status_overrides": int(row["status_overrides"] or 0),
                "conflicts": 0,
            }
        conflicts = self.conn.execute(
            "SELECT target_site, COUNT(*) AS n FROM conflicts GROUP BY target_site"
        ).fetchall()
        for row in conflicts:
            result.setdefault(
                row["target_site"],
                {
                    "applied": 0,
                    "stale": 0,
                    "point_merges": 0,
                    "status_overrides": 0,
                    "conflicts": 0,
                },
            )["conflicts"] = int(row["n"])
        return result

    def create_validation_batch(
        self, batch_id: str, scope: str, watermarks: dict[str, Any]
    ) -> None:
        self.conn.execute(
            """
            INSERT INTO validation_batches (
                batch_id, scope, status, phase, started_at,
                watermarks_json, summary_json
            ) VALUES (?, ?, 'running', 'snapshot', ?, ?, '{}')
            """,
            (batch_id, scope, utc_now(), canonical_json(watermarks)),
        )

    def validation_batch(self, batch_id: str) -> dict[str, Any] | None:
        row = self.conn.execute(
            "SELECT * FROM validation_batches WHERE batch_id = ?", (batch_id,)
        ).fetchone()
        return self._batch_dict(row) if row else None

    def _batch_dict(self, row: sqlite3.Row) -> dict[str, Any]:
        item = dict(row)
        item["watermarks"] = parse_json(item.pop("watermarks_json"), {})
        item["summary"] = parse_json(item.pop("summary_json"), {})
        return item

    def latest_validation(self) -> dict[str, Any] | None:
        row = self.conn.execute(
            "SELECT * FROM validation_batches ORDER BY started_at DESC LIMIT 1"
        ).fetchone()
        return self._batch_dict(row) if row else None

    def running_validation_batches(self) -> list[dict[str, Any]]:
        rows = self.conn.execute(
            """
            SELECT * FROM validation_batches
            WHERE status IN ('running', 'snapshot_complete')
            ORDER BY started_at
            """
        ).fetchall()
        return [self._batch_dict(row) for row in rows]

    def set_validation_phase(self, batch_id: str, phase: str, snapshot_at: str | None = None) -> None:
        self.conn.execute(
            """
            UPDATE validation_batches
            SET phase = ?, status = 'snapshot_complete',
                snapshot_at = COALESCE(?, snapshot_at)
            WHERE batch_id = ? AND status = 'running'
            """,
            (phase, snapshot_at, batch_id),
        )

    def set_validation_watermarks(self, batch_id: str, watermarks: dict[str, Any]) -> None:
        self.conn.execute(
            """
            UPDATE validation_batches
            SET watermarks_json = ?
            WHERE batch_id = ? AND status != 'completed'
            """,
            (canonical_json(watermarks), batch_id),
        )

    def reset_validation_snapshot(self, batch_id: str) -> None:
        with self.transaction() as conn:
            conn.execute(
                "DELETE FROM validation_snapshot WHERE batch_id = ?", (batch_id,)
            )
            conn.execute(
                "DELETE FROM validation_pending WHERE batch_id = ?", (batch_id,)
            )
            conn.execute(
                """
                UPDATE validation_batches
                SET status = 'running', phase = 'snapshot',
                    last_row_type = NULL, last_row_id = NULL
                WHERE batch_id = ? AND status != 'completed'
                """,
                (batch_id,),
            )

    def add_snapshot_rows(self, batch_id: str, site: str, row_type: str, rows: list[dict[str, Any]]) -> None:
        if not rows:
            return
        self.conn.executemany(
            """
            INSERT OR REPLACE INTO validation_snapshot (
                batch_id, site, row_type, row_id, row_version,
                points, status, updated_at
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
            """,
            [
                (
                    batch_id,
                    site,
                    row_type,
                    int(row["row_id"]),
                    int(row["row_version"]),
                    int(row["points"]) if row.get("points") is not None else None,
                    row.get("status"),
                    row.get("updated_at").isoformat() if row.get("updated_at") else None,
                )
                for row in rows
            ],
        )

    def add_pending_rows(self, batch_id: str, rows: list[dict[str, Any]]) -> None:
        if not rows:
            return
        self.conn.executemany(
            """
            INSERT INTO validation_pending (
                batch_id, row_type, row_id, source_site,
                min_change_id, max_change_id, change_count
            ) VALUES (?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT (batch_id, row_type, row_id) DO UPDATE
            SET source_site = validation_pending.source_site || ',' || excluded.source_site,
                min_change_id = MIN(validation_pending.min_change_id, excluded.min_change_id),
                max_change_id = MAX(validation_pending.max_change_id, excluded.max_change_id),
                change_count = validation_pending.change_count + excluded.change_count
            """,
            [
                (
                    batch_id,
                    row["row_type"],
                    int(row["row_id"]),
                    row["source_site"],
                    int(row["min_change_id"]),
                    int(row["max_change_id"]),
                    int(row["change_count"]),
                )
                for row in rows
            ],
        )

    def snapshot_bounds(self, batch_id: str, row_type: str) -> tuple[int, int]:
        row = self.conn.execute(
            """
            SELECT MIN(row_id) AS lo, MAX(row_id) AS hi
            FROM validation_snapshot WHERE batch_id = ? AND row_type = ?
            """,
            (batch_id, row_type),
        ).fetchone()
        if not row or row["lo"] is None:
            return (0, -1)
        return (int(row["lo"]), int(row["hi"]))

    def snapshot_counts(self, batch_id: str) -> dict[str, int]:
        rows = self.conn.execute(
            """
            SELECT row_type, COUNT(DISTINCT row_id) AS n
            FROM validation_snapshot WHERE batch_id = ?
            GROUP BY row_type ORDER BY row_type
            """,
            (batch_id,),
        ).fetchall()
        return {row["row_type"]: int(row["n"]) for row in rows}

    def snapshot_page(
        self, batch_id: str, row_type: str, after_row_id: int, limit: int
    ) -> list[dict[str, Any]]:
        rows = self.conn.execute(
            """
            SELECT row_id FROM validation_snapshot
            WHERE batch_id = ? AND row_type = ? AND row_id > ?
            GROUP BY row_id
            ORDER BY row_id LIMIT ?
            """,
            (batch_id, row_type, after_row_id, limit),
        ).fetchall()
        ids = [int(row["row_id"]) for row in rows]
        if not ids:
            return []
        placeholders = ",".join("?" for _ in ids)
        site_rows = self.conn.execute(
            f"""
            SELECT * FROM validation_snapshot
            WHERE batch_id = ? AND row_type = ? AND row_id IN ({placeholders})
            """,
            [batch_id, row_type, *ids],
        ).fetchall()
        grouped: dict[int, dict[str, dict[str, Any]]] = {row_id: {} for row_id in ids}
        for row in site_rows:
            grouped[int(row["row_id"])][row["site"]] = dict(row)
        return [{"row_id": row_id, "sites": grouped[row_id]} for row_id in ids]

    def pending_for_rows(
        self, batch_id: str, row_type: str, row_ids: list[int]
    ) -> dict[int, dict[str, Any]]:
        if not row_ids:
            return {}
        placeholders = ",".join("?" for _ in row_ids)
        rows = self.conn.execute(
            f"""
            SELECT * FROM validation_pending
            WHERE batch_id = ? AND row_type = ? AND row_id IN ({placeholders})
            """,
            [batch_id, row_type, *row_ids],
        ).fetchall()
        return {int(row["row_id"]): dict(row) for row in rows}

    def add_mismatches(self, batch_id: str, rows: list[dict[str, Any]]) -> None:
        if not rows:
            return
        self.conn.executemany(
            """
            INSERT OR IGNORE INTO validation_mismatches (
                batch_id, row_type, row_id, column_name,
                east_value_json, south_value_json, category, reason, details_json
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            [
                (
                    batch_id,
                    row["row_type"],
                    int(row["row_id"]),
                    row["column_name"],
                    canonical_json(row.get("east_value")),
                    canonical_json(row.get("south_value")),
                    row["category"],
                    row["reason"],
                    canonical_json(row.get("details", {})),
                )
                for row in rows
            ],
        )

    def set_validation_checkpoint(self, batch_id: str, row_type: str, row_id: int) -> None:
        self.conn.execute(
            """
            UPDATE validation_batches
            SET last_row_type = ?, last_row_id = ?
            WHERE batch_id = ? AND status != 'completed'
            """,
            (row_type, row_id, batch_id),
        )

    def complete_validation(
        self, batch_id: str, summary: dict[str, Any], error: str | None = None
    ) -> bool:
        now = utc_now()
        rowcount = self.conn.execute(
            """
            UPDATE validation_batches
            SET status = 'completed', phase = 'complete', completed_at = ?,
                summary_json = ?, error = ?
            WHERE batch_id = ? AND status != 'completed'
            """,
            (now, canonical_json(summary), error, batch_id),
        ).rowcount
        self.conn.execute(
            "DELETE FROM validation_snapshot WHERE batch_id = ?", (batch_id,)
        )
        self.conn.execute(
            "DELETE FROM validation_pending WHERE batch_id = ?", (batch_id,)
        )
        return bool(rowcount)

    def mark_validation_error(self, batch_id: str, error: str) -> None:
        self.conn.execute(
            """
            UPDATE validation_batches SET error = ?
            WHERE batch_id = ? AND status != 'completed'
            """,
            (error, batch_id),
        )

    def validation_mismatches(
        self, batch_id: str, limit: int = 1000, category: str | None = None
    ) -> list[dict[str, Any]]:
        clauses = ["batch_id = ?"]
        params: list[Any] = [batch_id]
        if category:
            clauses.append("category = ?")
            params.append(category)
        params.append(min(10000, max(1, limit)))
        rows = self.conn.execute(
            f"""
            SELECT * FROM validation_mismatches
            WHERE {' AND '.join(clauses)}
            ORDER BY row_type, row_id, column_name LIMIT ?
            """,
            params,
        ).fetchall()
        result = []
        for row in rows:
            item = dict(row)
            item["east_value"] = parse_json(item.pop("east_value_json"), None)
            item["south_value"] = parse_json(item.pop("south_value_json"), None)
            item["details"] = parse_json(item.pop("details_json"), {})
            result.append(item)
        return result

    def validation_summary(self, batch_id: str) -> dict[str, Any]:
        row = self.conn.execute(
            """
            SELECT COUNT(*) AS mismatch_count,
                   COUNT(DISTINCT row_type || ':' || row_id) AS mismatch_rows,
                   SUM(CASE WHEN category = 'unconverged' THEN 1 ELSE 0 END)
                       AS unconverged_columns,
                   SUM(CASE WHEN category = 'true_conflict' THEN 1 ELSE 0 END)
                       AS true_conflict_columns
            FROM validation_mismatches WHERE batch_id = ?
            """,
            (batch_id,),
        ).fetchone()
        columns = self.conn.execute(
            """
            SELECT column_name, COUNT(*) AS count
            FROM validation_mismatches WHERE batch_id = ?
            GROUP BY column_name ORDER BY column_name
            """,
            (batch_id,),
        ).fetchall()
        categories = self.conn.execute(
            """
            SELECT category, COUNT(*) AS columns,
                   COUNT(DISTINCT row_type || ':' || row_id) AS rows
            FROM validation_mismatches WHERE batch_id = ?
            GROUP BY category ORDER BY category
            """,
            (batch_id,),
        ).fetchall()
        return {
            "mismatch_count": int(row["mismatch_count"] or 0),
            "mismatch_rows": int(row["mismatch_rows"] or 0),
            "unconverged_columns": int(row["unconverged_columns"] or 0),
            "true_conflict_columns": int(row["true_conflict_columns"] or 0),
            "by_column": {
                item["column_name"]: int(item["count"]) for item in columns
            },
            "by_category": {
                item["category"]: {
                    "columns": int(item["columns"]),
                    "rows": int(item["rows"]),
                }
                for item in categories
            },
        }

    def new_batch_id(self) -> str:
        return uuid.uuid4().hex

    def reset_all(self) -> None:
        """Clear lab audit state when both business databases are explicitly reset."""
        with self.transaction() as conn:
            for table in (
                "validation_mismatches",
                "validation_pending",
                "validation_snapshot",
                "validation_batches",
                "row_field_state",
                "conflicts",
                "applied_changes",
                "deliveries",
                "sync_ack_gap",
                "sync_cursor",
            ):
                conn.execute(f"DELETE FROM {table}")
            conn.execute(
                "DELETE FROM sqlite_sequence WHERE name IN "
                "('applied_changes', 'conflicts', 'validation_mismatches')"
            )
