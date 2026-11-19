"""SQLite persistence with transactional decision/state updates."""

from __future__ import annotations

import sqlite3
import threading
import uuid
from contextlib import contextmanager
from pathlib import Path
from typing import Iterator


SCHEMA = """
CREATE TABLE IF NOT EXISTS releases (
    release_id TEXT PRIMARY KEY,
    parameter_key TEXT NOT NULL,
    target_version TEXT NOT NULL,
    previous_stable_version TEXT NOT NULL,
    status TEXT NOT NULL,
    current_batch INTEGER NOT NULL,
    next_batch INTEGER NOT NULL,
    total_batches INTEGER NOT NULL,
    rollback_target_version TEXT NOT NULL,
    rollback_required INTEGER NOT NULL DEFAULT 0,
    rollback_trigger_batch INTEGER,
    rollback_trigger_instances_json TEXT NOT NULL DEFAULT '[]',
    rollback_trigger_metrics_json TEXT NOT NULL DEFAULT '[]',
    rollback_started_at REAL,
    rollback_cutoff_at REAL,
    rollback_ineffective_json TEXT NOT NULL DEFAULT '[]',
    rollback_checked_at REAL,
    conflict_release_ids_json TEXT NOT NULL DEFAULT '[]',
    plan_json TEXT NOT NULL,
    plan_path TEXT,
    metrics_path TEXT,
    version_reports_path TEXT,
    plan_sha256 TEXT,
    created_at REAL NOT NULL,
    updated_at REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS batches (
    release_id TEXT NOT NULL,
    batch_no INTEGER NOT NULL,
    observation_seconds INTEGER NOT NULL,
    start_offset_seconds REAL,
    instances_json TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    result TEXT,
    reason_code TEXT,
    decision_json TEXT,
    started_at REAL,
    completed_at REAL,
    PRIMARY KEY (release_id, batch_no),
    FOREIGN KEY (release_id) REFERENCES releases(release_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS decision_records (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    release_id TEXT NOT NULL,
    batch_no INTEGER,
    record_type TEXT NOT NULL,
    result TEXT,
    reason_code TEXT,
    record_json TEXT NOT NULL,
    created_at REAL NOT NULL,
    FOREIGN KEY (release_id) REFERENCES releases(release_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS samples (
    release_id TEXT NOT NULL,
    batch_no INTEGER NOT NULL,
    instance_id TEXT NOT NULL,
    observed_at REAL NOT NULL,
    observed_at_raw TEXT,
    error_rate REAL,
    connection_reject_rate REAL,
    request_count REAL,
    parameter_version TEXT,
    PRIMARY KEY (release_id, batch_no, instance_id, observed_at)
);

CREATE TABLE IF NOT EXISTS version_reports (
    release_id TEXT NOT NULL,
    instance_id TEXT NOT NULL,
    observed_at REAL NOT NULL,
    observed_at_raw TEXT,
    parameter_version TEXT NOT NULL,
    PRIMARY KEY (release_id, instance_id, observed_at)
);

CREATE TABLE IF NOT EXISTS instance_locks (
    instance_id TEXT PRIMARY KEY,
    release_id TEXT NOT NULL,
    batch_no INTEGER NOT NULL,
    acquired_at REAL NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_decisions_release
    ON decision_records(release_id, id);
CREATE INDEX IF NOT EXISTS idx_samples_release_batch
    ON samples(release_id, batch_no, instance_id, observed_at);
CREATE INDEX IF NOT EXISTS idx_versions_release
    ON version_reports(release_id, instance_id, observed_at);
CREATE INDEX IF NOT EXISTS idx_locks_release
    ON instance_locks(release_id);
"""


class Database:
    def __init__(self, path: str) -> None:
        self._memory = path == ":memory:"
        if self._memory:
            self.path = (
                f"file:release_orchestrator_{uuid.uuid4().hex}"
                "?mode=memory&cache=shared"
            )
            self._keeper: sqlite3.Connection | None = sqlite3.connect(
                self.path,
                uri=True,
                check_same_thread=False,
            )
        else:
            self.path = path
            self._keeper = None
            parent = Path(path).expanduser().resolve().parent
            parent.mkdir(parents=True, exist_ok=True)
        self._write_lock = threading.RLock()
        self.initialize()

    def connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(
            self.path,
            timeout=30.0,
            isolation_level=None,
            check_same_thread=False,
            uri=self._memory,
        )
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA busy_timeout = 30000")
        connection.execute("PRAGMA foreign_keys = ON")
        if not self._memory:
            connection.execute("PRAGMA journal_mode = WAL")
            connection.execute("PRAGMA synchronous = NORMAL")
        return connection

    def initialize(self) -> None:
        with self._write_lock:
            connection = self.connect()
            try:
                connection.executescript(SCHEMA)
            finally:
                connection.close()

    @contextmanager
    def transaction(self) -> Iterator[sqlite3.Connection]:
        with self._write_lock:
            connection = self.connect()
            try:
                connection.execute("BEGIN IMMEDIATE")
                yield connection
                connection.execute("COMMIT")
            except BaseException:
                try:
                    connection.execute("ROLLBACK")
                except sqlite3.Error:
                    pass
                raise
            finally:
                connection.close()

    @contextmanager
    def reader(self) -> Iterator[sqlite3.Connection]:
        connection = self.connect()
        try:
            yield connection
        finally:
            connection.close()

    def close(self) -> None:
        if self._keeper is not None:
            self._keeper.close()
            self._keeper = None
