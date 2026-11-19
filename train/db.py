from __future__ import annotations

import json
import sqlite3
import time
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Iterator

from train.common import (
    load_manifest_for_config,
    parameter_names,
    resolve_path,
)


SCHEMA = """
CREATE TABLE IF NOT EXISTS metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS workers (
    worker_id TEXT PRIMARY KEY,
    data_shard_index INTEGER NOT NULL,
    sample_count INTEGER NOT NULL,
    status TEXT NOT NULL,
    registered_at REAL NOT NULL,
    last_heartbeat REAL NOT NULL,
    local_step INTEGER NOT NULL DEFAULT 0,
    total_stale INTEGER NOT NULL DEFAULT 0,
    consecutive_stale INTEGER NOT NULL DEFAULT 0,
    lagging INTEGER NOT NULL DEFAULT 0,
    trigger_version_gap INTEGER NOT NULL DEFAULT 0,
    last_lag_at REAL
);

CREATE TABLE IF NOT EXISTS shards (
    shard_id TEXT PRIMARY KEY,
    endpoint TEXT NOT NULL,
    declared_names TEXT NOT NULL,
    status TEXT NOT NULL,
    generation INTEGER NOT NULL,
    registered_at REAL NOT NULL,
    last_heartbeat REAL NOT NULL,
    last_error TEXT
);

CREATE TABLE IF NOT EXISTS shard_ownership (
    parameter_name TEXT PRIMARY KEY,
    shard_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    FOREIGN KEY (shard_id) REFERENCES shards(shard_id)
);

CREATE TABLE IF NOT EXISTS parameters (
    parameter_name TEXT PRIMARY KEY,
    value REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS parameter_history (
    version INTEGER NOT NULL,
    parameter_name TEXT NOT NULL,
    value REAL NOT NULL,
    PRIMARY KEY (version, parameter_name)
);

CREATE TABLE IF NOT EXISTS steps (
    step INTEGER PRIMARY KEY,
    version INTEGER NOT NULL UNIQUE,
    loss REAL NOT NULL,
    worker_count INTEGER NOT NULL,
    submission_count INTEGER NOT NULL,
    applied_at REAL NOT NULL,
    duration_ms REAL NOT NULL,
    route_generation INTEGER NOT NULL,
    takeover_id INTEGER
);

CREATE TABLE IF NOT EXISTS submissions (
    attempt_id INTEGER PRIMARY KEY AUTOINCREMENT,
    worker_id TEXT NOT NULL,
    step INTEGER NOT NULL,
    base_version INTEGER NOT NULL,
    sample_count INTEGER NOT NULL,
    local_loss REAL NOT NULL,
    gradient BLOB NOT NULL,
    status TEXT NOT NULL,
    code TEXT NOT NULL,
    applied_version INTEGER,
    submitted_at REAL NOT NULL,
    applied_at REAL NOT NULL,
    latency_ms REAL NOT NULL,
    FOREIGN KEY (worker_id) REFERENCES workers(worker_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS one_applied_per_worker_step
    ON submissions(worker_id, step) WHERE status = 'applied';
CREATE INDEX IF NOT EXISTS submissions_by_step ON submissions(step, worker_id);

CREATE TABLE IF NOT EXISTS stale_rejections (
    rejection_id INTEGER PRIMARY KEY AUTOINCREMENT,
    worker_id TEXT NOT NULL,
    step INTEGER NOT NULL,
    base_version INTEGER NOT NULL,
    current_version INTEGER NOT NULL,
    version_gap INTEGER NOT NULL,
    sample_count INTEGER NOT NULL DEFAULT 0,
    gradient BLOB,
    code TEXT NOT NULL,
    rejected_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS stale_by_worker ON stale_rejections(worker_id, rejected_at);

CREATE TABLE IF NOT EXISTS duplicate_submissions (
    duplicate_id INTEGER PRIMARY KEY AUTOINCREMENT,
    worker_id TEXT NOT NULL,
    step INTEGER NOT NULL,
    base_version INTEGER NOT NULL,
    current_version INTEGER NOT NULL,
    original_attempt_id INTEGER,
    returned_version INTEGER NOT NULL,
    sample_count INTEGER NOT NULL DEFAULT 0,
    gradient BLOB,
    received_at REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS routes (
    step INTEGER NOT NULL,
    parameter_name TEXT NOT NULL,
    shard_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    PRIMARY KEY (step, parameter_name)
);

CREATE TABLE IF NOT EXISTS takeovers (
    takeover_id INTEGER PRIMARY KEY AUTOINCREMENT,
    old_shard_id TEXT NOT NULL,
    new_shard_id TEXT NOT NULL,
    parameter_names TEXT NOT NULL,
    trigger TEXT NOT NULL,
    from_version INTEGER NOT NULL,
    to_version INTEGER,
    status TEXT NOT NULL,
    started_at REAL NOT NULL,
    completed_at REAL,
    error TEXT
);

CREATE TABLE IF NOT EXISTS lag_events (
    lag_event_id INTEGER PRIMARY KEY AUTOINCREMENT,
    worker_id TEXT NOT NULL,
    version_gap INTEGER NOT NULL,
    consecutive_stale INTEGER NOT NULL,
    triggered_at REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS shard_cache (
    shard_id TEXT NOT NULL,
    parameter_name TEXT NOT NULL,
    value REAL NOT NULL,
    version INTEGER NOT NULL,
    generation INTEGER NOT NULL,
    updated_at REAL NOT NULL,
    PRIMARY KEY (shard_id, parameter_name)
);
"""


