from __future__ import annotations

import sqlite3
from contextlib import contextmanager
from pathlib import Path
from typing import Iterator

from .errors import StateError


SCHEMA_SQL = """
PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
PRAGMA temp_store=FILE;
PRAGMA foreign_keys=OFF;
PRAGMA busy_timeout=30000;

CREATE TABLE IF NOT EXISTS job_runs (
    run_id TEXT PRIMARY KEY,
    signature TEXT NOT NULL,
    sql_text TEXT NOT NULL,
    sql_sha256 TEXT NOT NULL,
    dataset_sha256 TEXT NOT NULL,
    plan_mode TEXT NOT NULL,
    status TEXT NOT NULL,
    slots INTEGER NOT NULL,
    cpu_workers INTEGER NOT NULL,
    allowed_lateness_seconds INTEGER NOT NULL,
    skew_threshold_ratio REAL NOT NULL,
    memory_limit_bytes INTEGER NOT NULL,
    spill_threshold_bytes INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    resume_count INTEGER NOT NULL DEFAULT 0,
    consumed_batch INTEGER NOT NULL DEFAULT 0,
    watermark_us INTEGER,
    commit_seq INTEGER NOT NULL DEFAULT 0,
    telemetry_loaded INTEGER NOT NULL DEFAULT 0,
    published_at TEXT,
    error_code TEXT
);

CREATE TABLE IF NOT EXISTS checkpoints (
    run_id TEXT NOT NULL,
    commit_seq INTEGER NOT NULL,
    batch_id INTEGER NOT NULL,
    consumed_batch INTEGER NOT NULL,
    watermark_us INTEGER,
    accepted_events INTEGER NOT NULL,
    late_events INTEGER NOT NULL,
    dedup_discards INTEGER NOT NULL,
    closed_windows INTEGER NOT NULL,
    committed_at TEXT NOT NULL,
    PRIMARY KEY (run_id, commit_seq)
);

CREATE TABLE IF NOT EXISTS resume_events (
    run_id TEXT NOT NULL,
    resume_seq INTEGER NOT NULL,
    commit_seq INTEGER NOT NULL,
    consumed_batch INTEGER NOT NULL,
    resume_at TEXT NOT NULL,
    PRIMARY KEY (run_id, resume_seq)
);

CREATE TABLE IF NOT EXISTS vehicle_route (
    run_id TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    slot INTEGER NOT NULL,
    split INTEGER NOT NULL,
    PRIMARY KEY (run_id, vehicle_id)
);

CREATE TABLE IF NOT EXISTS unit_route (
    run_id TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    trip_id TEXT NOT NULL,
    window_start_us INTEGER NOT NULL,
    slot INTEGER NOT NULL,
    PRIMARY KEY (run_id, vehicle_id, trip_id, window_start_us)
);

CREATE TABLE IF NOT EXISTS vehicle_load (
    run_id TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    telemetry_count INTEGER NOT NULL,
    share REAL NOT NULL,
    split INTEGER NOT NULL,
    slots_json TEXT NOT NULL,
    unit_count INTEGER NOT NULL,
    PRIMARY KEY (run_id, vehicle_id)
);

CREATE TABLE IF NOT EXISTS trip_keys (
    run_id TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    trip_id TEXT NOT NULL,
    PRIMARY KEY (run_id, vehicle_id, trip_id)
);

CREATE TABLE IF NOT EXISTS slot_stats (
    run_id TEXT NOT NULL,
    slot INTEGER NOT NULL,
    event_count INTEGER NOT NULL DEFAULT 0,
    telemetry_count INTEGER NOT NULL DEFAULT 0,
    supplement_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, slot)
);

CREATE TABLE IF NOT EXISTS dedup_seen (
    run_id TEXT NOT NULL,
    dedup_key TEXT NOT NULL,
    source_type TEXT NOT NULL,
    batch_id INTEGER,
    PRIMARY KEY (run_id, dedup_key)
);

CREATE TABLE IF NOT EXISTS window_state (
    run_id TEXT NOT NULL,
    window_start_us INTEGER NOT NULL,
    window_end_us INTEGER NOT NULL,
    processing_slot INTEGER NOT NULL,
    vehicle_id TEXT NOT NULL,
    trip_id TEXT NOT NULL,
    source_type TEXT NOT NULL,
    mileage_u INTEGER NOT NULL,
    energy_u INTEGER NOT NULL,
    event_count INTEGER NOT NULL,
    supplement_count INTEGER NOT NULL,
    PRIMARY KEY (
        run_id, window_start_us, processing_slot, vehicle_id, trip_id, source_type
    )
);

CREATE INDEX IF NOT EXISTS idx_window_state_close
    ON window_state (run_id, window_end_us);

CREATE TABLE IF NOT EXISTS closed_windows (
    run_id TEXT NOT NULL,
    window_start_us INTEGER NOT NULL,
    window_end_us INTEGER NOT NULL,
    closed_at TEXT NOT NULL,
    PRIMARY KEY (run_id, window_start_us)
);

CREATE TABLE IF NOT EXISTS stage_vehicle_energy (
    run_id TEXT NOT NULL,
    plan_mode TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    window_start_us INTEGER NOT NULL,
    mileage_u INTEGER NOT NULL,
    energy_u INTEGER NOT NULL,
    event_count INTEGER NOT NULL,
    telemetry_count INTEGER NOT NULL,
    supplement_count INTEGER NOT NULL,
    PRIMARY KEY (run_id, window_start_us, vehicle_id)
);

CREATE TABLE IF NOT EXISTS stage_window_manifest (
    run_id TEXT NOT NULL,
    window_start_us INTEGER NOT NULL,
    window_end_us INTEGER NOT NULL,
    vehicle_count INTEGER NOT NULL,
    event_count INTEGER NOT NULL,
    committed_at TEXT NOT NULL,
    PRIMARY KEY (run_id, window_start_us)
);

CREATE TABLE IF NOT EXISTS vehicle_energy (
    run_id TEXT NOT NULL,
    plan_mode TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    window_start TEXT NOT NULL,
    window_start_us INTEGER NOT NULL,
    mileage_delta REAL NOT NULL,
    energy_delta REAL NOT NULL,
    energy_per_100km REAL NOT NULL,
    energy_per_100km_text TEXT NOT NULL,
    event_count INTEGER NOT NULL,
    telemetry_count INTEGER NOT NULL,
    supplement_count INTEGER NOT NULL,
    published_at TEXT NOT NULL,
    PRIMARY KEY (run_id, vehicle_id, window_start_us)
);

CREATE TABLE IF NOT EXISTS vehicle_energy_total (
    run_id TEXT NOT NULL,
    plan_mode TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    mileage_delta REAL NOT NULL,
    energy_delta REAL NOT NULL,
    energy_per_100km REAL NOT NULL,
    energy_per_100km_text TEXT NOT NULL,
    event_count INTEGER NOT NULL,
    telemetry_count INTEGER NOT NULL,
    supplement_count INTEGER NOT NULL,
    window_count INTEGER NOT NULL,
    published_at TEXT NOT NULL,
    PRIMARY KEY (run_id, vehicle_id)
);

CREATE TABLE IF NOT EXISTS window_manifest (
    run_id TEXT NOT NULL,
    plan_mode TEXT NOT NULL,
    window_start TEXT NOT NULL,
    window_start_us INTEGER NOT NULL,
    window_end TEXT NOT NULL,
    vehicle_count INTEGER NOT NULL,
    event_count INTEGER NOT NULL,
    published_at TEXT NOT NULL,
    PRIMARY KEY (run_id, window_start_us)
);

CREATE TABLE IF NOT EXISTS batch_attempts (
    run_id TEXT NOT NULL,
    plan_mode TEXT NOT NULL,
    batch_id INTEGER NOT NULL,
    attempt_no INTEGER NOT NULL,
    status TEXT NOT NULL,
    error_code TEXT,
    rows_read INTEGER,
    accepted_events INTEGER,
    dedup_discards INTEGER,
    late_events INTEGER,
    watermark_us INTEGER,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    PRIMARY KEY (run_id, batch_id, attempt_no)
);

CREATE TABLE IF NOT EXISTS event_issues (
    run_id TEXT NOT NULL,
    plan_mode TEXT NOT NULL,
    issue_type TEXT NOT NULL,
    source_type TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    trip_id TEXT NOT NULL,
    window_start_us INTEGER NOT NULL,
    event_time_us INTEGER NOT NULL,
    batch_id INTEGER,
    source_id TEXT NOT NULL,
    detail TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_event_issues_lookup
    ON event_issues (run_id, issue_type, vehicle_id, trip_id, window_start_us);

CREATE TABLE IF NOT EXISTS spill_events (
    run_id TEXT NOT NULL,
    sequence_no INTEGER NOT NULL,
    batch_id INTEGER,
    bytes_written INTEGER NOT NULL,
    reason TEXT NOT NULL,
    PRIMARY KEY (run_id, sequence_no)
);

CREATE TABLE IF NOT EXISTS verification_results (
    run_id TEXT NOT NULL,
    comparison TEXT NOT NULL,
    result_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (run_id, comparison)
);

CREATE TABLE IF NOT EXISTS job_reports (
    run_id TEXT NOT NULL,
    plan_mode TEXT NOT NULL,
    report_json TEXT NOT NULL,
    stable_hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (run_id)
);

CREATE TABLE IF NOT EXISTS job_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
"""


