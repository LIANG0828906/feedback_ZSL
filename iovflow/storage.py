from __future__ import annotations

import contextlib
import json
import os
import sqlite3
from pathlib import Path
from typing import Any, Iterator

from .util import canonical_json, iso, utc_now


STATE_SCHEMA = """
CREATE TABLE IF NOT EXISTS jobs (
    job_id TEXT PRIMARY KEY,
    sql_hash TEXT NOT NULL,
    sql_text TEXT NOT NULL,
    mode TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    config_json TEXT NOT NULL,
    plan_json TEXT NOT NULL,
    input_json TEXT NOT NULL,
    routing_json TEXT NOT NULL,
    report_json TEXT,
    error_code TEXT,
    error_message TEXT
);
CREATE TABLE IF NOT EXISTS job_batches (
    job_id TEXT NOT NULL,
    batch_id INTEGER NOT NULL,
    batch_no INTEGER NOT NULL,
    declared_epoch INTEGER NOT NULL,
    declared_event_time TEXT NOT NULL,
    max_event_epoch INTEGER NOT NULL,
    rows INTEGER NOT NULL,
    watermark INTEGER,
    filter_committed INTEGER NOT NULL DEFAULT 0,
    window_committed INTEGER NOT NULL DEFAULT 0,
    merge_committed INTEGER NOT NULL DEFAULT 0,
    sink_committed INTEGER NOT NULL DEFAULT 0,
    committed_at TEXT,
    PRIMARY KEY (job_id, batch_id)
);
CREATE TABLE IF NOT EXISTS stages (
    job_id TEXT NOT NULL,
    stage_id TEXT NOT NULL,
    stage_type TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    parallelism INTEGER NOT NULL,
    max_concurrent INTEGER NOT NULL,
    input_from TEXT NOT NULL,
    output_to TEXT NOT NULL,
    status TEXT NOT NULL,
    consumed_batch INTEGER NOT NULL DEFAULT -1,
    committed_batch INTEGER NOT NULL DEFAULT -1,
    watermark INTEGER,
    updated_at TEXT NOT NULL,
    state_json TEXT,
    PRIMARY KEY (job_id, stage_id)
);
CREATE TABLE IF NOT EXISTS stage_shards (
    job_id TEXT NOT NULL,
    stage_id TEXT NOT NULL,
    shard_id INTEGER NOT NULL,
    status TEXT NOT NULL,
    consumed_batch INTEGER NOT NULL DEFAULT -1,
    committed_batch INTEGER NOT NULL DEFAULT -1,
    watermark INTEGER,
    takeover_count INTEGER NOT NULL DEFAULT 0,
    owner TEXT,
    last_error TEXT,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (job_id, stage_id, shard_id)
);
CREATE TABLE IF NOT EXISTS stage_batches (
    job_id TEXT NOT NULL,
    stage_id TEXT NOT NULL,
    batch_id INTEGER NOT NULL,
    status TEXT NOT NULL,
    synthetic INTEGER NOT NULL DEFAULT 0,
    watermark INTEGER,
    committed_at TEXT NOT NULL,
    details_json TEXT,
    PRIMARY KEY (job_id, stage_id, batch_id)
);
CREATE TABLE IF NOT EXISTS batch_routes (
    job_id TEXT NOT NULL,
    batch_id INTEGER NOT NULL,
    shard_id INTEGER NOT NULL,
    path TEXT NOT NULL,
    rows INTEGER NOT NULL,
    bytes INTEGER NOT NULL,
    PRIMARY KEY (job_id, batch_id, shard_id)
);
CREATE TABLE IF NOT EXISTS dedupe_keys (
    job_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    batch_id INTEGER NOT NULL,
    PRIMARY KEY (job_id, fingerprint)
);
CREATE TABLE IF NOT EXISTS window_accum (
    job_id TEXT NOT NULL,
    stage_id TEXT NOT NULL,
    shard_id INTEGER NOT NULL,
    vehicle_id TEXT NOT NULL,
    window_start INTEGER NOT NULL,
    window_end INTEGER NOT NULL,
    acc_json TEXT NOT NULL,
    closed INTEGER NOT NULL DEFAULT 0,
    event_rows INTEGER NOT NULL DEFAULT 0,
    bytes INTEGER NOT NULL DEFAULT 0,
    updated_batch INTEGER NOT NULL,
    PRIMARY KEY (job_id, stage_id, shard_id, vehicle_id, window_start)
);
CREATE INDEX IF NOT EXISTS idx_window_accum_key
    ON window_accum (job_id, vehicle_id, window_start);
CREATE INDEX IF NOT EXISTS idx_window_accum_end
    ON window_accum (job_id, stage_id, shard_id, window_end, closed);
CREATE TABLE IF NOT EXISTS touched_windows (
    job_id TEXT NOT NULL,
    batch_id INTEGER NOT NULL,
    vehicle_id TEXT NOT NULL,
    window_start INTEGER NOT NULL,
    window_end INTEGER NOT NULL,
    PRIMARY KEY (job_id, batch_id, vehicle_id, window_start)
);
CREATE TABLE IF NOT EXISTS closed_windows (
    job_id TEXT NOT NULL,
    batch_id INTEGER NOT NULL,
    vehicle_id TEXT NOT NULL,
    window_start INTEGER NOT NULL,
    window_end INTEGER NOT NULL,
    PRIMARY KEY (job_id, batch_id, vehicle_id, window_start)
);
CREATE TABLE IF NOT EXISTS window_partials (
    job_id TEXT NOT NULL,
    shard_id INTEGER NOT NULL,
    vehicle_id TEXT NOT NULL,
    window_start INTEGER NOT NULL,
    window_end INTEGER NOT NULL,
    acc_json TEXT NOT NULL,
    closed INTEGER NOT NULL,
    batch_id INTEGER NOT NULL,
    PRIMARY KEY (job_id, shard_id, vehicle_id, window_start)
);
CREATE TABLE IF NOT EXISTS completed_windows (
    job_id TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    window_start INTEGER NOT NULL,
    window_end INTEGER NOT NULL,
    result_json TEXT NOT NULL,
    closed_batch INTEGER NOT NULL,
    watermark INTEGER NOT NULL,
    emitted_at TEXT NOT NULL,
    PRIMARY KEY (job_id, vehicle_id, window_start)
);
CREATE TABLE IF NOT EXISTS late_windows (
    job_id TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    window_start INTEGER NOT NULL,
    window_end INTEGER NOT NULL,
    late_count INTEGER NOT NULL DEFAULT 0,
    first_batch INTEGER NOT NULL,
    last_batch INTEGER NOT NULL,
    PRIMARY KEY (job_id, vehicle_id, window_start)
);
CREATE TABLE IF NOT EXISTS outbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id TEXT NOT NULL,
    batch_id INTEGER NOT NULL,
    vehicle_id TEXT NOT NULL,
    window_start INTEGER NOT NULL,
    window_end INTEGER NOT NULL,
    result_json TEXT NOT NULL,
    is_final INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (job_id, batch_id, vehicle_id, window_start)
);
CREATE TABLE IF NOT EXISTS sink_progress (
    job_id TEXT PRIMARY KEY,
    last_outbox_id INTEGER NOT NULL DEFAULT 0,
    published_batches INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS workers (
    worker_id TEXT PRIMARY KEY,
    pid INTEGER,
    status TEXT,
    current_task TEXT,
    started_at TEXT,
    heartbeat_at TEXT
);
CREATE TABLE IF NOT EXISTS job_workers (
    job_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    pid INTEGER,
    status TEXT,
    current_task TEXT,
    started_at TEXT,
    heartbeat_at TEXT,
    PRIMARY KEY (job_id, worker_id)
);
CREATE TABLE IF NOT EXISTS recoveries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id TEXT NOT NULL,
    stage_id TEXT NOT NULL,
    shard_id INTEGER NOT NULL,
    batch_id INTEGER NOT NULL,
    reason TEXT NOT NULL,
    old_pid INTEGER,
    new_pid INTEGER,
    takeover_no INTEGER NOT NULL,
    at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS watermark_history (
    job_id TEXT NOT NULL,
    batch_id INTEGER NOT NULL,
    declared_epoch INTEGER NOT NULL,
    declared_event_time TEXT NOT NULL,
    watermark INTEGER NOT NULL,
    consumed_at TEXT NOT NULL,
    PRIMARY KEY (job_id, batch_id)
);
CREATE TABLE IF NOT EXISTS spill_stats (
    job_id TEXT PRIMARY KEY,
    spill_bytes INTEGER NOT NULL DEFAULT 0,
    temp_bytes_peak INTEGER NOT NULL DEFAULT 0,
    peak_memory_mb REAL NOT NULL DEFAULT 0,
    batch_durations_json TEXT,
    updated_at TEXT NOT NULL
);
CREATE VIEW IF NOT EXISTS v_stage_dag AS
SELECT job_id, stage_id, stage_type, ordinal, parallelism, max_concurrent,
       input_from, output_to, status, consumed_batch, committed_batch, watermark
FROM stages;
CREATE VIEW IF NOT EXISTS v_open_windows AS
SELECT job_id, stage_id, shard_id, vehicle_id, window_start, window_end,
       event_rows, updated_batch, acc_json
FROM window_accum WHERE closed = 0;
"""


