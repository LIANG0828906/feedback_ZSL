from __future__ import annotations

import json
import queue
import sqlite3
import threading
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable, Iterator


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace(
        "+00:00", "Z"
    )


def _json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


class Ledger:
    def __init__(self, path: Path):
        self.path = path
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._init_lock = threading.Lock()
        self._initialized = False
        self._pool: queue.LifoQueue[sqlite3.Connection] = queue.LifoQueue()
        self._pool_size = 4
        self.initialize()

    def _connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(
            self.path, timeout=10, isolation_level=None, check_same_thread=False
        )
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA busy_timeout=10000")
        connection.execute("PRAGMA foreign_keys=ON")
        connection.execute("PRAGMA synchronous=NORMAL")
        return connection

    @contextmanager
    def _write(self) -> Iterator[sqlite3.Connection]:
        connection = self._pool.get(timeout=10)
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
            self._pool.put(connection)

    @contextmanager
    def _read(self) -> Iterator[sqlite3.Connection]:
        connection = self._pool.get(timeout=10)
        try:
            connection.execute("BEGIN")
            yield connection
            connection.execute("COMMIT")
        except BaseException:
            try:
                connection.execute("ROLLBACK")
            except sqlite3.Error:
                pass
            raise
        finally:
            self._pool.put(connection)

    def initialize(self) -> None:
        with self._init_lock:
            if self._initialized:
                return
            connection = self._connect()
            try:
                connection.execute("PRAGMA journal_mode=WAL")
                connection.execute("PRAGMA synchronous=NORMAL")
                connection.executescript(
                    """
                    CREATE TABLE IF NOT EXISTS trace_events (
                        event_id INTEGER PRIMARY KEY AUTOINCREMENT,
                        order_id INTEGER,
                        tenant_id INTEGER,
                        entity_type TEXT NOT NULL,
                        entity_id TEXT NOT NULL,
                        event_type TEXT NOT NULL,
                        payload_json TEXT NOT NULL,
                        created_at TEXT NOT NULL
                    );
                    CREATE INDEX IF NOT EXISTS IX_trace_order
                        ON trace_events(order_id, event_id);

                    CREATE TABLE IF NOT EXISTS dual_writes (
                        write_id TEXT PRIMARY KEY,
                        order_id INTEGER NOT NULL,
                        tenant_id INTEGER NOT NULL,
                        revision INTEGER NOT NULL,
                        payload_hash TEXT NOT NULL,
                        status TEXT NOT NULL,
                        legacy_commit_site TEXT,
                        shard_commit_site TEXT,
                        legacy_before_json TEXT,
                        shard_before_json TEXT,
                        error_code TEXT,
                        error_message TEXT,
                        created_at TEXT NOT NULL,
                        updated_at TEXT NOT NULL
                    );
                    CREATE INDEX IF NOT EXISTS IX_dual_writes_order
                        ON dual_writes(order_id, created_at);
                    CREATE INDEX IF NOT EXISTS IX_dual_writes_status
                        ON dual_writes(status, updated_at);

                    CREATE TABLE IF NOT EXISTS migration_progress (
                        tenant_id INTEGER PRIMARY KEY,
                        shard_index INTEGER NOT NULL,
                        status TEXT NOT NULL,
                        last_order_id INTEGER NOT NULL DEFAULT 0,
                        last_commit_site TEXT,
                        generation INTEGER NOT NULL DEFAULT 1,
                        error_code TEXT,
                        error_message TEXT,
                        started_at TEXT NOT NULL,
                        updated_at TEXT NOT NULL,
                        completed_at TEXT
                    );
                    CREATE TABLE IF NOT EXISTS migration_batches (
                        batch_id INTEGER PRIMARY KEY AUTOINCREMENT,
                        tenant_id INTEGER NOT NULL,
                        from_order_id INTEGER NOT NULL,
                        through_order_id INTEGER NOT NULL,
                        last_order_id INTEGER NOT NULL,
                        commit_site TEXT NOT NULL,
                        order_count INTEGER NOT NULL,
                        created_at TEXT NOT NULL
                    );
                    CREATE INDEX IF NOT EXISTS IX_migration_batches_tenant
                        ON migration_batches(tenant_id, batch_id);

                    CREATE TABLE IF NOT EXISTS route_state (
                        tenant_id INTEGER PRIMARY KEY,
                        mode TEXT NOT NULL,
                        rank INTEGER NOT NULL,
                        generation INTEGER NOT NULL,
                        updated_at TEXT NOT NULL
                    );

                    CREATE TABLE IF NOT EXISTS check_runs (
                        run_id TEXT PRIMARY KEY,
                        scope_key TEXT NOT NULL,
                        scope_json TEXT NOT NULL,
                        status TEXT NOT NULL,
                        cursor_order_id INTEGER NOT NULL DEFAULT 0,
                        total_checked INTEGER NOT NULL DEFAULT 0,
                        mismatch_count INTEGER NOT NULL DEFAULT 0,
                        error_code TEXT,
                        error_message TEXT,
                        created_at TEXT NOT NULL,
                        updated_at TEXT NOT NULL,
                        completed_at TEXT
                    );
                    CREATE INDEX IF NOT EXISTS IX_check_runs_scope
                        ON check_runs(scope_key, status);
                    CREATE TABLE IF NOT EXISTS check_results (
                        run_id TEXT NOT NULL,
                        order_id INTEGER NOT NULL,
                        tenant_id INTEGER,
                        field_name TEXT NOT NULL,
                        legacy_value TEXT,
                        shard_value TEXT,
                        detail_json TEXT NOT NULL,
                        created_at TEXT NOT NULL,
                        PRIMARY KEY(run_id, order_id, field_name)
                    );
                    CREATE INDEX IF NOT EXISTS IX_check_results_order
                        ON check_results(order_id, created_at);

                    CREATE TABLE IF NOT EXISTS rollback_runs (
                        run_id TEXT PRIMARY KEY,
                        scope_json TEXT NOT NULL,
                        status TEXT NOT NULL,
                        pending_tenants_json TEXT NOT NULL,
                        completed_tenants_json TEXT NOT NULL,
                        failed_tenants_json TEXT NOT NULL,
                        error_code TEXT,
                        error_message TEXT,
                        created_at TEXT NOT NULL,
                        updated_at TEXT NOT NULL,
                        completed_at TEXT
                    );
                    CREATE TABLE IF NOT EXISTS rollback_progress (
                        run_id TEXT NOT NULL,
                        tenant_id INTEGER NOT NULL,
                        last_order_id INTEGER NOT NULL DEFAULT 0,
                        status TEXT NOT NULL,
                        updated_at TEXT NOT NULL,
                        PRIMARY KEY(run_id, tenant_id)
                    );
                    CREATE TABLE IF NOT EXISTS rollback_actions (
                        run_id TEXT NOT NULL,
                        tenant_id INTEGER NOT NULL,
                        order_id INTEGER NOT NULL,
                        action TEXT NOT NULL,
                        source TEXT NOT NULL,
                        detail_json TEXT NOT NULL,
                        created_at TEXT NOT NULL,
                        PRIMARY KEY(run_id, order_id)
                    );
                    CREATE INDEX IF NOT EXISTS IX_rollback_actions_tenant
                        ON rollback_actions(run_id, tenant_id, order_id);
                    """
                )
                self._initialized = True
            finally:
                connection.close()
            for _ in range(self._pool_size):
                self._pool.put(self._connect())

    def close(self) -> None:
        while True:
            try:
                connection = self._pool.get_nowait()
            except queue.Empty:
                break
            connection.close()

    @staticmethod
    def _event(
        connection: sqlite3.Connection,
        *,
        order_id: int | None,
        tenant_id: int | None,
        entity_type: str,
        entity_id: str,
        event_type: str,
        payload: dict[str, Any],
    ) -> None:
        connection.execute(
            """
            INSERT INTO trace_events
                (order_id, tenant_id, entity_type, entity_id, event_type,
                 payload_json, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?)
            """,
            (
                order_id,
                tenant_id,
                entity_type,
                str(entity_id),
                event_type,
                _json(payload),
                utc_now(),
            ),
        )

    def record_dual_intent(
        self,
        *,
        write_id: str,
        order_id: int,
        tenant_id: int,
        revision: int,
        payload_hash: str,
        legacy_before: dict[str, Any] | None,
        shard_before: dict[str, Any] | None,
    ) -> None:
        now = utc_now()
        with self._write() as connection:
            connection.execute(
                """
                INSERT INTO dual_writes
                    (write_id, order_id, tenant_id, revision, payload_hash,
                     status, legacy_before_json, shard_before_json,
                     created_at, updated_at)
                VALUES (?, ?, ?, ?, ?, 'PREPARING', ?, ?, ?, ?)
                """,
                (
                    write_id,
                    order_id,
                    tenant_id,
                    revision,
                    payload_hash,
                    _json(legacy_before) if legacy_before else None,
                    _json(shard_before) if shard_before else None,
                    now,
                    now,
                ),
            )
            self._event(
                connection,
                order_id=order_id,
                tenant_id=tenant_id,
                entity_type="dual_write",
                entity_id=write_id,
                event_type="DUAL_WRITE_PREPARING",
                payload={"revision": revision, "payloadHash": payload_hash},
            )

    def complete_dual(
        self,
        write_id: str,
        *,
        legacy_commit_site: str,
        shard_commit_site: str,
    ) -> None:
        now = utc_now()
        with self._write() as connection:
            row = connection.execute(
                "SELECT order_id, tenant_id FROM dual_writes WHERE write_id=?",
                (write_id,),
            ).fetchone()
            connection.execute(
                """
                UPDATE dual_writes
                SET status='COMMITTED', legacy_commit_site=?,
                    shard_commit_site=?, updated_at=?
                WHERE write_id=? AND status='PREPARING'
                """,
                (legacy_commit_site, shard_commit_site, now, write_id),
            )
            self._event(
                connection,
                order_id=row["order_id"] if row else None,
                tenant_id=row["tenant_id"] if row else None,
                entity_type="dual_write",
                entity_id=write_id,
                event_type="DUAL_WRITE_COMMITTED",
                payload={
                    "legacyCommitSite": legacy_commit_site,
                    "shardCommitSite": shard_commit_site,
                },
            )

    def fail_dual(
        self,
        write_id: str,
        *,
        code: str,
        message: str,
        details: dict[str, Any] | None = None,
    ) -> None:
        now = utc_now()
        with self._write() as connection:
            row = connection.execute(
                "SELECT order_id, tenant_id FROM dual_writes WHERE write_id=?",
                (write_id,),
            ).fetchone()
            connection.execute(
                """
                UPDATE dual_writes
                SET status='FAILED', error_code=?, error_message=?, updated_at=?
                WHERE write_id=?
                """,
                (code, message, now, write_id),
            )
            self._event(
                connection,
                order_id=row["order_id"] if row else None,
                tenant_id=row["tenant_id"] if row else None,
                entity_type="dual_write",
                entity_id=write_id,
                event_type="DUAL_WRITE_FAILED",
                payload={"errorCode": code, "message": message, **(details or {})},
            )

    def mark_dual_recovered(self, write_id: str, status: str, detail: str) -> None:
        now = utc_now()
        with self._write() as connection:
            row = connection.execute(
                "SELECT order_id, tenant_id FROM dual_writes WHERE write_id=?",
                (write_id,),
            ).fetchone()
            connection.execute(
                """
                UPDATE dual_writes SET status=?, updated_at=?, error_message=?
                WHERE write_id=?
                """,
                (status, now, detail, write_id),
            )
            self._event(
                connection,
                order_id=row["order_id"] if row else None,
                tenant_id=row["tenant_id"] if row else None,
                entity_type="dual_write",
                entity_id=write_id,
                event_type="DUAL_WRITE_RECOVERED",
                payload={"status": status, "detail": detail},
            )

    def dual_write(self, write_id: str) -> dict[str, Any] | None:
        with self._read() as connection:
            row = connection.execute(
                "SELECT * FROM dual_writes WHERE write_id=?", (write_id,)
            ).fetchone()
            return dict(row) if row else None

    def pending_dual_intents(self) -> list[dict[str, Any]]:
        with self._read() as connection:
            rows = connection.execute(
                "SELECT * FROM dual_writes WHERE status='PREPARING' ORDER BY created_at"
            ).fetchall()
            return [dict(row) for row in rows]

    def in_flight_order_ids(self) -> set[int]:
        with self._read() as connection:
            rows = connection.execute(
                "SELECT DISTINCT order_id FROM dual_writes WHERE status='PREPARING'"
            ).fetchall()
            return {int(row["order_id"]) for row in rows}

    def migration(self, tenant_id: int) -> dict[str, Any] | None:
        with self._read() as connection:
            row = connection.execute(
                "SELECT * FROM migration_progress WHERE tenant_id=?", (tenant_id,)
            ).fetchone()
            return dict(row) if row else None

    def migration_tenants(self) -> list[int]:
        with self._read() as connection:
            rows = connection.execute(
                "SELECT tenant_id FROM migration_progress ORDER BY tenant_id"
            ).fetchall()
            return [int(row["tenant_id"]) for row in rows]

    def begin_migration(self, tenant_id: int, shard: int) -> dict[str, Any]:
        now = utc_now()
        with self._write() as connection:
            row = connection.execute(
                "SELECT * FROM migration_progress WHERE tenant_id=?", (tenant_id,)
            ).fetchone()
            if row is None:
                connection.execute(
                    """
                    INSERT INTO migration_progress
                        (tenant_id, shard_index, status, last_order_id,
                         generation, started_at, updated_at)
                    VALUES (?, ?, 'IN_PROGRESS', 0, 1, ?, ?)
                    """,
                    (tenant_id, shard, now, now),
                )
            elif row["status"] != "COMPLETE":
                connection.execute(
                    """
                    UPDATE migration_progress
                    SET status='IN_PROGRESS', error_code=NULL,
                        error_message=NULL, updated_at=?
                    WHERE tenant_id=?
                    """,
                    (now, tenant_id),
                )
            result = connection.execute(
                "SELECT * FROM migration_progress WHERE tenant_id=?", (tenant_id,)
            ).fetchone()
            self._event(
                connection,
                order_id=None,
                tenant_id=tenant_id,
                entity_type="migration",
                entity_id=str(tenant_id),
                event_type="MIGRATION_START_OR_RESUME",
                payload={
                    "shard": shard,
                    "lastOrderId": result["last_order_id"],
                    "status": result["status"],
                },
            )
            return dict(result)

    def record_migration_batch(
        self,
        *,
        tenant_id: int,
        from_order_id: int,
        through_order_id: int,
        last_order_id: int,
        commit_site: str,
        order_count: int,
    ) -> None:
        now = utc_now()
        with self._write() as connection:
            connection.execute(
                """
                UPDATE migration_progress
                SET status='IN_PROGRESS', last_order_id=?, last_commit_site=?,
                    updated_at=?
                WHERE tenant_id=?
                """,
                (last_order_id, commit_site, now, tenant_id),
            )
            connection.execute(
                """
                INSERT INTO migration_batches
                    (tenant_id, from_order_id, through_order_id, last_order_id,
                     commit_site, order_count, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    tenant_id,
                    from_order_id,
                    through_order_id,
                    last_order_id,
                    commit_site,
                    order_count,
                    now,
                ),
            )
            self._event(
                connection,
                order_id=None,
                tenant_id=tenant_id,
                entity_type="migration",
                entity_id=str(tenant_id),
                event_type="MIGRATION_BATCH_COMMITTED",
                payload={
                    "fromOrderId": from_order_id,
                    "throughOrderId": through_order_id,
                    "lastOrderId": last_order_id,
                    "commitSite": commit_site,
                    "orderCount": order_count,
                },
            )

    def complete_migration(self, tenant_id: int, last_order_id: int,
                           commit_site: str) -> None:
        now = utc_now()
        with self._write() as connection:
            connection.execute(
                """
                UPDATE migration_progress
                SET status='COMPLETE', last_order_id=?,
                    last_commit_site=?, updated_at=?, completed_at=?
                WHERE tenant_id=?
                """,
                (last_order_id, commit_site, now, now, tenant_id),
            )
            self._event(
                connection,
                order_id=None,
                tenant_id=tenant_id,
                entity_type="migration",
                entity_id=str(tenant_id),
                event_type="MIGRATION_COMPLETE",
                payload={
                    "lastOrderId": last_order_id,
                    "commitSite": commit_site,
                },
            )

    def fail_migration(self, tenant_id: int, code: str, message: str) -> None:
        now = utc_now()
        with self._write() as connection:
            connection.execute(
                """
                UPDATE migration_progress
                SET status='IN_PROGRESS', error_code=?, error_message=?,
                    updated_at=?
                WHERE tenant_id=?
                """,
                (code, message, now, tenant_id),
            )
            self._event(
                connection,
                order_id=None,
                tenant_id=tenant_id,
                entity_type="migration",
                entity_id=str(tenant_id),
                event_type="MIGRATION_INTERRUPTED",
                payload={"errorCode": code, "message": message},
            )

    def route(self, tenant_id: int) -> dict[str, Any] | None:
        with self._read() as connection:
            row = connection.execute(
                "SELECT * FROM route_state WHERE tenant_id=?", (tenant_id,)
            ).fetchone()
            return dict(row) if row else None

    def advance_route(
        self,
        tenant_id: int,
        mode: str,
        rank: int,
        force: bool = False,
    ) -> dict[str, Any]:
        now = utc_now()
        with self._write() as connection:
            row = connection.execute(
                "SELECT * FROM route_state WHERE tenant_id=?", (tenant_id,)
            ).fetchone()
            if row is None or force or rank >= int(row["rank"]):
                generation = (int(row["generation"]) + 1) if row else 1
                connection.execute(
                    """
                    INSERT INTO route_state
                        (tenant_id, mode, rank, generation, updated_at)
                    VALUES (?, ?, ?, ?, ?)
                    ON CONFLICT(tenant_id) DO UPDATE SET
                        mode=excluded.mode,
                        rank=excluded.rank,
                        generation=excluded.generation,
                        updated_at=excluded.updated_at
                    """,
                    (tenant_id, mode, rank, generation, now),
                )
                self._event(
                    connection,
                    order_id=None,
                    tenant_id=tenant_id,
                    entity_type="route",
                    entity_id=str(tenant_id),
                    event_type="ROUTE_ADVANCED",
                    payload={
                        "mode": mode,
                        "rank": rank,
                        "generation": generation,
                        "forced": force,
                    },
                )
                row = connection.execute(
                    "SELECT * FROM route_state WHERE tenant_id=?", (tenant_id,)
                ).fetchone()
            return dict(row)

    def create_check_run(
        self,
        *,
        run_id: str,
        scope_key: str,
        scope: dict[str, Any],
        force: bool = False,
    ) -> tuple[dict[str, Any], bool]:
        now = utc_now()
        with self._write() as connection:
            if not force:
                existing = connection.execute(
                    """
                    SELECT * FROM check_runs
                    WHERE scope_key=? AND status='COMPLETE'
                    ORDER BY completed_at DESC LIMIT 1
                    """,
                    (scope_key,),
                ).fetchone()
                if existing:
                    return dict(existing), False
            connection.execute(
                """
                INSERT INTO check_runs
                    (run_id, scope_key, scope_json, status, created_at, updated_at)
                VALUES (?, ?, ?, 'IN_PROGRESS', ?, ?)
                """,
                (run_id, scope_key, _json(scope), now, now),
            )
            self._event(
                connection,
                order_id=None,
                tenant_id=scope.get("tenantId"),
                entity_type="check",
                entity_id=run_id,
                event_type="CHECK_STARTED",
                payload={"scope": scope, "scopeKey": scope_key},
            )
            row = connection.execute(
                "SELECT * FROM check_runs WHERE run_id=?", (run_id,)
            ).fetchone()
            return dict(row), True

    def check_run(self, run_id: str) -> dict[str, Any] | None:
        with self._read() as connection:
            row = connection.execute(
                "SELECT * FROM check_runs WHERE run_id=?", (run_id,)
            ).fetchone()
            return dict(row) if row else None

    def pending_check_runs(self) -> list[dict[str, Any]]:
        with self._read() as connection:
            rows = connection.execute(
                """
                SELECT * FROM check_runs
                WHERE status IN ('IN_PROGRESS', 'INTERRUPTED')
                ORDER BY created_at
                """
            ).fetchall()
            return [dict(row) for row in rows]

    def append_check_batch(
        self,
        *,
        run_id: str,
        cursor_order_id: int,
        total_checked: int,
        mismatches: Iterable[dict[str, Any]],
    ) -> None:
        now = utc_now()
        mismatch_list = list(mismatches)
        with self._write() as connection:
            for mismatch in mismatch_list:
                connection.execute(
                    """
                    INSERT OR IGNORE INTO check_results
                        (run_id, order_id, tenant_id, field_name,
                         legacy_value, shard_value, detail_json, created_at)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        run_id,
                        mismatch["orderId"],
                        mismatch.get("tenantId"),
                        mismatch["fieldName"],
                        str(mismatch.get("legacyValue")),
                        str(mismatch.get("shardValue")),
                        _json(mismatch),
                        now,
                    ),
                )
            connection.execute(
                """
                UPDATE check_runs
                SET cursor_order_id=?, total_checked=?, updated_at=?
                WHERE run_id=? AND status!='COMPLETE'
                """,
                (cursor_order_id, total_checked, now, run_id),
            )
            count = connection.execute(
                "SELECT COUNT(*) FROM check_results WHERE run_id=?", (run_id,)
            ).fetchone()[0]
            connection.execute(
                "UPDATE check_runs SET mismatch_count=? WHERE run_id=?",
                (count, run_id),
            )
            self._event(
                connection,
                order_id=None,
                tenant_id=None,
                entity_type="check",
                entity_id=run_id,
                event_type="CHECK_BATCH_COMMITTED",
                payload={
                    "cursorOrderId": cursor_order_id,
                    "totalChecked": total_checked,
                    "mismatchCount": count,
                    "newMismatches": len(mismatch_list),
                },
            )

    def complete_check(
        self, run_id: str, *, cursor_order_id: int, total_checked: int
    ) -> None:
        now = utc_now()
        with self._write() as connection:
            connection.execute(
                """
                UPDATE check_runs
                SET status='COMPLETE', cursor_order_id=?, total_checked=?,
                    updated_at=?, completed_at=?
                WHERE run_id=?
                """,
                (cursor_order_id, total_checked, now, now, run_id),
            )
            count = connection.execute(
                "SELECT COUNT(*) FROM check_results WHERE run_id=?", (run_id,)
            ).fetchone()[0]
            connection.execute(
                "UPDATE check_runs SET mismatch_count=? WHERE run_id=?",
                (count, run_id),
            )
            self._event(
                connection,
                order_id=None,
                tenant_id=None,
                entity_type="check",
                entity_id=run_id,
                event_type="CHECK_COMPLETE",
                payload={
                    "cursorOrderId": cursor_order_id,
                    "totalChecked": total_checked,
                    "mismatchCount": count,
                },
            )

    def fail_check(self, run_id: str, code: str, message: str) -> None:
        now = utc_now()
        with self._write() as connection:
            connection.execute(
                """
                UPDATE check_runs
                SET status='INTERRUPTED', error_code=?, error_message=?,
                    updated_at=?
                WHERE run_id=?
                """,
                (code, message, now, run_id),
            )
            self._event(
                connection,
                order_id=None,
                tenant_id=None,
                entity_type="check",
                entity_id=run_id,
                event_type="CHECK_INTERRUPTED",
                payload={"errorCode": code, "message": message},
            )

    def check_results(
        self, run_id: str, limit: int = 1000
    ) -> list[dict[str, Any]]:
        with self._read() as connection:
            rows = connection.execute(
                """
                SELECT * FROM check_results WHERE run_id=?
                ORDER BY order_id, field_name LIMIT ?
                """,
                (run_id, limit),
            ).fetchall()
            return [dict(row) for row in rows]

    def create_rollback_run(
        self, *, run_id: str, scope: dict[str, Any], pending: list[int]
    ) -> dict[str, Any]:
        now = utc_now()
        with self._write() as connection:
            connection.execute(
                """
                INSERT INTO rollback_runs
                    (run_id, scope_json, status, pending_tenants_json,
                     completed_tenants_json, failed_tenants_json,
                     created_at, updated_at)
                VALUES (?, ?, 'IN_PROGRESS', ?, '[]', '[]', ?, ?)
                """,
                (run_id, _json(scope), _json(pending), now, now),
            )
            for tenant in pending:
                connection.execute(
                    """
                    INSERT INTO rollback_progress
                        (run_id, tenant_id, last_order_id, status, updated_at)
                    VALUES (?, ?, 0, 'PENDING', ?)
                    """,
                    (run_id, tenant, now),
                )
            self._event(
                connection,
                order_id=None,
                tenant_id=None,
                entity_type="rollback",
                entity_id=run_id,
                event_type="ROLLBACK_STARTED",
                payload={"scope": scope, "pendingTenants": pending},
            )
            row = connection.execute(
                "SELECT * FROM rollback_runs WHERE run_id=?", (run_id,)
            ).fetchone()
            return dict(row)

    def rollback_run(self, run_id: str) -> dict[str, Any] | None:
        with self._read() as connection:
            row = connection.execute(
                "SELECT * FROM rollback_runs WHERE run_id=?", (run_id,)
            ).fetchone()
            return dict(row) if row else None

    def pending_rollback_runs(self) -> list[dict[str, Any]]:
        with self._read() as connection:
            rows = connection.execute(
                """
                SELECT * FROM rollback_runs
                WHERE status IN ('IN_PROGRESS', 'INTERRUPTED')
                ORDER BY created_at
                """
            ).fetchall()
            return [dict(row) for row in rows]

    def rollback_progress(self, run_id: str) -> list[dict[str, Any]]:
        with self._read() as connection:
            rows = connection.execute(
                """
                SELECT * FROM rollback_progress
                WHERE run_id=? ORDER BY tenant_id
                """,
                (run_id,),
            ).fetchall()
            return [dict(row) for row in rows]

    def set_rollback_tenant_status(
        self,
        run_id: str,
        tenant_id: int,
        status: str,
        *,
        last_order_id: int | None = None,
    ) -> None:
        now = utc_now()
        with self._write() as connection:
            if last_order_id is None:
                connection.execute(
                    """
                    UPDATE rollback_progress SET status=?, updated_at=?
                    WHERE run_id=? AND tenant_id=?
                    """,
                    (status, now, run_id, tenant_id),
                )
            else:
                connection.execute(
                    """
                    UPDATE rollback_progress
                    SET status=?, last_order_id=?, updated_at=?
                    WHERE run_id=? AND tenant_id=?
                    """,
                    (status, last_order_id, now, run_id, tenant_id),
                )
            self._event(
                connection,
                order_id=None,
                tenant_id=tenant_id,
                entity_type="rollback",
                entity_id=run_id,
                event_type=f"ROLLBACK_TENANT_{status}",
                payload={"lastOrderId": last_order_id},
            )

    def record_rollback_action(
        self,
        *,
        run_id: str,
        tenant_id: int,
        order_id: int,
        action: str,
        source: str,
        detail: dict[str, Any],
        cursor_order_id: int,
    ) -> None:
        now = utc_now()
        with self._write() as connection:
            connection.execute(
                """
                INSERT OR IGNORE INTO rollback_actions
                    (run_id, tenant_id, order_id, action, source,
                     detail_json, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                (run_id, tenant_id, order_id, action, source, _json(detail), now),
            )
            connection.execute(
                """
                UPDATE rollback_progress
                SET last_order_id=?, status='IN_PROGRESS', updated_at=?
                WHERE run_id=? AND tenant_id=?
                """,
                (cursor_order_id, now, run_id, tenant_id),
            )
            self._event(
                connection,
                order_id=order_id,
                tenant_id=tenant_id,
                entity_type="rollback",
                entity_id=run_id,
                event_type="ROLLBACK_ORDER",
                payload={"action": action, "source": source, **detail},
            )

    def record_rollback_batch(
        self,
        *,
        run_id: str,
        tenant_id: int,
        actions: Iterable[dict[str, Any]],
        cursor_order_id: int,
    ) -> None:
        now = utc_now()
        action_list = list(actions)
        with self._write() as connection:
            for action in action_list:
                order_id = int(action["orderId"])
                connection.execute(
                    """
                    INSERT OR IGNORE INTO rollback_actions
                        (run_id, tenant_id, order_id, action, source,
                         detail_json, created_at)
                    VALUES (?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        run_id,
                        tenant_id,
                        order_id,
                        action["action"],
                        action["source"],
                        _json(action.get("detail", {})),
                        now,
                    ),
                )
                self._event(
                    connection,
                    order_id=order_id,
                    tenant_id=tenant_id,
                    entity_type="rollback",
                    entity_id=run_id,
                    event_type="ROLLBACK_ORDER",
                    payload={
                        "action": action["action"],
                        "source": action["source"],
                        **action.get("detail", {}),
                    },
                )
            connection.execute(
                """
                UPDATE rollback_progress
                SET last_order_id=?, status='IN_PROGRESS', updated_at=?
                WHERE run_id=? AND tenant_id=?
                """,
                (cursor_order_id, now, run_id, tenant_id),
            )
            self._event(
                connection,
                order_id=None,
                tenant_id=tenant_id,
                entity_type="rollback",
                entity_id=run_id,
                event_type="ROLLBACK_BATCH_COMMITTED",
                payload={
                    "cursorOrderId": cursor_order_id,
                    "changedOrders": len(action_list),
                },
            )

    def finish_rollback_tenant(
        self, run_id: str, tenant_id: int, last_order_id: int
    ) -> None:
        now = utc_now()
        with self._write() as connection:
            run = connection.execute(
                "SELECT * FROM rollback_runs WHERE run_id=?", (run_id,)
            ).fetchone()
            completed = set(json.loads(run["completed_tenants_json"]))
            pending = [
                value
                for value in json.loads(run["pending_tenants_json"])
                if value != tenant_id
            ]
            completed.add(tenant_id)
            connection.execute(
                """
                UPDATE rollback_runs
                SET completed_tenants_json=?, pending_tenants_json=?,
                    updated_at=?
                WHERE run_id=?
                """,
                (_json(sorted(completed)), _json(pending), now, run_id),
            )
            connection.execute(
                """
                UPDATE rollback_progress
                SET status='COMPLETE', last_order_id=?, updated_at=?
                WHERE run_id=? AND tenant_id=?
                """,
                (last_order_id, now, run_id, tenant_id),
            )
            self._event(
                connection,
                order_id=None,
                tenant_id=tenant_id,
                entity_type="rollback",
                entity_id=run_id,
                event_type="ROLLBACK_TENANT_COMPLETE",
                payload={"lastOrderId": last_order_id},
            )

    def finish_rollback(
        self,
        run_id: str,
        *,
        status: str,
        code: str | None = None,
        message: str | None = None,
        failed_tenants: list[int] | None = None,
    ) -> None:
        now = utc_now()
        with self._write() as connection:
            connection.execute(
                """
                UPDATE rollback_runs
                SET status=?, error_code=?, error_message=?,
                    failed_tenants_json=?, updated_at=?, completed_at=?
                WHERE run_id=?
                """,
                (
                    status,
                    code,
                    message,
                    _json(failed_tenants or []),
                    now,
                    now if status == "COMPLETE" else None,
                    run_id,
                ),
            )
            self._event(
                connection,
                order_id=None,
                tenant_id=None,
                entity_type="rollback",
                entity_id=run_id,
                event_type=f"ROLLBACK_{status}",
                payload={
                    "errorCode": code,
                    "message": message,
                    "failedTenants": failed_tenants or [],
                },
            )

    def trace_order(self, order_id: int) -> dict[str, Any]:
        with self._read() as connection:
            dual_rows = connection.execute(
                """
                SELECT * FROM dual_writes
                WHERE order_id=? ORDER BY created_at
                """,
                (order_id,),
            ).fetchall()
            rollback_rows = connection.execute(
                """
                SELECT * FROM rollback_actions
                WHERE order_id=? ORDER BY created_at
                """,
                (order_id,),
            ).fetchall()
            migration_rows = connection.execute(
                """
                SELECT * FROM migration_batches
                WHERE from_order_id<=? AND through_order_id>=?
                ORDER BY batch_id
                """,
                (order_id, order_id),
            ).fetchall()
            check_rows = connection.execute(
                """
                SELECT r.run_id, r.status AS run_status, r.scope_json,
                       x.order_id, x.tenant_id, x.field_name,
                       x.legacy_value, x.shard_value, x.detail_json,
                       x.created_at
                FROM check_results x
                JOIN check_runs r ON r.run_id=x.run_id
                WHERE x.order_id=?
                ORDER BY x.created_at, x.field_name
                """,
                (order_id,),
            ).fetchall()
            tenant_ids = {
                int(row["tenant_id"])
                for row in dual_rows
                if row["tenant_id"] is not None
            }
            tenant_ids.update(
                int(row["tenant_id"])
                for row in rollback_rows
                if row["tenant_id"] is not None
            )
            tenant_ids.update(
                int(row["tenant_id"]) for row in migration_rows
            )
            events: list[sqlite3.Row] = []
            if tenant_ids:
                placeholders = ",".join("?" for _ in tenant_ids)
                events = connection.execute(
                    f"""
                    SELECT * FROM trace_events
                    WHERE order_id=?
                       OR (tenant_id IN ({placeholders})
                           AND entity_type IN ('migration', 'route', 'check'))
                    ORDER BY event_id
                    """,
                    [order_id, *sorted(tenant_ids)],
                ).fetchall()
            else:
                events = connection.execute(
                    """
                    SELECT * FROM trace_events
                    WHERE order_id=? ORDER BY event_id
                    """,
                    (order_id,),
                ).fetchall()
            migration_progress = []
            if tenant_ids:
                placeholders = ",".join("?" for _ in tenant_ids)
                migration_progress = connection.execute(
                    f"""
                    SELECT * FROM migration_progress
                    WHERE tenant_id IN ({placeholders})
                    ORDER BY tenant_id
                    """,
                    sorted(tenant_ids),
                ).fetchall()
            return {
                "orderId": order_id,
                "dualWrites": [dict(row) for row in dual_rows],
                "rollbackActions": [dict(row) for row in rollback_rows],
                "migrationBatches": [dict(row) for row in migration_rows],
                "migrationProgress": [
                    dict(row) for row in migration_progress
                ],
                "checkConclusions": [
                    {
                        **dict(row),
                        "detail": json.loads(row["detail_json"]),
                    }
                    for row in check_rows
                ],
                "events": [
                    {
                        **dict(row),
                        "payload": json.loads(row["payload_json"]),
                    }
                    for row in events
                ],
            }
