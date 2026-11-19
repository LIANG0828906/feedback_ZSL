"""SQLite persistence with atomic audit commits and immutable batches."""

from __future__ import annotations

import json
import sqlite3
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Iterator

from .constants import ErrorCode


class DuplicateAuditBatch(ValueError):
    code = ErrorCode.AUDIT_BATCH_EXISTS


class DuplicatePlan(ValueError):
    code = "PLAN_ID_EXISTS"


class RecordNotFound(KeyError):
    pass


def _json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


class Database:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._initialize()

    def _connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(str(self.path), timeout=5.0)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA foreign_keys = ON")
        connection.execute("PRAGMA busy_timeout = 5000")
        return connection

    @contextmanager
    def _connection(self) -> Iterator[sqlite3.Connection]:
        connection = self._connect()
        try:
            yield connection
        finally:
            connection.close()

    def _initialize(self) -> None:
        with self._connection() as connection:
            connection.execute("PRAGMA journal_mode = WAL")
            connection.executescript(
                """
                CREATE TABLE IF NOT EXISTS audits (
                    batch_id TEXT PRIMARY KEY,
                    started_at TEXT NOT NULL,
                    completed_at TEXT NOT NULL,
                    duration_ms REAL NOT NULL,
                    within_total_deadline INTEGER NOT NULL,
                    totals_json TEXT NOT NULL,
                    audit_json TEXT NOT NULL
                );

                CREATE TABLE IF NOT EXISTS audit_servers (
                    batch_id TEXT NOT NULL,
                    server_id TEXT NOT NULL,
                    redundancy_status TEXT NOT NULL,
                    is_redundant INTEGER NOT NULL,
                    declaration_match INTEGER NOT NULL,
                    result_json TEXT NOT NULL,
                    PRIMARY KEY (batch_id, server_id),
                    FOREIGN KEY (batch_id) REFERENCES audits(batch_id) ON DELETE CASCADE
                );

                CREATE TABLE IF NOT EXISTS audit_paths (
                    batch_id TEXT NOT NULL,
                    server_id TEXT NOT NULL,
                    path_id TEXT NOT NULL,
                    pdu_id TEXT,
                    port INTEGER,
                    ups_id TEXT,
                    topology_known INTEGER NOT NULL,
                    path_json TEXT NOT NULL,
                    PRIMARY KEY (batch_id, server_id, path_id),
                    FOREIGN KEY (batch_id, server_id)
                        REFERENCES audit_servers(batch_id, server_id) ON DELETE CASCADE
                );

                CREATE TABLE IF NOT EXISTS audit_devices (
                    batch_id TEXT NOT NULL,
                    device_id TEXT NOT NULL,
                    device_type TEXT NOT NULL,
                    online INTEGER NOT NULL,
                    topology_known INTEGER NOT NULL,
                    all_outage_count INTEGER NOT NULL,
                    one_path_loss_count INTEGER NOT NULL,
                    result_json TEXT NOT NULL,
                    PRIMARY KEY (batch_id, device_id),
                    FOREIGN KEY (batch_id) REFERENCES audits(batch_id) ON DELETE CASCADE
                );

                CREATE TABLE IF NOT EXISTS plans (
                    plan_id TEXT PRIMARY KEY,
                    batch_id TEXT NOT NULL,
                    plan_type TEXT NOT NULL,
                    target_type TEXT,
                    target_id TEXT,
                    executable INTEGER NOT NULL,
                    created_at TEXT NOT NULL,
                    summary_json TEXT NOT NULL,
                    plan_json TEXT NOT NULL,
                    FOREIGN KEY (batch_id) REFERENCES audits(batch_id) ON DELETE CASCADE
                );

                CREATE INDEX IF NOT EXISTS idx_audits_completed
                    ON audits(completed_at DESC);
                CREATE INDEX IF NOT EXISTS idx_servers_status
                    ON audit_servers(batch_id, redundancy_status);
                CREATE INDEX IF NOT EXISTS idx_plans_batch
                    ON plans(batch_id, created_at DESC);
                CREATE INDEX IF NOT EXISTS idx_plans_executable
                    ON plans(plan_type, executable);
                """
            )

    def audit_exists(self, batch_id: str) -> bool:
        with self._connection() as connection:
            row = connection.execute(
                "SELECT 1 FROM audits WHERE batch_id = ?", (batch_id,)
            ).fetchone()
            return row is not None

    def save_audit(self, audit: dict[str, Any]) -> None:
        batch_id = audit["batch_id"]
        with self._connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            try:
                if connection.execute(
                    "SELECT 1 FROM audits WHERE batch_id = ?", (batch_id,)
                ).fetchone():
                    raise DuplicateAuditBatch(
                        f"audit batch already exists: {batch_id}"
                    )
                connection.execute(
                    """
                    INSERT INTO audits (
                        batch_id, started_at, completed_at, duration_ms,
                        within_total_deadline, totals_json, audit_json
                    ) VALUES (?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        batch_id,
                        audit["started_at"],
                        audit["completed_at"],
                        float(audit["duration_ms"]),
                        int(bool(audit["within_total_deadline"])),
                        _json(audit["totals"]),
                        _json(audit),
                    ),
                )
                for server in audit["servers"]:
                    connection.execute(
                        """
                        INSERT INTO audit_servers (
                            batch_id, server_id, redundancy_status, is_redundant,
                            declaration_match, result_json
                        ) VALUES (?, ?, ?, ?, ?, ?)
                        """,
                        (
                            batch_id,
                            server["server_id"],
                            server["redundancy_status"],
                            int(bool(server["is_redundant"])),
                            int(bool(server["declaration_match"])),
                            _json(server),
                        ),
                    )
                    for path in server["power_paths"]:
                        connection.execute(
                            """
                            INSERT INTO audit_paths (
                                batch_id, server_id, path_id, pdu_id, port,
                                ups_id, topology_known, path_json
                            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                            """,
                            (
                                batch_id,
                                server["server_id"],
                                path["path_id"],
                                path.get("pdu_id"),
                                path.get("port"),
                                path.get("ups_id"),
                                int(bool(path["topology_known"])),
                                _json(path),
                            ),
                        )
                for device in audit["devices"]:
                    connection.execute(
                        """
                        INSERT INTO audit_devices (
                            batch_id, device_id, device_type, online,
                            topology_known, all_outage_count, one_path_loss_count,
                            result_json
                        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                        """,
                        (
                            batch_id,
                            device["device_id"],
                            device["device_type"],
                            int(bool(device["online"])),
                            int(bool(device["topology_known"])),
                            int(device["all_outage_count"]),
                            int(device["one_path_loss_count"]),
                            _json(device),
                        ),
                    )
                connection.commit()
            except sqlite3.IntegrityError as exc:
                connection.rollback()
                if "audits.batch_id" in str(exc) or "UNIQUE constraint failed: audits.batch_id" in str(exc):
                    raise DuplicateAuditBatch(
                        f"audit batch already exists: {batch_id}"
                    ) from exc
                raise
            except Exception:
                connection.rollback()
                raise

    def get_audit(self, batch_id: str) -> dict[str, Any]:
        with self._connection() as connection:
            row = connection.execute(
                "SELECT audit_json FROM audits WHERE batch_id = ?", (batch_id,)
            ).fetchone()
            if row is None:
                raise RecordNotFound(batch_id)
            return json.loads(row["audit_json"])

    def latest_audit(self) -> dict[str, Any] | None:
        with self._connection() as connection:
            row = connection.execute(
                """
                SELECT audit_json FROM audits
                ORDER BY completed_at DESC, batch_id DESC LIMIT 1
                """
            ).fetchone()
            return None if row is None else json.loads(row["audit_json"])

    def list_audits(self, limit: int = 50) -> list[dict[str, Any]]:
        safe_limit = max(1, min(int(limit), 500))
        with self._connection() as connection:
            rows = connection.execute(
                """
                SELECT batch_id, started_at, completed_at, duration_ms,
                       within_total_deadline, totals_json
                FROM audits
                ORDER BY completed_at DESC, batch_id DESC LIMIT ?
                """,
                (safe_limit,),
            ).fetchall()
        return [
            {
                "batch_id": row["batch_id"],
                "started_at": row["started_at"],
                "completed_at": row["completed_at"],
                "duration_ms": row["duration_ms"],
                "within_total_deadline": bool(row["within_total_deadline"]),
                "totals": json.loads(row["totals_json"]),
            }
            for row in rows
        ]

    def save_plan(self, plan: dict[str, Any]) -> None:
        from .planner import plan_summary

        target = plan.get("target") or {}
        with self._connection() as connection:
            try:
                connection.execute(
                    """
                    INSERT INTO plans (
                        plan_id, batch_id, plan_type, target_type, target_id,
                        executable, created_at, summary_json, plan_json
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        plan["plan_id"],
                        plan["batch_id"],
                        plan["plan_type"],
                        target.get("type"),
                        target.get("id"),
                        int(bool(plan["executable"])),
                        plan["created_at"],
                        _json(plan_summary(plan)),
                        _json(plan),
                    ),
                )
                connection.commit()
            except sqlite3.IntegrityError as exc:
                connection.rollback()
                if "plans.plan_id" in str(exc) or "UNIQUE constraint failed: plans.plan_id" in str(exc):
                    raise DuplicatePlan(f"plan already exists: {plan['plan_id']}") from exc
                raise

    def get_plan(self, plan_id: str) -> dict[str, Any]:
        with self._connection() as connection:
            row = connection.execute(
                "SELECT plan_json FROM plans WHERE plan_id = ?", (plan_id,)
            ).fetchone()
            if row is None:
                raise RecordNotFound(plan_id)
            return json.loads(row["plan_json"])

    def list_plans(
        self,
        *,
        batch_id: str | None = None,
        plan_type: str | None = None,
        limit: int = 50,
    ) -> list[dict[str, Any]]:
        safe_limit = max(1, min(int(limit), 500))
        clauses: list[str] = []
        params: list[Any] = []
        if batch_id is not None:
            clauses.append("batch_id = ?")
            params.append(batch_id)
        if plan_type is not None:
            clauses.append("plan_type = ?")
            params.append(plan_type)
        where = f"WHERE {' AND '.join(clauses)}" if clauses else ""
        params.append(safe_limit)
        with self._connection() as connection:
            rows = connection.execute(
                f"""
                SELECT summary_json FROM plans
                {where}
                ORDER BY created_at DESC, plan_id DESC LIMIT ?
                """,
                params,
            ).fetchall()
        return [json.loads(row["summary_json"]) for row in rows]

    def latest_plan_for_batch(self, batch_id: str) -> dict[str, Any] | None:
        plans = self.list_plans(batch_id=batch_id, limit=1)
        return plans[0] if plans else None

    def infeasible_plan_count(self) -> int:
        with self._connection() as connection:
            row = connection.execute(
                """
                SELECT COUNT(*) AS count FROM plans
                WHERE plan_type = 'maintenance' AND executable = 0
                """
            ).fetchone()
            return int(row["count"])

    def plan_count(self) -> int:
        with self._connection() as connection:
            row = connection.execute("SELECT COUNT(*) AS count FROM plans").fetchone()
            return int(row["count"])