def connect(db_path: str | Path, timeout: float = 30.0) -> sqlite3.Connection:
    connection = sqlite3.connect(
        str(db_path), timeout=timeout, isolation_level=None, check_same_thread=False
    )
    connection.row_factory = sqlite3.Row
    connection.execute("PRAGMA busy_timeout = 30000")
    connection.execute("PRAGMA foreign_keys = ON")
    connection.execute("PRAGMA journal_mode = WAL")
    connection.execute("PRAGMA synchronous = FULL")
    return connection


@contextmanager
def transaction(connection: sqlite3.Connection, immediate: bool = True) -> Iterator[sqlite3.Connection]:
    connection.execute("BEGIN IMMEDIATE" if immediate else "BEGIN")
    try:
        yield connection
    except BaseException:
        connection.execute("ROLLBACK")
        raise
    else:
        connection.execute("COMMIT")


def database_path(config: dict[str, Any]) -> Path:
    return resolve_path(config["database"])


def initialize_database(config: dict[str, Any]) -> Path:
    manifest = load_manifest_for_config(config)
    db_path = database_path(config)
    db_path.parent.mkdir(parents=True, exist_ok=True)
    connection = connect(db_path)
    try:
        connection.executescript(SCHEMA)
        _apply_migrations(connection)
        now = time.time()
        with transaction(connection):
            existing = connection.execute(
                "SELECT value FROM metadata WHERE key = 'schema_version'"
            ).fetchone()
            if existing is None:
                names = parameter_names(manifest)
                connection.executemany(
                    "INSERT INTO parameters(parameter_name, value) VALUES (?, 0.0)",
                    [(name,) for name in names],
                )
                connection.executemany(
                    """
                    INSERT INTO parameter_history(version, parameter_name, value)
                    VALUES (0, ?, 0.0)
                    """,
                    [(name,) for name in names],
                )
                connection.executemany(
                    "INSERT INTO metadata(key, value) VALUES (?, ?)",
                    [
                        ("schema_version", "1"),
                        ("created_at", f"{now:.6f}"),
                        ("initialization", "zeros"),
                    ],
                )
            else:
                parameter_count = connection.execute(
                    "SELECT COUNT(*) AS count FROM parameters"
                ).fetchone()["count"]
                if parameter_count == 0:
                    raise RuntimeError("database has metadata but no parameters")
                connection.execute(
                    "UPDATE takeovers SET status = 'failed', error = ? "
                    "WHERE status = 'in_progress'",
                    ("coordinator restarted during takeover",),
                )
    finally:
        connection.close()
    return db_path


def _apply_migrations(connection: sqlite3.Connection) -> None:
    migrations = {
        "stale_rejections": [
            ("sample_count", "INTEGER NOT NULL DEFAULT 0"),
            ("gradient", "BLOB"),
        ],
        "duplicate_submissions": [
            ("sample_count", "INTEGER NOT NULL DEFAULT 0"),
            ("gradient", "BLOB"),
        ],
    }
    for table, columns in migrations.items():
        existing = {
            row["name"]
            for row in connection.execute(f"PRAGMA table_info({table})")
        }
        for name, declaration in columns:
            if name not in existing:
                connection.execute(
                    f"ALTER TABLE {table} ADD COLUMN {name} {declaration}"
                )


def last_applied_step(connection: sqlite3.Connection) -> int:
    row = connection.execute("SELECT MAX(step) AS step FROM steps").fetchone()
    return int(row["step"] or 0)


def load_current_parameters(connection: sqlite3.Connection) -> dict[str, float]:
    return {
        row["parameter_name"]: float(row["value"])
        for row in connection.execute(
            "SELECT parameter_name, value FROM parameters ORDER BY parameter_name"
        )
    }


def load_parameters_at_version(
    connection: sqlite3.Connection, version: int
) -> dict[str, float]:
    return {
        row["parameter_name"]: float(row["value"])
        for row in connection.execute(
            """
            SELECT parameter_name, value
            FROM parameter_history
            WHERE version = ?
            ORDER BY parameter_name
            """,
            (int(version),),
        )
    }


def metadata_value(connection: sqlite3.Connection, key: str) -> str | None:
    row = connection.execute(
        "SELECT value FROM metadata WHERE key = ?", (key,)
    ).fetchone()
    return None if row is None else str(row["value"])


def set_metadata_value(connection: sqlite3.Connection, key: str, value: str) -> None:
    connection.execute(
        """
        INSERT INTO metadata(key, value) VALUES (?, ?)
        ON CONFLICT(key) DO UPDATE SET value = excluded.value
        """,
        (key, value),
    )


def row_to_dict(row: sqlite3.Row | None) -> dict[str, Any] | None:
    return None if row is None else {key: row[key] for key in row.keys()}


def rows_to_dicts(rows: list[sqlite3.Row]) -> list[dict[str, Any]]:
    return [{key: row[key] for key in row.keys()} for row in rows]


def json_dumps_stable(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
