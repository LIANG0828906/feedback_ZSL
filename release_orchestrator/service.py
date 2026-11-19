"""Transactional gray-release state machine."""

from __future__ import annotations

import json
import os
import sqlite3
from pathlib import Path
from typing import Any, Mapping

from .dataset import load_metrics, load_version_reports
from .decision import evaluate_batch
from .errors import BusinessError
from .models import (
    Decision,
    ReleasePlan,
    decision_violations,
    parse_timestamp,
    stable_json,
    threshold_instance_ids,
    timestamp_to_iso,
    utc_now,
)
from .plan import load_plan, parse_plan
from .storage import Database


ACTIVE_STATUSES = {"observing", "in_progress"}
ACTIVE_RELEASE_STATUSES = {
    "observing",
    "in_progress",
    "rollback_in_progress",
}


def _json_loads(text: str | None, default: Any) -> Any:
    if not text:
        return default
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        return default


def _unique_sorted(values: list[Any] | tuple[Any, ...] | set[Any]) -> list[Any]:
    return sorted(set(values), key=str)


class Orchestrator:
    """Persistent orchestrator.

    The database is the only state store. A fresh ``Orchestrator`` instance
    opened on the same SQLite file can continue every unfinished release.
    """

    def __init__(
        self,
        db_path: str = "orchestrator.db",
        *,
        samples_dir: str | None = None,
    ) -> None:
        self.db_path = str(db_path)
        self.samples_dir = (
            str(Path(samples_dir).expanduser().resolve()) if samples_dir else None
        )
        self.db = Database(self.db_path)

    # ------------------------------------------------------------------
    # Plan and input handling
    # ------------------------------------------------------------------
    def _resolve_plan(
        self,
        plan: str | os.PathLike[str] | ReleasePlan | Mapping[str, Any],
        *,
        sample_dir: str | None = None,
    ) -> ReleasePlan:
        source_dir = sample_dir or self.samples_dir
        if isinstance(plan, ReleasePlan):
            return plan
        if isinstance(plan, Mapping):
            return parse_plan(dict(plan), base_dir=source_dir)
        path = Path(plan)
        if source_dir and not path.is_absolute():
            candidate = Path(source_dir) / path
            if candidate.is_file():
                path = candidate
        return load_plan(path)

    def _resolve_paths(
        self,
        plan: ReleasePlan,
        metrics_path: str | None,
        version_reports_path: str | None,
    ) -> tuple[str, str | None]:
        base = (
            Path(plan.plan_path).parent
            if plan.plan_path
            else Path(self.samples_dir or os.getcwd())
        )
        metrics_value = metrics_path or plan.metrics_file
        if metrics_value is None:
            if self.samples_dir:
                metrics_value = str(Path(self.samples_dir) / "metrics.jsonl")
            else:
                metrics_value = str(base / "metrics.jsonl")
        metrics = Path(metrics_value).expanduser()
        if not metrics.is_absolute():
            metrics = base / metrics
        metrics = metrics.resolve()

        reports_value = version_reports_path or plan.version_reports_file
        if reports_value is None:
            candidates = [
                base / "version_reports.jsonl",
                base / "version_reports.json",
                base / "version_reports.csv",
            ]
            if self.samples_dir:
                candidates = [
                    Path(self.samples_dir) / candidate.name
                    for candidate in candidates
                ]
            reports = next(
                (candidate.resolve() for candidate in candidates if candidate.is_file()),
                candidates[0].resolve(),
            )
        else:
            reports = Path(reports_value).expanduser()
            if not reports.is_absolute():
                reports = base / reports
            reports = reports.resolve()
            if not reports.exists():
                raise BusinessError(
                    "DATA_FILE_NOT_FOUND",
                    f"version reports file not found: {reports}",
                    details={"path": str(reports)},
                    status_code=422,
                )
        return str(metrics), str(reports) if reports else None

    def start_from_sample_dir(
        self,
        sample_dir: str,
        *,
        metrics_path: str | None = None,
        version_reports_path: str | None = None,
    ) -> dict[str, Any]:
        directory = Path(sample_dir).expanduser().resolve()
        plan_path = directory / "plan.json"
        if not plan_path.is_file():
            raise BusinessError(
                "PLAN_NOT_FOUND",
                f"plan.json not found in sample directory: {directory}",
                details={"path": str(plan_path)},
                status_code=422,
            )
        return self.start_release(
            str(plan_path),
            metrics_path=metrics_path,
            version_reports_path=version_reports_path,
            sample_dir=str(directory),
        )

    def start_release(
        self,
        plan: str | os.PathLike[str] | ReleasePlan | Mapping[str, Any],
        *,
        metrics_path: str | None = None,
        version_reports_path: str | None = None,
        sample_dir: str | None = None,
    ) -> dict[str, Any]:
        """Validate all inputs, then atomically create a release and batches."""

        canonical_plan = self._resolve_plan(plan, sample_dir=sample_dir)
        metrics_file, reports_file = self._resolve_paths(
            canonical_plan, metrics_path, version_reports_path
        )
        metric_rows = load_metrics(metrics_file, canonical_plan)
        report_rows = (
            load_version_reports(reports_file, canonical_plan)
            if reports_file and Path(reports_file).exists()
            else []
        )

        plan_payload = canonical_plan.to_dict()
        now = utc_now()
        first_batch = canonical_plan.batches[0].batch
        with self.db.transaction() as connection:
            existing = connection.execute(
                "SELECT * FROM releases WHERE release_id = ?",
                (canonical_plan.release_id,),
            ).fetchone()
            if existing is not None:
                if (
                    existing["plan_sha256"] == canonical_plan.plan_sha256
                    and existing["metrics_path"] == metrics_file
                ):
                    return self._status_from_connection(
                        connection, canonical_plan.release_id
                    )
                raise BusinessError(
                    "RELEASE_ALREADY_EXISTS",
                    f"release already exists: {canonical_plan.release_id}",
                    details={"release_id": canonical_plan.release_id},
                    status_code=409,
                )

            connection.execute(
                """
                INSERT INTO releases (
                    release_id, parameter_key, target_version,
                    previous_stable_version, status, current_batch,
                    next_batch, total_batches, rollback_target_version,
                    rollback_required, plan_json, plan_path, metrics_path,
                    version_reports_path, plan_sha256, created_at, updated_at
                ) VALUES (?, ?, ?, ?, 'observing', ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    canonical_plan.release_id,
                    canonical_plan.parameter_key,
                    canonical_plan.target_version,
                    canonical_plan.previous_stable_version,
                    first_batch,
                    first_batch,
                    canonical_plan.batch_count,
                    canonical_plan.previous_stable_version,
                    stable_json(plan_payload),
                    canonical_plan.plan_path,
                    metrics_file,
                    reports_file,
                    canonical_plan.plan_sha256,
                    now,
                    now,
                ),
            )
            for batch in canonical_plan.batches:
                connection.execute(
                    """
                    INSERT INTO batches (
                        release_id, batch_no, observation_seconds,
                        start_offset_seconds, instances_json, status
                    ) VALUES (?, ?, ?, ?, ?, 'pending')
                    """,
                    (
                        canonical_plan.release_id,
                        batch.batch,
                        batch.observation_seconds,
                        batch.start_offset_seconds,
                        stable_json(list(batch.instances)),
                    ),
                )

            connection.executemany(
                """
                INSERT INTO samples (
                    release_id, batch_no, instance_id, observed_at,
                    observed_at_raw, error_rate, connection_reject_rate,
                    request_count, parameter_version
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(release_id, batch_no, instance_id, observed_at)
                DO UPDATE SET
                    error_rate = excluded.error_rate,
                    connection_reject_rate = excluded.connection_reject_rate,
                    request_count = excluded.request_count,
                    parameter_version = excluded.parameter_version
                """,
                [
                    (
                        row["release_id"],
                        row["batch"],
                        row["instance_id"],
                        row["observed_at"],
                        row["observed_at_raw"],
                        row["error_rate"],
                        row["connection_reject_rate"],
                        row["request_count"],
                        row["parameter_version"],
                    )
                    for row in metric_rows
                ],
            )
            connection.executemany(
                """
                INSERT INTO version_reports (
                    release_id, instance_id, observed_at, observed_at_raw,
                    parameter_version
                ) VALUES (?, ?, ?, ?, ?)
                ON CONFLICT(release_id, instance_id, observed_at)
                DO UPDATE SET parameter_version = excluded.parameter_version
                """,
                [
                    (
                        row["release_id"],
                        row["instance_id"],
                        row["observed_at"],
                        row["observed_at_raw"],
                        row["parameter_version"],
                    )
                    for row in report_rows
                ],
            )
            self._insert_audit(
                connection,
                canonical_plan.release_id,
                None,
                "release_started",
                "observing",
                None,
                {
                    "type": "release_started",
                    "release_id": canonical_plan.release_id,
                    "plan_sha256": canonical_plan.plan_sha256,
                    "metrics_path": metrics_file,
                    "version_reports_path": reports_file,
                    "metric_rows": len(metric_rows),
                    "version_report_rows": len(report_rows),
                    "batches": [
                        {
                            "batch": batch.batch,
                            "instances": list(batch.instances),
                            "observation_seconds": batch.observation_seconds,
                        }
                        for batch in canonical_plan.batches
                    ],
                },
            )
            return self._status_from_connection(
                connection, canonical_plan.release_id
            )

    def refresh_inputs(self, release_id: str) -> None:
        """Upsert newly appended local metrics/report rows."""

        with self.db.reader() as connection:
            row = connection.execute(
                "SELECT * FROM releases WHERE release_id = ?", (release_id,)
            ).fetchone()
        if row is None:
            raise BusinessError(
                "RELEASE_NOT_FOUND",
                f"release not found: {release_id}",
                details={"release_id": release_id},
                status_code=404,
            )
        plan = parse_plan(json.loads(row["plan_json"]))
        metric_rows = load_metrics(row["metrics_path"], plan)
        report_rows = (
            load_version_reports(row["version_reports_path"], plan)
            if row["version_reports_path"]
            and Path(row["version_reports_path"]).exists()
            else []
        )
        with self.db.transaction() as connection:
            connection.executemany(
                """
                INSERT INTO samples (
                    release_id, batch_no, instance_id, observed_at,
                    observed_at_raw, error_rate, connection_reject_rate,
                    request_count, parameter_version
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(release_id, batch_no, instance_id, observed_at)
                DO UPDATE SET
                    error_rate = excluded.error_rate,
                    connection_reject_rate = excluded.connection_reject_rate,
                    request_count = excluded.request_count,
                    parameter_version = excluded.parameter_version
                """,
                [
                    (
                        item["release_id"],
                        item["batch"],
                        item["instance_id"],
                        item["observed_at"],
                        item["observed_at_raw"],
                        item["error_rate"],
                        item["connection_reject_rate"],
                        item["request_count"],
                        item["parameter_version"],
                    )
                    for item in metric_rows
                ],
            )
            connection.executemany(
                """
                INSERT INTO version_reports (
                    release_id, instance_id, observed_at, observed_at_raw,
                    parameter_version
                ) VALUES (?, ?, ?, ?, ?)
                ON CONFLICT(release_id, instance_id, observed_at)
                DO UPDATE SET parameter_version = excluded.parameter_version
                """,
                [
                    (
                        item["release_id"],
                        item["instance_id"],
                        item["observed_at"],
                        item["observed_at_raw"],
                        item["parameter_version"],
                    )
                    for item in report_rows
                ],
            )

    # ------------------------------------------------------------------
    # Transactions and state transitions
    # ------------------------------------------------------------------
    @staticmethod
    def _insert_audit(
        connection: sqlite3.Connection,
        release_id: str,
        batch_no: int | None,
        record_type: str,
        result: str | None,
        reason_code: str | None,
        payload: dict[str, Any],
    ) -> None:
        connection.execute(
            """
            INSERT INTO decision_records (
                release_id, batch_no, record_type, result, reason_code,
                record_json, created_at
            ) VALUES (?, ?, ?, ?, ?, ?, ?)
            """,
            (
                release_id,
                batch_no,
                record_type,
                result,
                reason_code,
                stable_json(payload),
                utc_now(),
            ),
        )

    @staticmethod
    def _fetch_release(
        connection: sqlite3.Connection, release_id: str
    ) -> sqlite3.Row:
        row = connection.execute(
            "SELECT * FROM releases WHERE release_id = ?", (release_id,)
        ).fetchone()
        if row is None:
            raise BusinessError(
                "RELEASE_NOT_FOUND",
                f"release not found: {release_id}",
                details={"release_id": release_id},
                status_code=404,
            )
        return row

    @staticmethod
    def _fetch_batches(
        connection: sqlite3.Connection, release_id: str
    ) -> list[sqlite3.Row]:
        return list(
            connection.execute(
                """
                SELECT * FROM batches
                WHERE release_id = ?
                ORDER BY batch_no
                """,
                (release_id,),
            ).fetchall()
        )

    @staticmethod
    def _lock_conflicts(
        connection: sqlite3.Connection,
        release_id: str,
        instances: list[str],
    ) -> list[dict[str, Any]]:
        if not instances:
            return []
        placeholders = ",".join("?" for _ in instances)
        rows = connection.execute(
            f"""
            SELECT instance_id, release_id, batch_no
            FROM instance_locks
            WHERE instance_id IN ({placeholders})
              AND release_id <> ?
            ORDER BY instance_id, release_id
            """,
            [*instances, release_id],
        ).fetchall()
        return [
            {
                "instance_id": row["instance_id"],
                "release_id": row["release_id"],
                "batch": int(row["batch_no"]),
            }
            for row in rows
        ]

    @staticmethod
    def _cutoff_at(
        connection: sqlite3.Connection,
        release_id: str,
        through_batch: int,
    ) -> float:
        """Latest metric time in batches already attempted.

        Version reports commonly arrive in one deterministic fixture file with
        rollback observations timestamped later than the failed observation
        window. Counting those reports here would exclude the very rollback
        evidence we need to verify.
        """

        row = connection.execute(
            """
            SELECT MAX(observed_at) AS cutoff
            FROM samples
            WHERE release_id = ? AND batch_no <= ?
            """,
            (release_id, through_batch),
        ).fetchone()
        return float(row["cutoff"] if row and row["cutoff"] is not None else 0.0)

    @staticmethod
    def _trigger_data(decision: Decision) -> tuple[list[str], list[dict[str, Any]]]:
        violations = [item.to_dict() for item in decision.violations]
        instances = sorted(
            {
                str(item["instance_id"])
                for item in violations
                if item.get("instance_id")
            }
        )
        if decision.conflicts:
            instances = sorted(
                set(instances)
                | {str(item["instance_id"]) for item in decision.conflicts}
            )
            for conflict in decision.conflicts:
                violations.append(
                    {
                        "scope": "instance",
                        "instance_id": conflict["instance_id"],
                        "metric": "instance_occupancy",
                        "value": None,
                        "threshold": None,
                        "sample_count": None,
                        "conflict_release_id": conflict["release_id"],
                        "conflict_batch": conflict["batch"],
                    }
                )
        return instances, sorted(
            violations,
            key=lambda item: (
                str(item.get("instance_id") or ""),
                str(item.get("metric") or ""),
                str(item.get("conflict_release_id") or ""),
            ),
        )

    def evaluate_next(
        self, release_id: str, *, refresh: bool = True
    ) -> dict[str, Any]:
        """Evaluate and persist exactly the next unreleased batch."""

        if refresh:
            self.refresh_inputs(release_id)

        with self.db.transaction() as connection:
            release = self._fetch_release(connection, release_id)
            if release["status"] not in ACTIVE_STATUSES:
                raise BusinessError(
                    "RELEASE_TERMINAL",
                    "release is not active and cannot start another batch",
                    details={
                        "release_id": release_id,
                        "status": release["status"],
                    },
                    status_code=409,
                )
            batches = self._fetch_batches(connection, release_id)
            pending = [batch for batch in batches if batch["status"] == "pending"]
            if not pending:
                raise BusinessError(
                    "NO_PENDING_BATCH",
                    "release has no pending batch",
                    details={"release_id": release_id},
                    status_code=409,
                )
            batch_row = pending[0]
            batch_no = int(batch_row["batch_no"])
            instances = [
                str(item) for item in json.loads(batch_row["instances_json"])
            ]
            plan = parse_plan(json.loads(release["plan_json"]))
            batch_plan = plan.batch(batch_no)
            if batch_plan is None:
                raise BusinessError(
                    "PLAN_SCHEMA_INVALID",
                    "stored batch is absent from stored plan",
                    details={"release_id": release_id, "batch": batch_no},
                    status_code=500,
                )

            conflicts = self._lock_conflicts(connection, release_id, instances)
            sample_rows = connection.execute(
                """
                SELECT * FROM samples
                WHERE release_id = ? AND batch_no = ?
                ORDER BY instance_id, observed_at
                """,
                (release_id, batch_no),
            ).fetchall()
            decision = evaluate_batch(
                plan,
                batch_plan,
                [dict(row) for row in sample_rows],
                conflicts=conflicts,
            )
            decision_payload = decision.to_dict()
            trigger_instances, trigger_metrics = self._trigger_data(decision)
            now = utc_now()

            if decision.passed:
                # All locks are inserted in the decision transaction. A
                # conflicting batch is aborted, never partially released.
                connection.executemany(
                    """
                    INSERT INTO instance_locks (
                        instance_id, release_id, batch_no, acquired_at
                    ) VALUES (?, ?, ?, ?)
                    ON CONFLICT(instance_id) DO NOTHING
                    """,
                    [
                        (instance_id, release_id, batch_no, now)
                        for instance_id in instances
                    ],
                )
                connection.execute(
                    """
                    UPDATE batches
                    SET status = 'passed', result = 'released',
                        reason_code = NULL, decision_json = ?,
                        started_at = COALESCE(started_at, ?), completed_at = ?
                    WHERE release_id = ? AND batch_no = ?
                    """,
                    (
                        stable_json(decision_payload),
                        now,
                        now,
                        release_id,
                        batch_no,
                    ),
                )
                next_batch_no = batch_no + 1
                if next_batch_no > int(release["total_batches"]):
                    release_status = "completed"
                    current_batch = batch_no
                    next_batch = batch_no + 1
                    connection.execute(
                        "DELETE FROM instance_locks WHERE release_id = ?",
                        (release_id,),
                    )
                else:
                    release_status = "in_progress"
                    current_batch = next_batch_no
                    next_batch = next_batch_no
                connection.execute(
                    """
                    UPDATE releases
                    SET status = ?, current_batch = ?, next_batch = ?,
                        updated_at = ?
                    WHERE release_id = ?
                    """,
                    (release_status, current_batch, next_batch, now, release_id),
                )
                record_payload = {
                    "type": "batch_decision",
                    "release_id": release_id,
                    "batch": batch_no,
                    "result": "released",
                    "reason_code": None,
                    "decision": decision_payload,
                    "rollback_target_version": plan.previous_stable_version,
                    "later_batches": "not_released",
                    "release_status_after": release_status,
                }
                self._insert_audit(
                    connection,
                    release_id,
                    batch_no,
                    "batch_decision",
                    "released",
                    None,
                    record_payload,
                )
            else:
                is_occupancy = decision.reason_code == "INSTANCE_OCCUPANCY_CONFLICT"
                if not is_occupancy:
                    # The failed batch was observed against its target version.
                    # Keep those instances occupied until rollback has been
                    # verified, exactly like instances from passed batches.
                    connection.executemany(
                        """
                        INSERT INTO instance_locks (
                            instance_id, release_id, batch_no, acquired_at
                        ) VALUES (?, ?, ?, ?)
                        ON CONFLICT(instance_id) DO NOTHING
                        """,
                        [
                            (instance_id, release_id, batch_no, now)
                            for instance_id in instances
                        ],
                    )
                released_before = connection.execute(
                    """
                    SELECT COUNT(*) AS count FROM instance_locks
                    WHERE release_id = ?
                    """,
                    (release_id,),
                ).fetchone()["count"]
                rollback_required = (not is_occupancy) or released_before > 0
                if rollback_required:
                    release_status = "rollback_in_progress"
                    cutoff = self._cutoff_at(connection, release_id, batch_no)
                    connection.execute(
                        """
                        UPDATE releases
                        SET status = ?, rollback_required = 1,
                            rollback_trigger_batch = ?,
                            rollback_trigger_instances_json = ?,
                            rollback_trigger_metrics_json = ?,
                            rollback_started_at = ?, rollback_cutoff_at = ?,
                            rollback_ineffective_json = '[]',
                            rollback_checked_at = NULL,
                            conflict_release_ids_json = ?,
                            updated_at = ?
                        WHERE release_id = ?
                        """,
                        (
                            release_status,
                            batch_no,
                            stable_json(trigger_instances),
                            stable_json(trigger_metrics),
                            now,
                            cutoff,
                            stable_json(
                                sorted(
                                    {
                                        str(item["release_id"])
                                        for item in decision.conflicts
                                    }
                                )
                            ),
                            now,
                            release_id,
                        ),
                    )
                else:
                    release_status = "blocked"
                    connection.execute(
                        """
                        UPDATE releases
                        SET status = ?, conflict_release_ids_json = ?,
                            updated_at = ?
                        WHERE release_id = ?
                        """,
                        (
                            release_status,
                            stable_json(
                                sorted(
                                    {
                                        str(item["release_id"])
                                        for item in decision.conflicts
                                    }
                                )
                            ),
                            now,
                            release_id,
                        ),
                    )
                connection.execute(
                    """
                    UPDATE batches
                    SET status = 'aborted', result = 'aborted',
                        reason_code = ?, decision_json = ?,
                        started_at = COALESCE(started_at, ?), completed_at = ?
                    WHERE release_id = ? AND batch_no = ?
                    """,
                    (
                        decision.reason_code,
                        stable_json(decision_payload),
                        now,
                        now,
                        release_id,
                        batch_no,
                    ),
                )
                record_payload = {
                    "type": "batch_decision",
                    "release_id": release_id,
                    "batch": batch_no,
                    "result": "aborted",
                    "reason_code": decision.reason_code,
                    "decision": decision_payload,
                    "trigger_batch": batch_no,
                    "trigger_instances": trigger_instances,
                    "trigger_metrics": trigger_metrics,
                    "rollback_target_version": plan.previous_stable_version,
                    "rollback_required": rollback_required,
                    "later_batches": "not_released",
                    "release_status_after": release_status,
                }
                self._insert_audit(
                    connection,
                    release_id,
                    batch_no,
                    "batch_decision",
                    "aborted",
                    decision.reason_code,
                    record_payload,
                )
            return self._status_from_connection(connection, release_id)

    def run_release(
        self,
        release_id: str,
        *,
        max_batches: int | None = None,
        refresh: bool = True,
    ) -> dict[str, Any]:
        """Advance until completion, rollback, conflict, or ``max_batches``."""

        processed = 0
        while max_batches is None or processed < max_batches:
            with self.db.reader() as connection:
                release = self._fetch_release(connection, release_id)
            if release["status"] not in ACTIVE_STATUSES:
                break
            pending_exists = False
            with self.db.reader() as connection:
                pending_exists = (
                    connection.execute(
                        """
                        SELECT 1 FROM batches
                        WHERE release_id = ? AND status = 'pending'
                        LIMIT 1
                        """,
                        (release_id,),
                    ).fetchone()
                    is not None
                )
            if not pending_exists:
                break
            self.evaluate_next(release_id, refresh=refresh)
            processed += 1
        return self.get_status(release_id)

    # ------------------------------------------------------------------
    # Version checks and rollback completion
    # ------------------------------------------------------------------
    @staticmethod
    def _latest_versions(
        connection: sqlite3.Connection,
        release_id: str,
        plan: ReleasePlan,
        *,
        after: float | None = None,
    ) -> dict[str, dict[str, Any]]:
        candidates: dict[str, list[dict[str, Any]]] = {
            instance_id: [] for instance_id in plan.all_instances
        }
        report_sql = """
            SELECT instance_id, observed_at, parameter_version
            FROM version_reports
            WHERE release_id = ?
        """
        report_params: list[Any] = [release_id]
        if after is not None:
            report_sql += " AND observed_at > ?"
            report_params.append(after)
        for row in connection.execute(report_sql, report_params):
            if row["instance_id"] in candidates:
                candidates[row["instance_id"]].append(
                    {
                        "observed_at": float(row["observed_at"]),
                        "parameter_version": str(row["parameter_version"]),
                        "source": "version_report",
                    }
                )
        sample_sql = """
            SELECT instance_id, observed_at, parameter_version
            FROM samples
            WHERE release_id = ? AND parameter_version IS NOT NULL
        """
        sample_params: list[Any] = [release_id]
        if after is not None:
            sample_sql += " AND observed_at > ?"
            sample_params.append(after)
        for row in connection.execute(sample_sql, sample_params):
            if row["instance_id"] in candidates:
                candidates[row["instance_id"]].append(
                    {
                        "observed_at": float(row["observed_at"]),
                        "parameter_version": str(row["parameter_version"]),
                        "source": "metric_sample",
                    }
                )
        latest: dict[str, dict[str, Any]] = {}
        for instance_id, values in candidates.items():
            if values:
                latest[instance_id] = max(
                    values,
                    key=lambda item: (
                        item["observed_at"],
                        item["source"] == "version_report",
                    ),
                )
        return latest

    def verify_rollback(
        self, release_id: str, *, refresh: bool = True
    ) -> dict[str, Any]:
        """Read-only version verification; never writes parameter values."""

        if refresh:
            self.refresh_inputs(release_id)
        with self.db.transaction() as connection:
            release = self._fetch_release(connection, release_id)
            if release["status"] == "rolled_back":
                return self._status_from_connection(connection, release_id)
            if not release["rollback_required"]:
                raise BusinessError(
                    "ROLLBACK_NOT_REQUIRED",
                    "release does not require rollback",
                    details={
                        "release_id": release_id,
                        "status": release["status"],
                    },
                    status_code=409,
                )
            if release["status"] != "rollback_in_progress":
                raise BusinessError(
                    "ROLLBACK_NOT_ACTIVE",
                    "rollback verification is only available while rollback is in progress",
                    details={
                        "release_id": release_id,
                        "status": release["status"],
                    },
                    status_code=409,
                )
            plan = parse_plan(json.loads(release["plan_json"]))
            cutoff = (
                float(release["rollback_cutoff_at"])
                if release["rollback_cutoff_at"] is not None
                else 0.0
            )
            latest_after = self._latest_versions(
                connection, release_id, plan, after=cutoff
            )
            latest_any = self._latest_versions(connection, release_id, plan)
            ineffective: list[dict[str, Any]] = []
            for instance_id in plan.all_instances:
                current = latest_after.get(instance_id)
                if (
                    current is not None
                    and current["parameter_version"]
                    == plan.previous_stable_version
                ):
                    continue
                observed = current or latest_any.get(instance_id)
                if current is None:
                    reason = "NO_VERSION_REPORT_AFTER_ROLLBACK"
                else:
                    reason = "VERSION_MISMATCH"
                ineffective.append(
                    {
                        "instance_id": instance_id,
                        "observed_at": timestamp_to_iso(
                            observed["observed_at"] if observed else None
                        ),
                        "reported_version": (
                            observed["parameter_version"] if observed else None
                        ),
                        "reason": reason,
                    }
                )
            ineffective.sort(key=lambda item: item["instance_id"])
            checked_at = utc_now()
            completed = not ineffective
            new_status = "rolled_back" if completed else "rollback_in_progress"
            connection.execute(
                """
                UPDATE releases
                SET status = ?, rollback_ineffective_json = ?,
                    rollback_checked_at = ?, updated_at = ?
                WHERE release_id = ?
                """,
                (
                    new_status,
                    stable_json(ineffective),
                    checked_at,
                    checked_at,
                    release_id,
                ),
            )
            if completed:
                connection.execute(
                    "DELETE FROM instance_locks WHERE release_id = ?",
                    (release_id,),
                )
            self._insert_audit(
                connection,
                release_id,
                int(release["rollback_trigger_batch"])
                if release["rollback_trigger_batch"] is not None
                else None,
                "rollback_verification",
                "rolled_back" if completed else "incomplete",
                None if completed else "VERSION_NOT_EFFECTIVE",
                {
                    "type": "rollback_verification",
                    "release_id": release_id,
                    "target_version": plan.previous_stable_version,
                    "cutoff_observed_at": timestamp_to_iso(cutoff),
                    "checked_at": timestamp_to_iso(checked_at),
                    "effective_instance_count": len(plan.all_instances)
                    - len(ineffective),
                    "ineffective_instances": ineffective,
                    "completed": completed,
                    "write_parameter": False,
                },
            )
            return self._status_from_connection(connection, release_id)

    # ------------------------------------------------------------------
    # Read models
    # ------------------------------------------------------------------
    def _status_from_connection(
        self, connection: sqlite3.Connection, release_id: str
    ) -> dict[str, Any]:
        release = self._fetch_release(connection, release_id)
        plan = parse_plan(json.loads(release["plan_json"]))
        batches = self._fetch_batches(connection, release_id)
        batch_views = []
        released_instances: set[str] = set()
        threshold_instances: set[str] = set()
        conflict_release_ids: set[str] = set(
            json.loads(release["conflict_release_ids_json"] or "[]")
        )
        conflicts_by_key: dict[tuple[str, str, int], dict[str, Any]] = {}
        abort_count = 0
        decision_sequence = []

        for batch in batches:
            decision = _json_loads(batch["decision_json"], None)
            instance_ids = [
                str(item) for item in json.loads(batch["instances_json"])
            ]
            if batch["status"] == "passed":
                released_instances.update(instance_ids)
            if batch["status"] == "aborted":
                abort_count += 1
            threshold_instances.update(threshold_instance_ids(decision))
            if decision:
                for conflict in decision.get("conflicts", []):
                    release_conflict = str(conflict["release_id"])
                    conflict_release_ids.add(release_conflict)
                    key = (
                        str(conflict["instance_id"]),
                        release_conflict,
                        int(conflict["batch"]),
                    )
                    conflicts_by_key[key] = {
                        "instance_id": key[0],
                        "release_id": key[1],
                        "batch": key[2],
                    }
            batch_result = (
                "released"
                if batch["status"] == "passed"
                else "aborted"
                if batch["status"] == "aborted"
                else "not_released"
            )
            batch_views.append(
                {
                    "batch": int(batch["batch_no"]),
                    "status": batch["status"],
                    "result": batch_result,
                    "reason_code": batch["reason_code"],
                    "instances": sorted(instance_ids),
                    "observation_seconds": int(batch["observation_seconds"]),
                    "started_at": timestamp_to_iso(batch["started_at"]),
                    "completed_at": timestamp_to_iso(batch["completed_at"]),
                    "decision": decision,
                }
            )
            decision_sequence.append(
                {
                    "batch": int(batch["batch_no"]),
                    "result": batch_result,
                    "status": batch["status"],
                    "reason_code": batch["reason_code"],
                }
            )

        latest = self._latest_versions(connection, release_id, plan)
        version_distribution: dict[str, int] = {}
        instance_versions = []
        for instance_id in plan.all_instances:
            item = latest.get(instance_id)
            version = item["parameter_version"] if item else None
            if version is not None:
                version_distribution[version] = (
                    version_distribution.get(version, 0) + 1
                )
            instance_versions.append(
                {
                    "instance_id": instance_id,
                    "parameter_version": version,
                    "observed_at": timestamp_to_iso(
                        item["observed_at"] if item else None
                    ),
                }
            )

        trigger_instances = sorted(
            set(json.loads(release["rollback_trigger_instances_json"] or "[]"))
        )
        trigger_metrics = sorted(
            json.loads(release["rollback_trigger_metrics_json"] or "[]"),
            key=lambda item: (
                str(item.get("instance_id") or ""),
                str(item.get("metric") or ""),
            ),
        )
        ineffective = sorted(
            json.loads(release["rollback_ineffective_json"] or "[]"),
            key=lambda item: str(item.get("instance_id") or ""),
        )
        rollback = {
            "required": bool(release["rollback_required"]),
            "target_version": release["rollback_target_version"],
            "trigger_batch": release["rollback_trigger_batch"],
            "trigger_instances": trigger_instances,
            "trigger_metrics": trigger_metrics,
            "in_progress": release["status"] == "rollback_in_progress",
            "completed": release["status"] == "rolled_back",
            "started_at": timestamp_to_iso(release["rollback_started_at"]),
            "cutoff_observed_at": timestamp_to_iso(release["rollback_cutoff_at"]),
            "checked_at": timestamp_to_iso(release["rollback_checked_at"]),
            "ineffective_instances": ineffective,
            "ineffective_instance_count": len(ineffective),
        }
        status = str(release["status"])
        business_error = None
        if status in {"blocked", "rollback_in_progress"} and conflict_release_ids:
            business_error = {
                "code": "INSTANCE_OCCUPANCY_CONFLICT",
                "message": "instance is occupied by another unfinished release",
                "conflict_release_ids": sorted(conflict_release_ids),
            }
        return {
            "release_id": release["release_id"],
            "parameter_key": release["parameter_key"],
            "target_version": release["target_version"],
            "previous_stable_version": release["previous_stable_version"],
            "status": status,
            "outcome": {
                "completed": "completed",
                "rolled_back": "rolled_back",
                "rollback_in_progress": "aborted",
                "blocked": "blocked",
            }.get(status, "in_progress"),
            "current_batch": int(release["current_batch"]),
            "next_batch": int(release["next_batch"]),
            "total_batches": int(release["total_batches"]),
            "batch_count": int(release["total_batches"]),
            "batches": batch_views,
            "decision_sequence": decision_sequence,
            "released_instances": sorted(released_instances),
            "released_instance_count": len(released_instances),
            "threshold_instances": sorted(threshold_instances),
            "threshold_instance_count": len(threshold_instances),
            "abort_count": abort_count,
            "conflict_release_ids": sorted(conflict_release_ids),
            "conflicts": [
                conflicts_by_key[key] for key in sorted(conflicts_by_key)
            ],
            "business_error": business_error,
            "rollback": rollback,
            "rollback_target_version": release["rollback_target_version"],
            "version_distribution": {
                key: version_distribution[key]
                for key in sorted(version_distribution)
            },
            "instance_versions": instance_versions,
            "plan_sha256": release["plan_sha256"],
            "created_at": timestamp_to_iso(release["created_at"]),
            "updated_at": timestamp_to_iso(release["updated_at"]),
        }

    def get_status(self, release_id: str) -> dict[str, Any]:
        with self.db.reader() as connection:
            return self._status_from_connection(connection, release_id)

    def list_releases(self) -> list[dict[str, Any]]:
        with self.db.reader() as connection:
            rows = connection.execute(
                "SELECT release_id FROM releases ORDER BY release_id"
            ).fetchall()
            return [
                self._status_from_connection(connection, row["release_id"])
                for row in rows
            ]

    def get_audit(self, release_id: str) -> list[dict[str, Any]]:
        with self.db.reader() as connection:
            self._fetch_release(connection, release_id)
            rows = connection.execute(
                """
                SELECT * FROM decision_records
                WHERE release_id = ?
                ORDER BY id
                """,
                (release_id,),
            ).fetchall()
            return [
                {
                    "id": int(row["id"]),
                    "release_id": row["release_id"],
                    "batch": (
                        int(row["batch_no"]) if row["batch_no"] is not None else None
                    ),
                    "record_type": row["record_type"],
                    "result": row["result"],
                    "reason_code": row["reason_code"],
                    "record": _json_loads(row["record_json"], {}),
                    "created_at": timestamp_to_iso(row["created_at"]),
                }
                for row in rows
            ]

    def release_report(self, release_id: str) -> dict[str, Any]:
        status = self.get_status(release_id)
        batches = []
        for batch in status["batches"]:
            decision = batch["decision"] or {}
            violations = decision_violations(decision)
            trigger_instances = sorted(
                {
                    str(item["instance_id"])
                    for item in violations
                    if item.get("instance_id")
                }
                | {
                    str(item["instance_id"])
                    for item in decision.get("conflicts", [])
                    if item.get("instance_id")
                }
            )
            batches.append(
                {
                    "batch": batch["batch"],
                    "status": batch["status"],
                    "result": batch["result"],
                    "reason_code": batch["reason_code"],
                    "trigger_instances": trigger_instances,
                    "aggregate": decision.get("aggregate"),
                    "instances": decision.get("instances", []),
                    "violations": violations,
                    "conflicts": decision.get("conflicts", []),
                    "rollback_target_version": decision.get(
                        "rollback_target_version",
                        status["rollback_target_version"],
                    ),
                }
            )
        return {
            "release_id": status["release_id"],
            "status": status["status"],
            "outcome": status["outcome"],
            "current_batch": status["current_batch"],
            "batches": batches,
            "rollback_required": status["rollback"]["required"],
            "rollback_target_version": status["rollback_target_version"],
            "rollback_completed": status["rollback"]["completed"],
            "ineffective_instances": status["rollback"][
                "ineffective_instances"
            ],
            "ineffective_instance_count": status["rollback"][
                "ineffective_instance_count"
            ],
            "conflict_release_ids": status["conflict_release_ids"],
            "released_instance_count": status["released_instance_count"],
            "threshold_instance_count": status["threshold_instance_count"],
            "abort_count": status["abort_count"],
        }

    def recover(self) -> dict[str, int]:
        """Reopen existing state without changing terminal release outcomes."""

        with self.db.reader() as connection:
            rows = connection.execute(
                """
                SELECT status, COUNT(*) AS count
                FROM releases GROUP BY status ORDER BY status
                """
            ).fetchall()
        return {str(row["status"]): int(row["count"]) for row in rows}
