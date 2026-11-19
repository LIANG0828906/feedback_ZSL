from __future__ import annotations

import sqlite3
import threading
import uuid
from contextlib import contextmanager
from datetime import datetime, timedelta, timezone
from typing import Iterator

from .config import Config
from .faults import (
    DatabaseBusy,
    DatabaseFailure,
    DatabaseTimeout,
    DatabaseUnavailable,
    FaultController,
)
from .metrics import Metrics


class Database:
    def __init__(
        self,
        config: Config,
        metrics: Metrics,
        faults: FaultController,
    ) -> None:
        self.config = config
        self.metrics = metrics
        self.faults = faults
        self.path = config.database.path
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._slots = threading.BoundedSemaphore(config.database.connection_limit)
        self._active = 0
        self._active_lock = threading.Lock()
        if config.database.init:
            self.initialize()
        self.metrics.set(
            "order_guard_db_connection_limit",
            config.database.connection_limit,
        )

    def _connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(
            self.path,
            timeout=self.config.database.busy_timeout_ms / 1000.0,
            isolation_level=None,
        )
        connection.row_factory = sqlite3.Row
        connection.execute(
            f"PRAGMA busy_timeout={int(self.config.database.busy_timeout_ms)}"
        )
        return connection

    def initialize(self) -> None:
        connection = self._connect()
        try:
            connection.execute("PRAGMA journal_mode=WAL")
            connection.execute("PRAGMA synchronous=NORMAL")
            connection.execute(
                """
                CREATE TABLE IF NOT EXISTS orders (
                    order_id TEXT PRIMARY KEY,
                    tenant_id TEXT NOT NULL,
                    status TEXT NOT NULL,
                    amount_cents INTEGER NOT NULL,
                    created_at TEXT NOT NULL,
                    updated_at TEXT NOT NULL,
                    version INTEGER NOT NULL DEFAULT 1
                )
                """
            )
            connection.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_orders_tenant_status_created
                ON orders(tenant_id, status, created_at DESC, order_id ASC)
                """
            )
            self._seed(connection)
        finally:
            connection.close()

    def _seed(self, connection: sqlite3.Connection) -> None:
        base = datetime(2026, 1, 1, tzinfo=timezone.utc)
        rows: list[tuple[str, str, str, int, str, str, int]] = []
        for tenant_index, tenant in enumerate(self.config.tenants):
            for index in range(self.config.database.orders_per_tenant):
                status = self.config.database.statuses[
                    (tenant_index + index) % len(self.config.database.statuses)
                ]
                created = base + timedelta(seconds=index)
                amount = 1000 + (tenant_index * 997 + index * 37) % 90000
                rows.append(
                    (
                        f"ord-{tenant.tenant_id}-{index:06d}",
                        tenant.tenant_id,
                        status,
                        amount,
                        created.isoformat(timespec="seconds"),
                        created.isoformat(timespec="seconds"),
                        1,
                    )
                )
        connection.execute("BEGIN IMMEDIATE")
        try:
            connection.executemany(
                """
                INSERT OR IGNORE INTO orders (
                    order_id, tenant_id, status, amount_cents, created_at,
                    updated_at, version
                ) VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                rows,
            )
            connection.execute("COMMIT")
        except Exception:
            connection.execute("ROLLBACK")
            raise

    @contextmanager
    def operation(self) -> Iterator[sqlite3.Connection]:
        acquired = self._slots.acquire(
            timeout=max(0.01, self.config.database.operation_timeout_ms / 1000.0)
        )
        if not acquired:
            self.metrics.inc("order_guard_db_pool_timeout_total")
            raise DatabaseTimeout("timed out waiting for a SQLite connection slot")
        with self._active_lock:
            self._active += 1
            active = self._active
        self.metrics.set("order_guard_active_queries", active)
        connection: sqlite3.Connection | None = None
        self.metrics.inc("order_guard_sqlite_operations_total")
        try:
            self.faults.before_database_operation()
            connection = self._connect()
            yield connection
        except DatabaseBusy:
            self.metrics.inc("order_guard_sqlite_busy_events_total")
            raise
        except sqlite3.OperationalError as exc:
            if "locked" in str(exc).lower() or "busy" in str(exc).lower():
                self.metrics.inc("order_guard_sqlite_busy_events_total")
                raise DatabaseBusy(str(exc)) from exc
            raise DatabaseFailure(str(exc)) from exc
        finally:
            if connection is not None:
                connection.close()
            with self._active_lock:
                self._active = max(0, self._active - 1)
                active = self._active
            self.metrics.set("order_guard_active_queries", active)
            self._slots.release()

    @staticmethod
    def _row_to_dict(row: sqlite3.Row) -> dict[str, object]:
        return dict(row)

    def fetch_orders(
        self,
        *,
        tenant_id: str,
        status: str,
        page: int,
        page_size: int,
        slim: bool,
    ) -> list[dict[str, object]]:
        columns = (
            "order_id, status, created_at"
            if slim
            else "order_id, tenant_id, status, amount_cents, created_at, updated_at, version"
        )
        offset = (page - 1) * page_size
        with self.operation() as connection:
            rows = connection.execute(
                f"""
                SELECT {columns}
                FROM orders
                WHERE tenant_id = ? AND status = ?
                ORDER BY created_at DESC, order_id ASC
                LIMIT ? OFFSET ?
                """,
                (tenant_id, status, page_size, offset),
            ).fetchall()
        return [self._row_to_dict(row) for row in rows]

    def fetch_snapshot_rows(
        self, tenant_id: str, max_rows: int
    ) -> list[dict[str, object]]:
        with self.operation() as connection:
            rows = connection.execute(
                """
                SELECT order_id, tenant_id, status, amount_cents, created_at,
                       updated_at, version
                FROM orders
                WHERE tenant_id = ?
                ORDER BY created_at DESC, order_id ASC
                LIMIT ?
                """,
                (tenant_id, max_rows),
            ).fetchall()
        return [self._row_to_dict(row) for row in rows]

    def create_order(
        self,
        *,
        tenant_id: str,
        amount_cents: int,
        status: str = "created",
    ) -> dict[str, object]:
        order_id = f"ord-{tenant_id}-{uuid.uuid4().hex[:20]}"
        now = datetime.now(timezone.utc).isoformat(timespec="milliseconds")
        with self.operation() as connection:
            connection.execute("BEGIN IMMEDIATE")
            try:
                connection.execute(
                    """
                    INSERT INTO orders (
                        order_id, tenant_id, status, amount_cents, created_at,
                        updated_at, version
                    ) VALUES (?, ?, ?, ?, ?, ?, 1)
                    """,
                    (order_id, tenant_id, status, amount_cents, now, now),
                )
                connection.execute("COMMIT")
            except Exception:
                connection.execute("ROLLBACK")
                raise
        return {
            "order_id": order_id,
            "tenant_id": tenant_id,
            "status": status,
            "amount_cents": amount_cents,
            "created_at": now,
            "updated_at": now,
            "version": 1,
        }

    def update_status(
        self,
        *,
        tenant_id: str,
        order_id: str,
        new_status: str,
    ) -> dict[str, object] | None:
        now = datetime.now(timezone.utc).isoformat(timespec="milliseconds")
        with self.operation() as connection:
            connection.execute("BEGIN IMMEDIATE")
            try:
                cursor = connection.execute(
                    """
                    UPDATE orders
                    SET status = ?, updated_at = ?, version = version + 1
                    WHERE order_id = ? AND tenant_id = ?
                    """,
                    (new_status, now, order_id, tenant_id),
                )
                if cursor.rowcount == 0:
                    connection.execute("ROLLBACK")
                    return None
                row = connection.execute(
                    """
                    SELECT order_id, tenant_id, status, amount_cents, created_at,
                           updated_at, version
                    FROM orders
                    WHERE order_id = ? AND tenant_id = ?
                    """,
                    (order_id, tenant_id),
                ).fetchone()
                connection.execute("COMMIT")
            except Exception:
                connection.execute("ROLLBACK")
                raise
        return self._row_to_dict(row) if row is not None else None