RESULTS_SCHEMA = """
CREATE TABLE IF NOT EXISTS window_results (
    job_id TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    window_start TEXT NOT NULL,
    window_start_epoch INTEGER NOT NULL,
    window_end TEXT NOT NULL,
    window_end_epoch INTEGER NOT NULL,
    result_json TEXT NOT NULL,
    is_final INTEGER NOT NULL,
    version_batch INTEGER NOT NULL,
    publish_seq INTEGER NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (job_id, vehicle_id, window_start_epoch)
);
CREATE INDEX IF NOT EXISTS idx_window_results_job
    ON window_results (job_id, is_final, vehicle_id, window_start_epoch);
CREATE TABLE IF NOT EXISTS result_versions (
    job_id TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    window_start_epoch INTEGER NOT NULL,
    publish_seq INTEGER NOT NULL,
    result_json TEXT NOT NULL,
    is_final INTEGER NOT NULL,
    version_batch INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (job_id, vehicle_id, window_start_epoch, publish_seq)
);
CREATE TABLE IF NOT EXISTS late_windows (
    job_id TEXT NOT NULL,
    vehicle_id TEXT NOT NULL,
    window_start INTEGER NOT NULL,
    window_end INTEGER NOT NULL,
    late_count INTEGER NOT NULL,
    first_batch INTEGER NOT NULL,
    last_batch INTEGER NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (job_id, vehicle_id, window_start)
);
CREATE TABLE IF NOT EXISTS job_reports (
    job_id TEXT PRIMARY KEY,
    report_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE VIEW IF NOT EXISTS v_window_results_with_late AS
SELECT r.*, COALESCE(l.late_count, 0) AS late_count_after_close,
       l.first_batch AS late_first_batch, l.last_batch AS late_last_batch
FROM window_results r
LEFT JOIN late_windows l
  ON l.job_id = r.job_id
 AND l.vehicle_id = r.vehicle_id
 AND l.window_start = r.window_start_epoch;
"""


