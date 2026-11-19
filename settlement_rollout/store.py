from __future__ import annotations

import hashlib
import json
import sqlite3
import threading
import time
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Iterator

from .config import BatchConfig, PlanConfig
from .errors import STATE_CONFIG_MISMATCH, StateError


TERMINAL_BATCH_STATUSES = {
    "PASSED",
    "ROLLED_BACK",
    "PARTIAL_ROLLBACK",
    "ROLLBACK_FAILED",
    "ABORTED",
}

TERMINAL_RUN_STATUSES = {
    "RELEASED",
    "ROLLED_BACK",
    "PARTIAL_ROLLBACK",
    "ROLLBACK_FAILED",
    "ABORTED",
}

ALLOWED_TRANSITIONS: dict[str, set[str]] = {
    "PENDING": {"PRECHECKING", "ABORTING"},
    "PRECHECKING": {"SWITCHING", "ROLLING_BACK", "ABORTING"},
    "SWITCHING": {"SWITCHING", "OBSERVING", "ROLLING_BACK", "ABORTING"},
    "OBSERVING": {"OBSERVING", "PASSED", "ROLLING_BACK", "ABORTING"},
    "ROLLING_BACK": {"ROLLING_BACK", "ROLLED_BACK", "PARTIAL_ROLLBACK",
                     "ROLLBACK_FAILED", "ABORTING"},
    "ABORTING": {"ROLLED_BACK", "PARTIAL_ROLLBACK", "ROLLBACK_FAILED", "ABORTED"},
    "PASSED": set(),
    "ROLLED_BACK": set(),
    "PARTIAL_ROLLBACK": set(),
    "ROLLBACK_FAILED": set(),
    "ABORTED": set(),
}


def _canonical(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    )


def _load_json(text: str | None, default: Any) -> Any:
    if not text:
        return default
    return json.loads(text)


