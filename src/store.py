"""SQLite persistence for rounds, slots, relief, resync progress and samples."""
from __future__ import annotations

import sqlite3
import threading
from pathlib import Path
from typing import Any

SCHEMA = """
CREATE TABLE IF NOT EXISTS rounds (
    round_id      TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    status        TEXT NOT NULL,
    started_at    REAL NOT NULL,
    finished_at   REAL,
    detail        TEXT
);
CREATE TABLE IF NOT EXISTS slot_snapshots (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    round_id         TEXT NOT NULL,
    instance         TEXT NOT NULL,
    slot_name        TEXT NOT NULL,
    plugin           TEXT,
    slot_type        TEXT,
    active           INTEGER NOT NULL,
    restart_lsn      TEXT,
    confirmed_lsn    TEXT,
    current_lsn      TEXT,
    lag_bytes        INTEGER,
    wal_retention    INTEGER,
    pid              INTEGER,
    subscription     TEXT,
    inactive_since   TEXT,
    inactive_seconds REAL,
    UNIQUE(round_id, instance, slot_name)
);
CREATE TABLE IF NOT EXISTS relief_advices (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    slot_name         TEXT NOT NULL,
    instance          TEXT NOT NULL,
    subscriber_id     TEXT,
    level             TEXT NOT NULL,
    status            TEXT NOT NULL,
    restart_lsn       TEXT,
    confirmed_lsn     TEXT,
    wal_retention     INTEGER,
    recheck_count     INTEGER NOT NULL DEFAULT 0,
    deferred_reclaim  INTEGER NOT NULL DEFAULT 0,
    created_round_id  TEXT NOT NULL,
    created_at        REAL NOT NULL,
    updated_round_id  TEXT,
    updated_at        REAL,
    detail            TEXT
);
CREATE TABLE IF NOT EXISTS resync_jobs (
    job_id                TEXT PRIMARY KEY,
    status                TEXT NOT NULL,
    stage                 TEXT NOT NULL DEFAULT 'prepare',
    trigger_reason        TEXT,
    snapshot_lsn          TEXT,
    previous_snapshot_lsn TEXT,
    baseline_at           REAL,
    created_at            REAL NOT NULL,
    updated_at            REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS resync_table_progress (
    job_id         TEXT NOT NULL,
    table_name     TEXT NOT NULL,
    status         TEXT NOT NULL,
    phase          TEXT NOT NULL DEFAULT 'bulk',
    last_key       INTEGER,
    delta_last_key INTEGER,
    rows_copied    INTEGER NOT NULL DEFAULT 0,
    updated_at     REAL NOT NULL,
    PRIMARY KEY (job_id, table_name)
);
CREATE TABLE IF NOT EXISTS offset_corrections (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    round_id         TEXT NOT NULL,
    slot_name        TEXT NOT NULL,
    redis_lsn        TEXT,
    applied_lsn      TEXT NOT NULL,
    verdict          TEXT NOT NULL,
    source_of_truth  TEXT NOT NULL,
    corrected_lsn    TEXT NOT NULL,
    basis            TEXT,
    created_at       REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS sample_mismatches (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    round_id      TEXT NOT NULL,
    table_name    TEXT NOT NULL,
    primary_key   TEXT NOT NULL,
    column_name   TEXT NOT NULL,
    primary_val   TEXT,
    replica_val   TEXT,
    created_at    REAL NOT NULL,
    UNIQUE(round_id, table_name, primary_key, column_name)
);
CREATE TABLE IF NOT EXISTS query_errors (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    round_id    TEXT,
    instance    TEXT NOT NULL,
    target      TEXT NOT NULL,
    code        TEXT NOT NULL,
    message     TEXT NOT NULL,
    created_at  REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS meta_counters (
    name   TEXT PRIMARY KEY,
    value  INTEGER NOT NULL
);
"""

MIGRATIONS = {
    "slot_snapshots": {
        "inactive_since": "TEXT",
        "inactive_seconds": "REAL",
    },
    "relief_advices": {
        "confirmed_lsn": "TEXT",
        "recheck_count": "INTEGER NOT NULL DEFAULT 0",
        "deferred_reclaim": "INTEGER NOT NULL DEFAULT 0",
    },
    "resync_jobs": {
        "stage": "TEXT NOT NULL DEFAULT 'prepare'",
        "previous_snapshot_lsn": "TEXT",
        "baseline_at": "REAL",
    },
    "resync_table_progress": {
        "phase": "TEXT NOT NULL DEFAULT 'bulk'",
        "delta_last_key": "INTEGER",
    },
    "offset_corrections": {
        "basis": "TEXT",
    },
}