def open_state(path: str | Path) -> sqlite3.Connection:
    p = Path(path)
    p.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(p), timeout=60.0)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=FULL")
    conn.execute("PRAGMA busy_timeout=60000")
    conn.execute("PRAGMA foreign_keys=ON")
    conn.executescript(STATE_SCHEMA)
    return conn


def open_results(path: str | Path) -> sqlite3.Connection:
    p = Path(path)
    p.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(p), timeout=60.0)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=FULL")
    conn.execute("PRAGMA busy_timeout=60000")
    conn.executescript(RESULTS_SCHEMA)
    return conn


@contextlib.contextmanager
def tx(conn: sqlite3.Connection) -> Iterator[sqlite3.Connection]:
    try:
        conn.execute("BEGIN IMMEDIATE")
        yield conn
        conn.commit()
    except BaseException:
        conn.rollback()
        raise


def get_job(conn: sqlite3.Connection, job_id: str) -> dict[str, Any] | None:
    row = conn.execute("SELECT * FROM jobs WHERE job_id=?", (job_id,)).fetchone()
    return dict(row) if row else None


def create_or_resume_job(
    conn: sqlite3.Connection,
    job_id: str,
    config: dict[str, Any],
    plan: dict[str, Any],
    dataset: dict[str, Any],
    routing: dict[str, Any],
    stages: list[dict[str, Any]],
) -> dict[str, Any]:
    existing = get_job(conn, job_id)
    now = iso()
    if existing:
        if existing["sql_hash"] != plan["sql_hash"]:
            from .errors import EngineError

            raise EngineError(
                "IOV_STATE_INCOMPATIBLE",
                "resume SQL hash differs from the committed job",
                context={"existing": existing["sql_hash"], "requested": plan["sql_hash"]},
            )
        for key in ("mode", "allowed_lateness", "vehicle_column"):
            if config.get(key) != json.loads(existing["config_json"]).get(key):
                from .errors import EngineError

                raise EngineError(
                    "IOV_STATE_INCOMPATIBLE",
                    f"resume changes {key}",
                    context={"existing": existing["config_json"], "requested": config},
                )
        return existing
    with tx(conn):
        conn.execute(
            """
            INSERT INTO jobs (
                job_id, sql_hash, sql_text, mode, status, created_at, updated_at,
                config_json, plan_json, input_json, routing_json
            ) VALUES (?,?,?,?,?,?,?,?,?,?,?)
            """,
            (
                job_id,
                plan["sql_hash"],
                plan["sql"],
                config["mode"],
                "pending",
                now,
                now,
                canonical_json(config),
                canonical_json(plan),
                canonical_json(dataset),
                canonical_json(routing),
            ),
        )
        for batch in dataset.get("batches", []):
            conn.execute(
                """
                INSERT INTO job_batches (
                    job_id, batch_id, batch_no, declared_epoch, declared_event_time,
                    max_event_epoch, rows
                ) VALUES (?,?,?,?,?,?,?)
                """,
                (
                    job_id,
                    batch["batch_id"],
                    batch["batch_no"],
                    batch["declared_event_time"],
                    batch.get("declared_iso") or _epoch_iso(batch["declared_event_time"]),
                    batch.get("max_event_time", 0),
                    batch["rows"],
                ),
            )
        for stage in stages:
            conn.execute(
                """
                INSERT INTO stages (
                    job_id, stage_id, stage_type, ordinal, parallelism,
                    max_concurrent, input_from, output_to, status, updated_at
                ) VALUES (?,?,?,?,?,?,?,?,?,?)
                """,
                (
                    job_id,
                    stage["stage_id"],
                    stage["stage_type"],
                    stage["ordinal"],
                    stage["parallelism"],
                    stage["max_concurrent"],
                    stage["input_from"],
                    stage["output_to"],
                    "pending",
                    now,
                ),
            )
            shard_count = 1
            if stage["stage_type"] in ("filter", "group_by_vehicle"):
                shard_count = max(1, int(stage.get("parallelism", config.get("parallelism", 2))))
            elif stage["stage_type"] == "window_aggregate":
                shard_count = max(1, int(routing.get("shard_count", 1)))
            for shard in range(shard_count):
                conn.execute(
                    """
                    INSERT INTO stage_shards (
                        job_id, stage_id, shard_id, status, updated_at
                    ) VALUES (?,?,?,?,?)
                    """,
                    (job_id, stage["stage_id"], shard, "pending", now),
                )
        conn.execute(
            "INSERT OR IGNORE INTO sink_progress (job_id, updated_at) VALUES (?,?)",
            (job_id, now),
        )
        conn.execute(
            "INSERT OR IGNORE INTO spill_stats (job_id, updated_at) VALUES (?,?)",
            (job_id, now),
        )
    return get_job(conn, job_id) or {}