class StateStore:
    def __init__(self, db_path: str | Path):
        self.path = Path(db_path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        try:
            self.connection = sqlite3.connect(str(self.path), timeout=30.0, isolation_level=None)
            self.connection.row_factory = sqlite3.Row
            self.connection.executescript(SCHEMA_SQL)
        except sqlite3.Error as exc:
            raise StateError(f"could not open settlement SQLite database: {exc}") from exc

    def close(self) -> None:
        try:
            self.connection.close()
        except sqlite3.Error:
            pass

    @contextmanager
    def tx(self) -> Iterator[sqlite3.Connection]:
        try:
            self.connection.execute("BEGIN IMMEDIATE")
            yield self.connection
            self.connection.execute("COMMIT")
        except BaseException:
            try:
                self.connection.execute("ROLLBACK")
            except sqlite3.Error:
                pass
            raise

    def execute(self, sql: str, parameters: tuple | list = ()) -> sqlite3.Cursor:
        return self.connection.execute(sql, parameters)

    def fetchone(self, sql: str, parameters: tuple | list = ()) -> sqlite3.Row | None:
        return self.connection.execute(sql, parameters).fetchone()

    def fetchall(self, sql: str, parameters: tuple | list = ()) -> list[sqlite3.Row]:
        return list(self.connection.execute(sql, parameters).fetchall())