class Store:
    def __init__(self, path: str):
        Path(path).parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self.conn = sqlite3.connect(
            path, check_same_thread=False, timeout=30
        )
        self.conn.row_factory = sqlite3.Row
        self.conn.execute("PRAGMA journal_mode=WAL")
        self.conn.execute("PRAGMA synchronous=NORMAL")
        self.conn.execute("PRAGMA busy_timeout=30000")
        self.conn.executescript(SCHEMA)
        self._migrate()
        self.conn.commit()

    def _migrate(self) -> None:
        for table, columns in MIGRATIONS.items():
            existing = {
                row["name"]
                for row in self.conn.execute(f"PRAGMA table_info({table})")
            }
            for name, ddl in columns.items():
                if name not in existing:
                    self.conn.execute(f"ALTER TABLE {table} ADD COLUMN {name} {ddl}")
        # Older databases may contain duplicate pending rows. Keep the newest
        # one before enforcing the partial unique index.
        self.conn.execute(
            "DELETE FROM relief_advices WHERE status='pending' AND id NOT IN ("
            "SELECT max(id) FROM relief_advices WHERE status='pending' "
            "GROUP BY slot_name, instance)"
        )
        self.conn.execute(
            "CREATE UNIQUE INDEX IF NOT EXISTS uq_relief_pending "
            "ON relief_advices(slot_name, instance) WHERE status='pending'"
        )

    def close(self) -> None:
        with self._lock:
            self.conn.commit()
            self.conn.close()

    # ---- rounds ---------------------------------------------------------
    def begin_round(self, round_id: str, kind: str, started_at: float) -> bool:
        """Persist a durable running marker.

        A running marker can be resumed after a process restart.  A completed
        round is immutable and returns False.
        """
        with self._lock:
            cur = self.conn.execute(
                "INSERT OR IGNORE INTO rounds(round_id, kind, status, started_at) "
                "VALUES (?,?, 'running', ?)",
                (round_id, kind, started_at),
            )
            created = cur.rowcount == 1
            self.conn.commit()
            if created:
                return True
            row = self.conn.execute(
                "SELECT kind, status FROM rounds WHERE round_id=?", (round_id,)
            ).fetchone()
            return bool(
                row and row["kind"] == kind and row["status"] == "running"
            )

    def round_status(self, round_id: str) -> str | None:
        with self._lock:
            row = self.conn.execute(
                "SELECT status FROM rounds WHERE round_id=?", (round_id,)
            ).fetchone()
            return row["status"] if row else None

    def finish_round(
        self, round_id: str, status: str, finished_at: float,
        detail: str | None = None,
    ) -> None:
        """Atomically publish conclusions and mark the round finished."""
        with self._lock:
            self.conn.execute(
                "UPDATE rounds SET status=?, finished_at=?, detail=? "
                "WHERE round_id=? AND status='running'",
                (status, finished_at, detail, round_id),
            )
            self.conn.commit()

    def rollback(self) -> None:
        with self._lock:
            self.conn.rollback()

    def list_incomplete_rounds(self) -> list[sqlite3.Row]:
        with self._lock:
            return self.conn.execute(
                "SELECT * FROM rounds WHERE status='running' "
                "ORDER BY started_at"
            ).fetchall()

    def _require_running(self, round_id: str) -> None:
        if self.round_status(round_id) != "running":
            raise ValueError(
                f"round {round_id} is not running; conclusions are immutable"
            )

    # ---- monitor conclusions -------------------------------------------
    def insert_slot_snapshot(self, row: dict[str, Any]) -> None:
        self._require_running(row["round_id"])
        cols = (
            "round_id", "instance", "slot_name", "plugin", "slot_type", "active",
            "restart_lsn", "confirmed_lsn", "current_lsn", "lag_bytes",
            "wal_retention", "pid", "subscription", "inactive_since",
            "inactive_seconds",
        )
        placeholders = ",".join("?" for _ in cols)
        with self._lock:
            self.conn.execute(
                f"INSERT OR REPLACE INTO slot_snapshots({','.join(cols)}) "
                f"VALUES ({placeholders})",
                tuple(row.get(c) for c in cols),
            )

    def insert_query_error(
        self, round_id: str, instance: str, target: str, code: str,
        message: str, created_at: float,
    ) -> None:
        self._require_running(round_id)
        with self._lock:
            self.conn.execute(
                "INSERT INTO query_errors(round_id, instance, target, code, "
                "message, created_at) VALUES (?,?,?,?,?,?)",
                (round_id, instance, target, code, message, created_at),
            )

    def upsert_relief_advice(
        self, slot_name: str, instance: str, subscriber_id: str, level: str,
        status: str, restart_lsn: str | None, confirmed_lsn: str | None,
        wal_retention: int, recheck_count: int, deferred_reclaim: bool,
        round_id: str, now: float, detail: str | None = None,
    ) -> None:
        with self._lock:
            row = self.conn.execute(
                "SELECT id FROM relief_advices WHERE slot_name=? AND instance=? "
                "AND status='pending' ORDER BY id DESC LIMIT 1",
                (slot_name, instance),
            ).fetchone()
            if row:
                self.conn.execute(
                    "UPDATE relief_advices SET level=?, status=?, subscriber_id=?, "
                    "restart_lsn=?, confirmed_lsn=?, wal_retention=?, "
                    "recheck_count=?, deferred_reclaim=?, updated_round_id=?, "
                    "updated_at=?, detail=? WHERE id=?",
                    (
                        level, status, subscriber_id, restart_lsn, confirmed_lsn,
                        wal_retention, recheck_count, int(deferred_reclaim),
                        round_id, now, detail, row["id"],
                    ),
                )
            else:
                self.conn.execute(
                    "INSERT INTO relief_advices(slot_name, instance, "
                    "subscriber_id, level, status, restart_lsn, confirmed_lsn, "
                    "wal_retention, recheck_count, deferred_reclaim, "
                    "created_round_id, created_at, detail) "
                    "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
                    (
                        slot_name, instance, subscriber_id, level, status,
                        restart_lsn, confirmed_lsn, wal_retention,
                        recheck_count, int(deferred_reclaim), round_id, now,
                        detail,
                    ),
                )

    def get_pending_advice(self, slot_name: str, instance: str) -> sqlite3.Row | None:
        with self._lock:
            return self.conn.execute(
                "SELECT * FROM relief_advices WHERE slot_name=? AND instance=? "
                "AND status='pending' ORDER BY id DESC LIMIT 1",
                (slot_name, instance),
            ).fetchone()

    def count_done_monitor_rounds_since(self, round_id: str) -> int:
        with self._lock:
            row = self.conn.execute(
                "SELECT count(*) AS c FROM rounds r WHERE r.kind='monitor' "
                "AND r.status='done' AND r.started_at > "
                "(SELECT started_at FROM rounds WHERE round_id=?)",
                (round_id,),
            ).fetchone()
            return int(row["c"])

    def revoke_pending_advice(
        self, slot_name: str, instance: str, round_id: str, now: float,
        reason: str,
    ) -> None:
        with self._lock:
            self.conn.execute(
                "UPDATE relief_advices SET status='revoked', updated_round_id=?, "
                "updated_at=?, detail=? WHERE slot_name=? AND instance=? "
                "AND status='pending'",
                (round_id, now, reason, slot_name, instance),
            )

    def revoke_unseen_advices(
        self, seen: set[tuple[str, str]], readable_instances: set[str],
        round_id: str, now: float,
    ) -> None:
        rows = self.list_pending_advices()
        for row in rows:
            key = (row["slot_name"], row["instance"])
            if row["instance"] in readable_instances and key not in seen:
                self.revoke_pending_advice(
                    row["slot_name"], row["instance"], round_id, now,
                    "slot no longer present",
                )

    def list_pending_advices(self) -> list[sqlite3.Row]:
        with self._lock:
            return self.conn.execute(
                "SELECT * FROM relief_advices WHERE status='pending' ORDER BY id"
            ).fetchall()

    # ---- resync ---------------------------------------------------------
    def upsert_resync_job(self, job: dict[str, Any]) -> None:
        with self._lock:
            self.conn.execute(
                "INSERT INTO resync_jobs(job_id, status, stage, trigger_reason, "
                "snapshot_lsn, previous_snapshot_lsn, baseline_at, created_at, "
                "updated_at) VALUES (:job_id,:status,:stage,:trigger_reason,"
                ":snapshot_lsn,:previous_snapshot_lsn,:baseline_at,"
                ":created_at,:updated_at) "
                "ON CONFLICT(job_id) DO UPDATE SET status=excluded.status, "
                "stage=excluded.stage, snapshot_lsn=excluded.snapshot_lsn, "
                "previous_snapshot_lsn=excluded.previous_snapshot_lsn, "
                "baseline_at=excluded.baseline_at, updated_at=excluded.updated_at",
                {
                    "job_id": job["job_id"],
                    "status": job["status"],
                    "stage": job.get("stage", "prepare"),
                    "trigger_reason": job.get("trigger_reason"),
                    "snapshot_lsn": job.get("snapshot_lsn"),
                    "previous_snapshot_lsn": job.get("previous_snapshot_lsn"),
                    "baseline_at": job.get("baseline_at"),
                    "created_at": job["created_at"],
                    "updated_at": job["updated_at"],
                },
            )

    def get_resync_job(self, job_id: str) -> sqlite3.Row | None:
        with self._lock:
            return self.conn.execute(
                "SELECT * FROM resync_jobs WHERE job_id=?", (job_id,)
            ).fetchone()

    def list_resync_jobs(self, status: str | None = None) -> list[sqlite3.Row]:
        with self._lock:
            if status:
                return self.conn.execute(
                    "SELECT * FROM resync_jobs WHERE status=? ORDER BY created_at",
                    (status,),
                ).fetchall()
            return self.conn.execute(
                "SELECT * FROM resync_jobs ORDER BY created_at"
            ).fetchall()

    def upsert_table_progress(
        self, job_id: str, table_name: str, status: str, last_key: int | None,
        rows_copied: int, now: float, phase: str = "bulk",
        delta_last_key: int | None = None,
    ) -> None:
        with self._lock:
            self.conn.execute(
                "INSERT INTO resync_table_progress(job_id, table_name, status, "
                "phase, last_key, delta_last_key, rows_copied, updated_at) "
                "VALUES (?,?,?,?,?,?,?,?) "
                "ON CONFLICT(job_id, table_name) DO UPDATE SET "
                "status=excluded.status, phase=excluded.phase, "
                "last_key=excluded.last_key, "
                "delta_last_key=excluded.delta_last_key, "
                "rows_copied=max(resync_table_progress.rows_copied, "
                "excluded.rows_copied), updated_at=excluded.updated_at",
                (
                    job_id, table_name, status, phase, last_key, delta_last_key,
                    rows_copied, now,
                ),
            )

    def get_table_progress(
        self, job_id: str, table_name: str
    ) -> sqlite3.Row | None:
        with self._lock:
            return self.conn.execute(
                "SELECT * FROM resync_table_progress WHERE job_id=? "
                "AND table_name=?",
                (job_id, table_name),
            ).fetchone()

    def list_table_progress(self, job_id: str) -> list[sqlite3.Row]:
        with self._lock:
            return self.conn.execute(
                "SELECT * FROM resync_table_progress WHERE job_id=? "
                "ORDER BY table_name",
                (job_id,),
            ).fetchall()

    def count_resync_tables_in_progress(self) -> int:
        with self._lock:
            row = self.conn.execute(
                "SELECT count(*) AS c FROM resync_table_progress p "
                "JOIN resync_jobs j ON j.job_id=p.job_id "
                "WHERE j.status IN ('running','paused') "
                "AND p.status IN ('pending','running')"
            ).fetchone()
            return int(row["c"])

    # ---- offsets / samples ---------------------------------------------
    def insert_offset_correction(self, row: dict[str, Any]) -> None:
        self._require_running(row["round_id"])
        with self._lock:
            self.conn.execute(
                "INSERT INTO offset_corrections(round_id, slot_name, redis_lsn, "
                "applied_lsn, verdict, source_of_truth, corrected_lsn, basis, "
                "created_at) VALUES (:round_id,:slot_name,:redis_lsn,"
                ":applied_lsn,:verdict,:source_of_truth,:corrected_lsn,:basis,"
                ":created_at)",
                row,
            )

    def insert_sample_mismatch(self, row: dict[str, Any]) -> None:
        self._require_running(row["round_id"])
        with self._lock:
            self.conn.execute(
                "INSERT OR IGNORE INTO sample_mismatches(round_id, table_name, "
                "primary_key, column_name, primary_val, replica_val, created_at) "
                "VALUES (:round_id,:table_name,:primary_key,:column_name,"
                ":primary_val,:replica_val,:created_at)",
                row,
            )

    def inc_counter(self, name: str, delta: int = 1) -> int:
        with self._lock:
            self.conn.execute(
                "INSERT INTO meta_counters(name, value) VALUES (?,?) "
                "ON CONFLICT(name) DO UPDATE SET value=value+excluded.value",
                (name, delta),
            )
            row = self.conn.execute(
                "SELECT value FROM meta_counters WHERE name=?", (name,)
            ).fetchone()
            return row["value"]

    def get_counter(self, name: str) -> int:
        with self._lock:
            row = self.conn.execute(
                "SELECT value FROM meta_counters WHERE name=?", (name,)
            ).fetchone()
            return row["value"] if row else 0

    def count_query_errors(self) -> int:
        with self._lock:
            row = self.conn.execute(
                "SELECT count(*) AS c FROM query_errors"
            ).fetchone()
            return int(row["c"])

    def commit(self) -> None:
        with self._lock:
            self.conn.commit()