def update_job(
    conn: sqlite3.Connection,
    job_id: str,
    **fields: Any,
) -> None:
    if not fields:
        return
    fields = dict(fields)
    fields["updated_at"] = iso()
    cols = ", ".join(f"{k}=?" for k in fields)
    with tx(conn):
        conn.execute(f"UPDATE jobs SET {cols} WHERE job_id=?", (*fields.values(), job_id))


def update_stage(
    conn: sqlite3.Connection,
    job_id: str,
    stage_id: str,
    **fields: Any,
) -> None:
    fields = dict(fields)
    fields["updated_at"] = iso()
    cols = ", ".join(f"{k}=?" for k in fields)
    with tx(conn):
        conn.execute(
            f"UPDATE stages SET {cols} WHERE job_id=? AND stage_id=?",
            (*fields.values(), job_id, stage_id),
        )


def stage_batch_done(conn: sqlite3.Connection, job_id: str, stage_id: str, batch_id: int) -> bool:
    row = conn.execute(
        "SELECT status FROM stage_batches WHERE job_id=? AND stage_id=? AND batch_id=?",
        (job_id, stage_id, batch_id),
    ).fetchone()
    return bool(row and row["status"] == "committed")


def mark_stage_batch(
    conn: sqlite3.Connection,
    job_id: str,
    stage_id: str,
    batch_id: int,
    watermark: int | None,
    synthetic: bool = False,
    details: dict[str, Any] | None = None,
) -> None:
    with tx(conn):
        mark_stage_batch_tx(conn, job_id, stage_id, batch_id, watermark, synthetic, details)