class AuditStore:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._conn = sqlite3.connect(
            str(self.path),
            timeout=30.0,
            check_same_thread=False,
            isolation_level=None,
        )
        self._conn.row_factory = sqlite3.Row
        with self._lock:
            self._conn.execute("PRAGMA journal_mode=WAL")
            self._conn.execute("PRAGMA synchronous=FULL")
            self._conn.execute("PRAGMA foreign_keys=ON")
        self._initialize()

    def close(self) -> None:
        with self._lock:
            self._conn.close()

    @contextmanager
    def transaction(self) -> Iterator[sqlite3.Connection]:
        with self._lock:
            self._conn.execute("BEGIN IMMEDIATE")
            try:
                yield self._conn
            except BaseException:
                self._conn.execute("ROLLBACK")
                raise
            else:
                self._conn.execute("COMMIT")

    def _initialize(self) -> None:
        schema = """
        CREATE TABLE IF NOT EXISTS runs (
            run_id TEXT PRIMARY KEY,
            release_id TEXT NOT NULL,
            plan_hash TEXT NOT NULL,
            status TEXT NOT NULL,
            created_ts REAL NOT NULL,
            updated_ts REAL NOT NULL,
            finished_ts REAL
        );
        CREATE TABLE IF NOT EXISTS batches (
            run_id TEXT NOT NULL,
            batch_id TEXT NOT NULL,
            sequence INTEGER NOT NULL,
            status TEXT NOT NULL,
            target_version TEXT NOT NULL,
            rollback_version TEXT NOT NULL,
            switched_json TEXT NOT NULL DEFAULT '[]',
            target_switched_json TEXT NOT NULL DEFAULT '[]',
            precheck_json TEXT NOT NULL DEFAULT '{}',
            observation_started_ts REAL,
            observation_ends_ts REAL,
            rollback_started_ts REAL,
            rollback_finished_ts REAL,
            decision_count INTEGER NOT NULL DEFAULT 0,
            rollback_count INTEGER NOT NULL DEFAULT 0,
            last_reason TEXT,
            updated_ts REAL NOT NULL,
            PRIMARY KEY (run_id, batch_id),
            FOREIGN KEY (run_id) REFERENCES runs(run_id)
        );
        CREATE TABLE IF NOT EXISTS instance_states (
            run_id TEXT NOT NULL,
            instance_id TEXT NOT NULL,
            batch_id TEXT NOT NULL,
            active_version TEXT NOT NULL,
            switched INTEGER NOT NULL DEFAULT 0,
            last_error TEXT,
            updated_ts REAL NOT NULL,
            PRIMARY KEY (run_id, instance_id),
            FOREIGN KEY (run_id) REFERENCES runs(run_id)
        );
        CREATE TABLE IF NOT EXISTS events (
            seq INTEGER PRIMARY KEY AUTOINCREMENT,
            run_id TEXT NOT NULL,
            batch_id TEXT,
            event_type TEXT NOT NULL,
            payload_json TEXT NOT NULL,
            created_ts REAL NOT NULL,
            FOREIGN KEY (run_id) REFERENCES runs(run_id)
        );
        CREATE TABLE IF NOT EXISTS metrics_snapshots (
            snapshot_id INTEGER PRIMARY KEY AUTOINCREMENT,
            run_id TEXT NOT NULL,
            batch_id TEXT,
            scope TEXT NOT NULL,
            metrics_json TEXT NOT NULL,
            created_ts REAL NOT NULL,
            FOREIGN KEY (run_id) REFERENCES runs(run_id)
        );
        CREATE TABLE IF NOT EXISTS decisions (
            decision_id INTEGER PRIMARY KEY AUTOINCREMENT,
            run_id TEXT NOT NULL,
            batch_id TEXT NOT NULL,
            verdict TEXT NOT NULL,
            reason TEXT NOT NULL,
            metrics_json TEXT NOT NULL,
            thresholds_json TEXT NOT NULL,
            triggers_json TEXT NOT NULL,
            sample_count INTEGER NOT NULL,
            created_ts REAL NOT NULL,
            FOREIGN KEY (run_id) REFERENCES runs(run_id)
        );
        CREATE TABLE IF NOT EXISTS rollback_actions (
            action_id INTEGER PRIMARY KEY AUTOINCREMENT,
            run_id TEXT NOT NULL,
            batch_id TEXT NOT NULL,
            instance_id TEXT NOT NULL,
            from_version TEXT NOT NULL,
            to_version TEXT NOT NULL,
            status TEXT NOT NULL,
            error_code TEXT,
            detail_json TEXT NOT NULL,
            created_ts REAL NOT NULL,
            FOREIGN KEY (run_id) REFERENCES runs(run_id)
        );
        CREATE TABLE IF NOT EXISTS manual_aborts (
            abort_id INTEGER PRIMARY KEY AUTOINCREMENT,
            run_id TEXT NOT NULL,
            batch_id TEXT,
            reason TEXT NOT NULL,
            metrics_json TEXT NOT NULL,
            created_ts REAL NOT NULL,
            FOREIGN KEY (run_id) REFERENCES runs(run_id)
        );
        CREATE TABLE IF NOT EXISTS observation_samples (
            run_id TEXT NOT NULL,
            batch_id TEXT NOT NULL,
            instance_id TEXT NOT NULL,
            sample_id INTEGER NOT NULL,
            version TEXT NOT NULL,
            status INTEGER NOT NULL,
            latency_ms REAL NOT NULL,
            PRIMARY KEY (run_id, batch_id, instance_id, sample_id),
            FOREIGN KEY (run_id) REFERENCES runs(run_id)
        );
        CREATE INDEX IF NOT EXISTS idx_events_run_batch
            ON events(run_id, batch_id, seq);
        CREATE INDEX IF NOT EXISTS idx_decisions_run_batch
            ON decisions(run_id, batch_id, decision_id);
        """
        with self._lock:
            self._conn.executescript(schema)

    def start_run(self, config: PlanConfig) -> str:
        run_id = hashlib.sha256(
            f"{config.release_id}:{config.logical_sha256}".encode("utf-8")
        ).hexdigest()[:16]
        now = time.time()
        with self.transaction() as conn:
            existing = conn.execute(
                "SELECT release_id, plan_hash FROM runs WHERE run_id = ?",
                (run_id,),
            ).fetchone()
            if existing is not None:
                if (
                    existing["release_id"] != config.release_id
                    or existing["plan_hash"] != config.logical_sha256
                ):
                    raise StateError(
                        STATE_CONFIG_MISMATCH,
                        "stored run does not match the supplied configuration",
                        {
                            "run_id": run_id,
                            "stored_plan_hash": existing["plan_hash"],
                            "supplied_plan_hash": config.logical_sha256,
                        },
                    )
                return run_id
            conn.execute(
                """
                INSERT INTO runs(
                    run_id, release_id, plan_hash, status, created_ts, updated_ts
                ) VALUES (?, ?, ?, 'RUNNING', ?, ?)
                """,
                (run_id, config.release_id, config.logical_sha256, now, now),
            )
            for batch in config.batches:
                conn.execute(
                    """
                    INSERT INTO batches(
                        run_id, batch_id, sequence, status, target_version,
                        rollback_version, updated_ts
                    ) VALUES (?, ?, ?, 'PENDING', ?, ?, ?)
                    """,
                    (
                        run_id,
                        batch.batch_id,
                        batch.order,
                        config.target_version,
                        batch.rollback_version,
                        now,
                    ),
                )
            for instance in config.instances:
                batch = next(
                    item
                    for item in config.batches
                    if instance.instance_id in item.instances
                )
                conn.execute(
                    """
                    INSERT INTO instance_states(
                        run_id, instance_id, batch_id, active_version,
                        switched, updated_ts
                    ) VALUES (?, ?, ?, ?, 0, ?)
                    """,
                    (
                        run_id,
                        instance.instance_id,
                        batch.batch_id,
                        instance.initial_version,
                        now,
                    ),
                )
            self._insert_event(
                conn,
                run_id,
                None,
                "RUN_CREATED",
                {
                    "release_id": config.release_id,
                    "plan_hash": config.logical_sha256,
                    "batches": [batch.batch_id for batch in config.batches],
                },
            )
        return run_id

    def _insert_event(
        self,
        conn: sqlite3.Connection,
        run_id: str,
        batch_id: str | None,
        event_type: str,
        payload: dict[str, Any],
    ) -> None:
        conn.execute(
            """
            INSERT INTO events(run_id, batch_id, event_type, payload_json, created_ts)
            VALUES (?, ?, ?, ?, ?)
            """,
            (run_id, batch_id, event_type, _canonical(payload), time.time()),
        )

    def _insert_snapshot(
        self,
        conn: sqlite3.Connection,
        run_id: str,
        batch_id: str | None,
        scope: str,
        metrics: dict[str, Any],
    ) -> None:
        conn.execute(
            """
            INSERT INTO metrics_snapshots(
                run_id, batch_id, scope, metrics_json, created_ts
            ) VALUES (?, ?, ?, ?, ?)
            """,
            (run_id, batch_id, scope, _canonical(metrics), time.time()),
        )

    def get_run(self, run_id: str) -> dict[str, Any]:
        with self._lock:
            row = self._conn.execute(
                "SELECT * FROM runs WHERE run_id = ?", (run_id,)
            ).fetchone()
        if row is None:
            raise StateError(
                "STATE_RUN_NOT_FOUND",
                f"run not found: {run_id}",
                {"run_id": run_id},
            )
        return dict(row)

    def get_batches(self, run_id: str) -> list[dict[str, Any]]:
        with self._lock:
            rows = self._conn.execute(
                "SELECT * FROM batches WHERE run_id = ? ORDER BY sequence",
                (run_id,),
            ).fetchall()
        return [self._batch_dict(row) for row in rows]

    def get_batch(self, run_id: str, batch_id: str) -> dict[str, Any]:
        with self._lock:
            row = self._conn.execute(
                "SELECT * FROM batches WHERE run_id = ? AND batch_id = ?",
                (run_id, batch_id),
            ).fetchone()
        if row is None:
            raise StateError(
                "STATE_BATCH_NOT_FOUND",
                f"batch not found: {batch_id}",
                {"run_id": run_id, "batch_id": batch_id},
            )
        return self._batch_dict(row)

    @staticmethod
    def _batch_dict(row: sqlite3.Row) -> dict[str, Any]:
        result = dict(row)
        result["switched_instances"] = _load_json(
            result.pop("switched_json"), []
        )
        result["target_switched_instances"] = _load_json(
            result.pop("target_switched_json"), []
        )
        result["precheck"] = _load_json(result.pop("precheck_json"), {})
        return result

    def get_instances(self, run_id: str) -> dict[str, dict[str, Any]]:
        with self._lock:
            rows = self._conn.execute(
                """
                SELECT * FROM instance_states
                WHERE run_id = ? ORDER BY instance_id
                """,
                (run_id,),
            ).fetchall()
        return {
            row["instance_id"]: {
                **dict(row),
                "switched": bool(row["switched"]),
            }
            for row in rows
        }

    def transition_batch(
        self,
        run_id: str,
        batch_id: str,
        status: str,
        event_type: str,
        payload: dict[str, Any],
        snapshot: dict[str, Any] | None = None,
        extra_updates: dict[str, Any] | None = None,
    ) -> None:
        now = time.time()
        with self.transaction() as conn:
            row = conn.execute(
                "SELECT status FROM batches WHERE run_id = ? AND batch_id = ?",
                (run_id, batch_id),
            ).fetchone()
            if row is None:
                raise StateError(
                    "STATE_BATCH_NOT_FOUND",
                    f"batch not found: {batch_id}",
                    {"batch_id": batch_id},
                )
            current = row["status"]
            if current != status:
                if current in TERMINAL_BATCH_STATUSES:
                    raise StateError(
                        "STATE_TERMINAL_BATCH",
                        f"batch {batch_id} is terminal ({current})",
                        {"batch_id": batch_id, "status": current},
                    )
                if status not in ALLOWED_TRANSITIONS.get(current, set()):
                    raise StateError(
                        "STATE_INVALID_TRANSITION",
                        f"invalid batch transition {current} -> {status}",
                        {
                            "batch_id": batch_id,
                            "from": current,
                            "to": status,
                        },
                    )
            assignments = ["status = ?", "updated_ts = ?"]
            values: list[Any] = [status, now]
            if extra_updates:
                allowed = {
                    "observation_started_ts",
                    "observation_ends_ts",
                    "rollback_started_ts",
                    "rollback_finished_ts",
                    "last_reason",
                }
                for key, value in extra_updates.items():
                    if key not in allowed:
                        raise ValueError(f"unsupported batch update: {key}")
                    assignments.append(f"{key} = ?")
                    values.append(value)
            values.extend([run_id, batch_id])
            conn.execute(
                f"UPDATE batches SET {', '.join(assignments)} "
                "WHERE run_id = ? AND batch_id = ?",
                values,
            )
            self._insert_event(conn, run_id, batch_id, event_type, payload)
            if snapshot is not None:
                self._insert_snapshot(
                    conn, run_id, batch_id, event_type, snapshot
                )
            else:
                self._insert_snapshot(
                    conn,
                    run_id,
                    batch_id,
                    event_type,
                    {
                        "status": status,
                        "event": event_type,
                        "payload": payload,
                    },
                )

    def set_precheck(
        self,
        run_id: str,
        batch_id: str,
        ready: list[str],
        failures: list[dict[str, Any]],
    ) -> None:
        now = time.time()
        payload = {
            "ready_instances": ready,
            "failed_instances": failures,
        }
        with self.transaction() as conn:
            conn.execute(
                """
                UPDATE batches
                SET precheck_json = ?, status = 'SWITCHING', updated_ts = ?
                WHERE run_id = ? AND batch_id = ?
                """,
                (_canonical(payload), now, run_id, batch_id),
            )
            self._insert_event(
                conn,
                run_id,
                batch_id,
                "PRECHECK_COMPLETED",
                payload,
            )
            self._insert_snapshot(
                conn,
                run_id,
                batch_id,
                "PRECHECK_COMPLETED",
                {
                    "status": "SWITCHING",
                    "ready_instance_count": len(ready),
                    "failed_instance_count": len(failures),
                },
            )

    def mark_instance_switched(
        self,
        run_id: str,
        batch_id: str,
        instance_id: str,
        version: str,
        switched: bool,
        status: str = "SWITCHED",
    ) -> None:
        now = time.time()
        with self.transaction() as conn:
            row = conn.execute(
                """
                SELECT switched_json, target_switched_json
                FROM batches WHERE run_id = ? AND batch_id = ?
                """,
                (run_id, batch_id),
            ).fetchone()
            if row is None:
                raise StateError(
                    "STATE_BATCH_NOT_FOUND",
                    f"batch not found: {batch_id}",
                    {"batch_id": batch_id},
                )
            switched_ids = _load_json(row["switched_json"], [])
            target_ids = _load_json(row["target_switched_json"], [])
            if switched and instance_id not in switched_ids:
                switched_ids.append(instance_id)
            if switched and instance_id not in target_ids:
                target_ids.append(instance_id)
            if not switched and instance_id in switched_ids:
                switched_ids.remove(instance_id)
            conn.execute(
                """
                UPDATE batches
                SET switched_json = ?, target_switched_json = ?, updated_ts = ?
                WHERE run_id = ? AND batch_id = ?
                """,
                (
                    _canonical(sorted(switched_ids)),
                    _canonical(sorted(target_ids)),
                    now,
                    run_id,
                    batch_id,
                ),
            )
            conn.execute(
                """
                UPDATE instance_states
                SET active_version = ?, switched = ?, last_error = NULL,
                    updated_ts = ?
                WHERE run_id = ? AND instance_id = ?
                """,
                (version, 1 if switched else 0, now, run_id, instance_id),
            )
            self._insert_event(
                conn,
                run_id,
                batch_id,
                status,
                {
                    "instance_id": instance_id,
                    "active_version": version,
                    "switched": switched,
                },
            )
            self._insert_snapshot(
                conn,
                run_id,
                batch_id,
                status,
                {
                    "switched_instance_count": len(switched_ids),
                    "active_version": version,
                },
            )

    def mark_instance_error(
        self,
        run_id: str,
        batch_id: str,
        instance_id: str,
        code: str,
        detail: dict[str, Any],
    ) -> None:
        now = time.time()
        with self.transaction() as conn:
            conn.execute(
                """
                UPDATE instance_states
                SET last_error = ?, updated_ts = ?
                WHERE run_id = ? AND instance_id = ?
                """,
                (code, now, run_id, instance_id),
            )
            self._insert_event(
                conn,
                run_id,
                batch_id,
                "INSTANCE_ERROR",
                {
                    "instance_id": instance_id,
                    "code": code,
                    **detail,
                },
            )
            self._insert_snapshot(
                conn,
                run_id,
                batch_id,
                "INSTANCE_ERROR",
                {
                    "instance_id": instance_id,
                    "error_code": code,
                    "detail": detail,
                },
            )

    def mark_observation_started(
        self,
        run_id: str,
        batch_id: str,
        started_ts: float,
        ends_ts: float,
    ) -> None:
        with self.transaction() as conn:
            current = conn.execute(
                "SELECT status FROM batches WHERE run_id = ? AND batch_id = ?",
                (run_id, batch_id),
            ).fetchone()
            if current is None:
                raise StateError(
                    "STATE_BATCH_NOT_FOUND",
                    f"batch not found: {batch_id}",
                    {"batch_id": batch_id},
                )
            conn.execute(
                """
                UPDATE batches
                SET status = 'OBSERVING', observation_started_ts = ?,
                    observation_ends_ts = ?, updated_ts = ?
                WHERE run_id = ? AND batch_id = ?
                """,
                (started_ts, ends_ts, time.time(), run_id, batch_id),
            )
            self._insert_event(
                conn,
                run_id,
                batch_id,
                "OBSERVATION_STARTED",
                {
                    "started_ts": started_ts,
                    "ends_ts": ends_ts,
                    "window_seconds": ends_ts - started_ts,
                },
            )
            self._insert_snapshot(
                conn,
                run_id,
                batch_id,
                "OBSERVATION_STARTED",
                {
                    "status": "OBSERVING",
                    "window_seconds": ends_ts - started_ts,
                },
            )

    def store_samples(
        self,
        run_id: str,
        batch_id: str,
        samples: list[dict[str, Any]],
    ) -> None:
        if not samples:
            return
        with self.transaction() as conn:
            conn.executemany(
                """
                INSERT OR IGNORE INTO observation_samples(
                    run_id, batch_id, instance_id, sample_id, version,
                    status, latency_ms
                ) VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                [
                    (
                        run_id,
                        batch_id,
                        sample["instance_id"],
                        int(sample["sample_id"]),
                        sample["version"],
                        int(sample["status"]),
                        float(sample["latency_ms"]),
                    )
                    for sample in samples
                ],
            )

    def load_samples(self, run_id: str, batch_id: str) -> list[dict[str, Any]]:
        with self._lock:
            rows = self._conn.execute(
                """
                SELECT instance_id, sample_id, version, status, latency_ms
                FROM observation_samples
                WHERE run_id = ? AND batch_id = ?
                ORDER BY instance_id, sample_id
                """,
                (run_id, batch_id),
            ).fetchall()
        return [dict(row) for row in rows]

    def max_sample_ids(
        self, run_id: str, batch_id: str
    ) -> dict[str, int]:
        with self._lock:
            rows = self._conn.execute(
                """
                SELECT instance_id, MAX(sample_id) AS max_id
                FROM observation_samples
                WHERE run_id = ? AND batch_id = ?
                GROUP BY instance_id
                """,
                (run_id, batch_id),
            ).fetchall()
        return {row["instance_id"]: int(row["max_id"]) for row in rows}

    def persist_interval_snapshot(
        self,
        run_id: str,
        batch_id: str,
        metrics: dict[str, Any],
    ) -> None:
        with self.transaction() as conn:
            conn.execute(
                "UPDATE batches SET updated_ts = ? WHERE run_id = ? AND batch_id = ?",
                (time.time(), run_id, batch_id),
            )
            self._insert_event(
                conn,
                run_id,
                batch_id,
                "SLI_SNAPSHOT",
                {
                    "metrics": metrics,
                },
            )
            self._insert_snapshot(
                conn, run_id, batch_id, "SLI_SNAPSHOT", metrics
            )

    def record_decision(
        self,
        run_id: str,
        batch_id: str,
        verdict: str,
        reason: str,
        metrics: dict[str, Any],
        thresholds: dict[str, Any],
        triggers: list[dict[str, Any]],
        status: str,
    ) -> None:
        now = time.time()
        with self.transaction() as conn:
            row = conn.execute(
                "SELECT status FROM batches WHERE run_id = ? AND batch_id = ?",
                (run_id, batch_id),
            ).fetchone()
            if row is None:
                raise StateError(
                    "STATE_BATCH_NOT_FOUND",
                    f"batch not found: {batch_id}",
                    {"batch_id": batch_id},
                )
            if row["status"] in TERMINAL_BATCH_STATUSES:
                raise StateError(
                    "STATE_TERMINAL_BATCH",
                    f"batch {batch_id} is already terminal",
                    {"batch_id": batch_id, "status": row["status"]},
                )
            if status not in ALLOWED_TRANSITIONS.get(row["status"], set()):
                raise StateError(
                    "STATE_INVALID_TRANSITION",
                    f"invalid decision transition {row['status']} -> {status}",
                    {
                        "batch_id": batch_id,
                        "from": row["status"],
                        "to": status,
                    },
                )
            conn.execute(
                """
                UPDATE batches
                SET status = ?, decision_count = decision_count + 1,
                    last_reason = ?, updated_ts = ?
                WHERE run_id = ? AND batch_id = ?
                """,
                (status, reason, now, run_id, batch_id),
            )
            conn.execute(
                """
                INSERT INTO decisions(
                    run_id, batch_id, verdict, reason, metrics_json,
                    thresholds_json, triggers_json, sample_count, created_ts
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    run_id,
                    batch_id,
                    verdict,
                    reason,
                    _canonical(metrics),
                    _canonical(thresholds),
                    _canonical(triggers),
                    int(metrics.get("sample_count", 0)),
                    now,
                ),
            )
            self._insert_snapshot(
                conn, run_id, batch_id, "DECISION", metrics
            )
            self._insert_event(
                conn,
                run_id,
                batch_id,
                "DECISION_RECORDED",
                {
                    "verdict": verdict,
                    "reason": reason,
                    "metrics": metrics,
                    "thresholds": thresholds,
                    "triggers": triggers,
                },
            )

    def record_rollback_action(
        self,
        run_id: str,
        batch_id: str,
        instance_id: str,
        from_version: str,
        to_version: str,
        success: bool,
        error_code: str | None,
        detail: dict[str, Any],
        metrics: dict[str, Any],
    ) -> None:
        now = time.time()
        with self.transaction() as conn:
            conn.execute(
                """
                INSERT INTO rollback_actions(
                    run_id, batch_id, instance_id, from_version, to_version,
                    status, error_code, detail_json, created_ts
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    run_id,
                    batch_id,
                    instance_id,
                    from_version,
                    to_version,
                    "SUCCEEDED" if success else "FAILED",
                    error_code,
                    _canonical(detail),
                    now,
                ),
            )
            conn.execute(
                """
                UPDATE batches
                SET rollback_count = rollback_count + 1, updated_ts = ?
                WHERE run_id = ? AND batch_id = ?
                """,
                (now, run_id, batch_id),
            )
            if success:
                row = conn.execute(
                    """
                    SELECT switched_json FROM batches
                    WHERE run_id = ? AND batch_id = ?
                    """,
                    (run_id, batch_id),
                ).fetchone()
                switched = _load_json(row["switched_json"], [])
                if instance_id in switched:
                    switched.remove(instance_id)
                conn.execute(
                    """
                    UPDATE batches SET switched_json = ?
                    WHERE run_id = ? AND batch_id = ?
                    """,
                    (_canonical(sorted(switched)), run_id, batch_id),
                )
                conn.execute(
                    """
                    UPDATE instance_states
                    SET active_version = ?, switched = 0, last_error = NULL,
                        updated_ts = ?
                    WHERE run_id = ? AND instance_id = ?
                    """,
                    (to_version, now, run_id, instance_id),
                )
            else:
                conn.execute(
                    """
                    UPDATE instance_states
                    SET last_error = ?, updated_ts = ?
                    WHERE run_id = ? AND instance_id = ?
                    """,
                    (error_code, now, run_id, instance_id),
                )
            self._insert_event(
                conn,
                run_id,
                batch_id,
                "ROLLBACK_INSTANCE_SUCCEEDED" if success else "ROLLBACK_INSTANCE_FAILED",
                {
                    "instance_id": instance_id,
                    "from_version": from_version,
                    "to_version": to_version,
                    "error_code": error_code,
                    "detail": detail,
                },
            )
            self._insert_snapshot(conn, run_id, batch_id, "ROLLBACK", metrics)

    def record_manual_abort(
        self,
        run_id: str,
        batch_id: str | None,
        reason: str,
        metrics: dict[str, Any],
    ) -> None:
        now = time.time()
        with self.transaction() as conn:
            conn.execute(
                """
                INSERT INTO manual_aborts(
                    run_id, batch_id, reason, metrics_json, created_ts
                ) VALUES (?, ?, ?, ?, ?)
                """,
                (
                    run_id,
                    batch_id,
                    reason,
                    _canonical(metrics),
                    now,
                ),
            )
            if batch_id:
                conn.execute(
                    """
                    UPDATE batches
                    SET status = 'ABORTING', last_reason = 'manual_abort',
                        updated_ts = ?
                    WHERE run_id = ? AND batch_id = ?
                      AND status NOT IN (
                        'PASSED','ROLLED_BACK','PARTIAL_ROLLBACK',
                        'ROLLBACK_FAILED','ABORTED'
                      )
                    """,
                    (now, run_id, batch_id),
                )
            conn.execute(
                """
                UPDATE runs SET status = 'ABORTING', updated_ts = ?
                WHERE run_id = ?
                """,
                (now, run_id),
            )
            self._insert_event(
                conn,
                run_id,
                batch_id,
                "MANUAL_ABORT",
                {
                    "reason": reason,
                    "metrics": metrics,
                },
            )
            self._insert_snapshot(
                conn, run_id, batch_id, "MANUAL_ABORT", metrics
            )

    def finish_batch(
        self,
        run_id: str,
        batch_id: str,
        status: str,
        reason: str,
        snapshot: dict[str, Any],
        set_rollback_finished: bool = False,
    ) -> None:
        extra: dict[str, Any] = {"last_reason": reason}
        if set_rollback_finished:
            extra["rollback_finished_ts"] = time.time()
        self.transition_batch(
            run_id,
            batch_id,
            status,
            "BATCH_FINISHED",
            {"status": status, "reason": reason},
            snapshot=snapshot,
            extra_updates=extra,
        )

    def finish_run(
        self,
        run_id: str,
        status: str,
        reason: str,
        metrics: dict[str, Any] | None = None,
    ) -> None:
        now = time.time()
        with self.transaction() as conn:
            row = conn.execute(
                "SELECT status FROM runs WHERE run_id = ?", (run_id,)
            ).fetchone()
            if row is None:
                raise StateError(
                    "STATE_RUN_NOT_FOUND",
                    f"run not found: {run_id}",
                    {"run_id": run_id},
                )
            if row["status"] in TERMINAL_RUN_STATUSES:
                if row["status"] == status:
                    return
                raise StateError(
                    "STATE_TERMINAL_RUN",
                    f"run {run_id} is terminal ({row['status']})",
                    {"run_id": run_id, "status": row["status"]},
                )
            conn.execute(
                """
                UPDATE runs
                SET status = ?, updated_ts = ?, finished_ts = ?
                WHERE run_id = ?
                """,
                (status, now, now, run_id),
            )
            self._insert_event(
                conn,
                run_id,
                None,
                "RUN_FINISHED",
                {"status": status, "reason": reason},
            )
            if metrics is not None:
                self._insert_snapshot(conn, run_id, None, "RUN_FINISHED", metrics)

    def latest_snapshot(
        self, run_id: str, batch_id: str
    ) -> dict[str, Any]:
        with self._lock:
            row = self._conn.execute(
                """
                SELECT metrics_json FROM metrics_snapshots
                WHERE run_id = ? AND batch_id = ?
                ORDER BY snapshot_id DESC LIMIT 1
                """,
                (run_id, batch_id),
            ).fetchone()
        if row is None:
            return {}
        return _load_json(row["metrics_json"], {})

    def latest_decision(
        self, run_id: str, batch_id: str
    ) -> dict[str, Any] | None:
        with self._lock:
            row = self._conn.execute(
                """
                SELECT * FROM decisions
                WHERE run_id = ? AND batch_id = ?
                ORDER BY decision_id DESC LIMIT 1
                """,
                (run_id, batch_id),
            ).fetchone()
        if row is None:
            return None
        return self._decision_dict(row)

    @staticmethod
    def _decision_dict(row: sqlite3.Row) -> dict[str, Any]:
        result = dict(row)
        result["metrics"] = _load_json(result.pop("metrics_json"), {})
        result["thresholds"] = _load_json(result.pop("thresholds_json"), {})
        result["triggers"] = _load_json(result.pop("triggers_json"), [])
        return result

    def audit_batch(self, run_id: str, batch_id: str) -> dict[str, Any]:
        batch = self.get_batch(run_id, batch_id)
        with self._lock:
            decisions = self._conn.execute(
                """
                SELECT * FROM decisions
                WHERE run_id = ? AND batch_id = ?
                ORDER BY decision_id
                """,
                (run_id, batch_id),
            ).fetchall()
            events = self._conn.execute(
                """
                SELECT * FROM events
                WHERE run_id = ? AND batch_id = ?
                ORDER BY seq
                """,
                (run_id, batch_id),
            ).fetchall()
            rollbacks = self._conn.execute(
                """
                SELECT * FROM rollback_actions
                WHERE run_id = ? AND batch_id = ?
                ORDER BY action_id
                """,
                (run_id, batch_id),
            ).fetchall()
            snapshots = self._conn.execute(
                """
                SELECT * FROM metrics_snapshots
                WHERE run_id = ? AND batch_id = ?
                ORDER BY snapshot_id
                """,
                (run_id, batch_id),
            ).fetchall()
            aborts = self._conn.execute(
                """
                SELECT * FROM manual_aborts
                WHERE run_id = ? AND (batch_id = ? OR batch_id IS NULL)
                ORDER BY abort_id
                """,
                (run_id, batch_id),
            ).fetchall()
        return {
            "run_id": run_id,
            "batch": batch,
            "decisions": [self._decision_dict(row) for row in decisions],
            "events": [
                {
                    **dict(row),
                    "payload": _load_json(row["payload_json"], {}),
                }
                for row in events
            ],
            "rollback_actions": [
                {
                    **dict(row),
                    "detail": _load_json(row["detail_json"], {}),
                }
                for row in rollbacks
            ],
            "snapshots": [
                {
                    **dict(row),
                    "metrics": _load_json(row["metrics_json"], {}),
                }
                for row in snapshots
            ],
            "manual_aborts": [
                {
                    **dict(row),
                    "metrics": _load_json(row["metrics_json"], {}),
                }
                for row in aborts
            ],
        }

    def status_summary(self, run_id: str) -> dict[str, Any]:
        run = self.get_run(run_id)
        batches = self.get_batches(run_id)
        now = time.time()
        entries: list[dict[str, Any]] = []
        current: dict[str, Any] | None = None
        for batch in batches:
            remaining = None
            if (
                batch["status"] == "OBSERVING"
                and batch["observation_ends_ts"] is not None
            ):
                remaining = round(
                    max(0.0, float(batch["observation_ends_ts"]) - now), 3
                )
            entry = {
                "batch_id": batch["batch_id"],
                "sequence": batch["sequence"],
                "status": batch["status"],
                "target_version": batch["target_version"],
                "rollback_version": batch["rollback_version"],
                "switched_instance_count": len(batch["switched_instances"]),
                "switched_instances": batch["switched_instances"],
                "decision_count": batch["decision_count"],
                "rollback_count": batch["rollback_count"],
                "observation_seconds_remaining": remaining,
                "last_reason": batch["last_reason"],
            }
            entries.append(entry)
            if (
                current is None
                and run["status"] not in TERMINAL_RUN_STATUSES
                and batch["status"] not in TERMINAL_BATCH_STATUSES
            ):
                current = entry
        return {
            "run_id": run_id,
            "release_id": run["release_id"],
            "status": run["status"],
            "current_batch": current,
            "batches": entries,
        }

    def all_events(self, run_id: str) -> list[dict[str, Any]]:
        with self._lock:
            rows = self._conn.execute(
                "SELECT * FROM events WHERE run_id = ? ORDER BY seq",
                (run_id,),
            ).fetchall()
        return [
            {
                **dict(row),
                "payload": _load_json(row["payload_json"], {}),
            }
            for row in rows
        ]

    def all_rollbacks(self, run_id: str) -> list[dict[str, Any]]:
        with self._lock:
            rows = self._conn.execute(
                """
                SELECT * FROM rollback_actions
                WHERE run_id = ? ORDER BY action_id
                """,
                (run_id,),
            ).fetchall()
        return [
            {
                **dict(row),
                "detail": _load_json(row["detail_json"], {}),
            }
            for row in rows
        ]