def mark_stage_batch_tx(
    conn: sqlite3.Connection,
    job_id: str,
    stage_id: str,
    batch_id: int,
    watermark: int | None,
    synthetic: bool = False,
    details: dict[str, Any] | None = None,
) -> None:
    now = iso()
    conn.execute(
        """
        INSERT INTO stage_batches (
            job_id, stage_id, batch_id, status, synthetic, watermark,
            committed_at, details_json
        ) VALUES (?,?,?,?,?,?,?,?)
        ON CONFLICT(job_id, stage_id, batch_id) DO UPDATE SET
            status=excluded.status,
            watermark=excluded.watermark,
            committed_at=excluded.committed_at,
            details_json=excluded.details_json
        """,
        (
            job_id,
            stage_id,
            batch_id,
            "committed",
            1 if synthetic else 0,
            watermark,
            now,
            canonical_json(details or {}),
        ),
    )
    conn.execute(
        """
        UPDATE stages SET status='running', committed_batch=?,
            consumed_batch=?, watermark=COALESCE(?, watermark), updated_at=?
        WHERE job_id=? AND stage_id=?
        """,
        (batch_id, batch_id, watermark, now, job_id, stage_id),
    )
    if stage_id == "filter":
        conn.execute(
            "UPDATE job_batches SET filter_committed=1 WHERE job_id=? AND batch_id=?",
            (job_id, batch_id),
        )
    elif stage_id == "window_aggregate":
        conn.execute(
            "UPDATE job_batches SET window_committed=1, watermark=? WHERE job_id=? AND batch_id=?",
            (watermark, job_id, batch_id),
        )
    elif stage_id == "merge_skew":
        conn.execute(
            "UPDATE job_batches SET merge_committed=1 WHERE job_id=? AND batch_id=?",
            (job_id, batch_id),
        )
    elif stage_id == "sink":
        conn.execute(
            "UPDATE job_batches SET sink_committed=1 WHERE job_id=? AND batch_id=?",
            (job_id, batch_id),
        )


def record_worker(
    conn: sqlite3.Connection,
    job_id: str,
    worker_id: str,
    pid: int | None,
    status: str,
    current_task: str | None = None,
) -> None:
    now = iso()
    with tx(conn):
        conn.execute(
            """
            INSERT INTO job_workers (
                job_id, worker_id, pid, status, current_task, started_at, heartbeat_at
            ) VALUES (?,?,?,?,?,?,?)
            ON CONFLICT(job_id, worker_id) DO UPDATE SET
                pid=excluded.pid, status=excluded.status,
                current_task=excluded.current_task, heartbeat_at=excluded.heartbeat_at
            """,
            (job_id, worker_id, pid, status, current_task, now, now),
        )


def record_recovery(
    conn: sqlite3.Connection,
    job_id: str,
    stage_id: str,
    shard_id: int,
    batch_id: int,
    reason: str,
    old_pid: int | None,
    new_pid: int | None,
    takeover_no: int,
) -> None:
    with tx(conn):
        conn.execute(
            """
            INSERT INTO recoveries (
                job_id, stage_id, shard_id, batch_id, reason, old_pid,
                new_pid, takeover_no, at
            ) VALUES (?,?,?,?,?,?,?,?,?)
            """,
            (job_id, stage_id, shard_id, batch_id, reason, old_pid, new_pid, takeover_no, iso()),
        )
        conn.execute(
            """
            UPDATE stage_shards SET takeover_count=takeover_count+1, updated_at=?
            WHERE job_id=? AND stage_id=? AND shard_id=?
            """,
            (iso(), job_id, stage_id, shard_id),
        )


def _epoch_iso(epoch: int) -> str:
    from .util import ts_iso

    return ts_iso(epoch)

