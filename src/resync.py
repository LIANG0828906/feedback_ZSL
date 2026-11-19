"""Consistent, resumable logical-subscription rebootstrap.

The important invariant is that the snapshot and the logical slot start at the
same table state:

* writes to the two business tables are briefly drained with SHARE locks;
* a logical slot is created in the same transaction;
* an exported snapshot is imported by a second connection before the exporter
  commits and releases the locks.

The snapshot is then copied in primary-key batches.  Each replica batch and its
target-side progress watermark are committed in one transaction, so a process
crash cannot redo a batch that was already committed.  On resume a new snapshot
is taken and a timestamp delta pass refreshes previously copied rows.  Finally,
a key anti-join removes rows deleted while the copy was in progress, the slot is
advanced to the new snapshot position, and the native subscription is attached
from exactly that point.
"""
from __future__ import annotations

import logging
import time
import uuid
from typing import Any

import psycopg
from psycopg import sql
from psycopg.conninfo import conninfo_to_dict, make_conninfo

from config import Config
from db import SlotGuardError, connect, lsn_int, table_identifier
from store import Store

log = logging.getLogger("slotguard.resync")

_MIN_BIGINT = -(2**63)


class ResyncManager:
    def __init__(self, cfg: Config, store: Store):
        self.cfg = cfg
        self.store = store

    # ---- connections / identifiers -------------------------------------
    def _primary(self, autocommit: bool = False):
        return connect(self.cfg.primary_dsn, self.cfg.connect_timeout_s, autocommit)

    def _replica(self, autocommit: bool = False):
        return connect(self.cfg.replica_dsn, self.cfg.connect_timeout_s, autocommit)

    def _table(self, name: str) -> sql.Composed:
        return table_identifier(name)

    def _meta_schema(self) -> sql.Identifier:
        return sql.Identifier(self.cfg.target_meta_schema)

    def _progress_table(self) -> sql.Composed:
        return sql.SQL("{}.{}").format(
            self._meta_schema(), sql.Identifier("resync_progress")
        )

    # ---- job state ------------------------------------------------------
    def active_job(self) -> dict[str, Any] | None:
        for row in self.store.list_resync_jobs():
            if row["status"] in ("running", "paused"):
                return dict(row)
        return None

    def start_job(self, reason: str) -> dict[str, Any]:
        existing = self.active_job()
        if existing:
            return existing
        now = time.time()
        job_id = f"rs-{time.strftime('%Y%m%d%H%M%S')}-{uuid.uuid4().hex[:8]}"
        self.store.upsert_resync_job(
            {
                "job_id": job_id,
                "status": "running",
                "stage": "prepare",
                "trigger_reason": reason,
                "snapshot_lsn": None,
                "previous_snapshot_lsn": None,
                "baseline_at": None,
                "created_at": now,
                "updated_at": now,
            }
        )
        for table in self.cfg.tables:
            self.store.upsert_table_progress(
                job_id, table, "pending", None, 0, now,
                phase="bulk", delta_last_key=None,
            )
        self.store.commit()
        return dict(self.store.get_resync_job(job_id))  # type: ignore[arg-type]

    def _update_job(self, job_id: str, **changes: Any) -> dict[str, Any]:
        row = self.store.get_resync_job(job_id)
        if row is None:
            raise ValueError(f"unknown resync job {job_id}")
        job = dict(row)
        job.update(changes)
        job["updated_at"] = time.time()
        self.store.upsert_resync_job(job)
        self.store.commit()
        return dict(self.store.get_resync_job(job_id))  # type: ignore[arg-type]

    # ---- preparation ----------------------------------------------------
    def ensure_publication(self) -> None:
        table_list = sql.SQL(", ").join(
            self._table(t) for t in self.cfg.tables
        )
        with self._primary(autocommit=True) as conn, conn.cursor() as cur:
            cur.execute(
                "SELECT 1 FROM pg_publication WHERE pubname=%s",
                (self.cfg.publication_name,),
            )
            if cur.fetchone() is None:
                cur.execute(
                    sql.SQL("CREATE PUBLICATION {} FOR TABLE {}").format(
                        sql.Identifier(self.cfg.publication_name), table_list
                    )
                )
            else:
                cur.execute(
                    sql.SQL("ALTER PUBLICATION {} SET TABLE {}").format(
                        sql.Identifier(self.cfg.publication_name), table_list
                    )
                )

    def ensure_disabled_subscription(self) -> None:
        with self._replica(autocommit=True) as conn, conn.cursor() as cur:
            cur.execute(
                "SELECT 1 FROM pg_subscription WHERE subname=%s",
                (self.cfg.subscription_name,),
            )
            if cur.fetchone() is not None:
                cur.execute(
                    sql.SQL("ALTER SUBSCRIPTION {} DISABLE").format(
                        sql.Identifier(self.cfg.subscription_name)
                    )
                )

    def _ensure_target_meta(self) -> None:
        with self._replica(autocommit=True) as conn, conn.cursor() as cur:
            cur.execute(
                sql.SQL("CREATE SCHEMA IF NOT EXISTS {}").format(
                    self._meta_schema()
                )
            )
            cur.execute(
                sql.SQL(
                    "CREATE TABLE IF NOT EXISTS {} ("
                    "job_id text NOT NULL, table_name text NOT NULL, "
                    "status text NOT NULL, phase text NOT NULL DEFAULT 'bulk', "
                    "bulk_last_key bigint, delta_last_key bigint, "
                    "rows_copied bigint NOT NULL DEFAULT 0, "
                    "updated_at timestamptz NOT NULL DEFAULT clock_timestamp(), "
                    "PRIMARY KEY(job_id, table_name))"
                ).format(self._progress_table())
            )

    def _open_consistent_snapshot(self):
        """Return (slot_created, snapshot_lsn, snapshot_at, importer)."""
        # The lock connection may wait for existing writers.  Its transaction
        # snapshot can therefore predate the moment the lock is granted.  Use a
        # separate creator connection that starts *after* the lock is held, so
        # its snapshot includes every transaction that committed before the
        # lock and no writer can commit table changes until locks are released.
        lock_conn = self._primary()
        creator = None
        importer = None
        try:
            lcur = lock_conn.cursor()
            lcur.execute("BEGIN")
            table_list = sql.SQL(", ").join(
                self._table(t) for t in self.cfg.tables
            )
            lcur.execute(
                sql.SQL("LOCK TABLE {} IN SHARE MODE").format(table_list)
            )

            creator = self._primary()
            ccur = creator.cursor()
            ccur.execute(
                "SELECT slot_name, plugin FROM pg_replication_slots "
                "WHERE slot_name=%s",
                (self.cfg.slot_name,),
            )
            row = ccur.fetchone()
            slot_created = False
            if row is None:
                ccur.execute(
                    "SELECT slot_name FROM "
                    "pg_create_logical_replication_slot(%s, 'pgoutput', false, false)",
                    (self.cfg.slot_name,),
                )
                slot_created = True
            elif row[1] != "pgoutput":
                raise SlotGuardError(
                    "SG-E3001", "primary", "pg_replication_slots",
                    f"slot {self.cfg.slot_name!r} uses plugin {row[1]!r}, "
                    "expected pgoutput",
                )

            # pg_current_wal_lsn() can be slightly behind the commit records
            # visible to a just-exported snapshot.  Emit a non-transactional
            # logical-message WAL record after the snapshot while table writes
            # are locked; its LSN is therefore a safe replay boundary that is
            # strictly after every row visible in the snapshot.
            ccur.execute(
                "SELECT pg_export_snapshot(), "
                "pg_logical_emit_message(false, 'slotguard', "
                "'resync-snapshot-boundary')::text, clock_timestamp()"
            )
            snapshot_id, snapshot_lsn, snapshot_at = ccur.fetchone()

            importer = self._primary()
            icur = importer.cursor()
            icur.execute("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY")
            icur.execute(
                sql.SQL("SET TRANSACTION SNAPSHOT {}").format(
                    sql.Literal(snapshot_id)
                )
            )
            creator.commit()
            lock_conn.commit()
            return slot_created, snapshot_lsn, snapshot_at, importer
        except Exception:
            lock_conn.rollback()
            lock_conn.close()
            if creator is not None:
                creator.rollback()
                creator.close()
            if importer is not None:
                importer.rollback()
                importer.close()
            raise

    # ---- copy -----------------------------------------------------------
    def _columns(self, importer: psycopg.Connection[Any], table: str) -> list[str]:
        with importer.cursor() as cur:
            cur.execute(
                "SELECT attname FROM pg_attribute "
                "WHERE attrelid=%s::regclass AND attnum>0 AND NOT attisdropped "
                "ORDER BY attnum",
                (table,),
            )
            return [row[0] for row in cur.fetchall()]

    def _column_type(
        self, importer: psycopg.Connection[Any], table: str, column: str
    ) -> str | None:
        with importer.cursor() as cur:
            cur.execute(
                "SELECT format_type(a.atttypid, a.atttypmod) "
                "FROM pg_attribute a WHERE a.attrelid=%s::regclass "
                "AND a.attname=%s AND a.attnum>0 AND NOT a.attisdropped",
                (table, column),
            )
            row = cur.fetchone()
            return row[0] if row else None

    def _target_progress(
        self, job_id: str, table: str
    ) -> dict[str, Any]:
        target: dict[str, Any] = {}
        with self._replica(autocommit=True) as conn, conn.cursor() as cur:
            cur.execute(
                sql.SQL(
                    "SELECT status, phase, bulk_last_key, delta_last_key, "
                    "rows_copied FROM {} WHERE job_id=%s AND table_name=%s"
                ).format(self._progress_table()),
                (job_id, table),
            )
            row = cur.fetchone()
            if row is not None:
                target = {
                    "status": row[0],
                    "phase": row[1],
                    "bulk_last_key": row[2],
                    "delta_last_key": row[3],
                    "rows_copied": int(row[4]),
                }
        local = self.store.get_table_progress(job_id, table)
        local_map = dict(local) if local is not None else {}
        bulk_values = [
            value for value in (
                local_map.get("last_key"), target.get("bulk_last_key")
            ) if value is not None
        ]
        delta_values = [
            value for value in (
                local_map.get("delta_last_key"), target.get("delta_last_key")
            ) if value is not None
        ]
        return {
            "status": (
                "done" if target.get("status") == "done"
                or local_map.get("status") == "done"
                else "running"
            ),
            "bulk_last_key": max(bulk_values) if bulk_values else _MIN_BIGINT,
            "delta_last_key": max(delta_values) if delta_values else _MIN_BIGINT,
            "rows_copied": max(
                int(local_map.get("rows_copied") or 0),
                int(target.get("rows_copied") or 0),
            ),
        }

    def _upsert_batch(
        self, rconn: psycopg.Connection[Any], job_id: str, table: str,
        columns: list[str], key: str, version: str | None,
        rows: list[tuple[Any, ...]], *, bulk_last: int, delta_last: int,
        rows_copied: int, phase: str, status: str,
    ) -> int:
        if rows:
            placeholders = ", ".join(
                "(" + ", ".join(["%s"] * len(columns)) + ")"
                for _ in rows
            )
            values = [value for row in rows for value in row]
            assignments = [
                sql.SQL("{} = EXCLUDED.{}").format(
                    sql.Identifier(col), sql.Identifier(col)
                )
                for col in columns if col != key
            ]
            if assignments:
                conflict = sql.SQL(
                    "ON CONFLICT ({}) DO UPDATE SET {}"
                ).format(
                    sql.Identifier(key),
                    sql.SQL(", ").join(assignments),
                )
                if version and version in columns:
                    conflict = sql.SQL(
                        "{} WHERE EXCLUDED.{} >= sg_target.{}"
                    ).format(
                        conflict,
                        sql.Identifier(version),
                        sql.Identifier(version),
                    )
            else:
                conflict = sql.SQL("ON CONFLICT ({}) DO NOTHING").format(
                    sql.Identifier(key)
                )
            stmt = sql.SQL(
                "INSERT INTO {} AS sg_target ({}) VALUES {} {}"
            ).format(
                self._table(table),
                sql.SQL(", ").join(sql.Identifier(c) for c in columns),
                sql.SQL(placeholders),
                conflict,
            )
            with rconn.cursor() as cur:
                cur.execute(stmt, values)

        with rconn.cursor() as cur:
            cur.execute(
                sql.SQL(
                    "INSERT INTO {} "
                    "(job_id, table_name, status, phase, bulk_last_key, "
                    "delta_last_key, rows_copied, updated_at) "
                    "VALUES (%s,%s,%s,%s,%s,%s,%s,clock_timestamp()) "
                    "ON CONFLICT (job_id, table_name) DO UPDATE SET "
                    "status=excluded.status, phase=excluded.phase, "
                    "bulk_last_key=excluded.bulk_last_key, "
                    "delta_last_key=excluded.delta_last_key, "
                    "rows_copied=GREATEST("
                    "{}.rows_copied, excluded.rows_copied), "
                    "updated_at=excluded.updated_at"
                ).format(self._progress_table(), self._progress_table()),
                (
                    job_id, table, status, phase,
                    bulk_last if bulk_last != _MIN_BIGINT else None,
                    delta_last if delta_last != _MIN_BIGINT else None,
                    rows_copied,
                ),
            )
        rconn.commit()
        return rows_copied

    def _copy_one_table(
        self, importer: psycopg.Connection[Any], job_id: str, table: str,
        key: str, version_column: str, baseline_at: float | None,
        resume: bool,
    ) -> dict[str, Any]:
        columns = self._columns(importer, table)
        if key not in columns:
            raise SlotGuardError(
                "SG-E3002", "primary", table,
                f"configured key column {key!r} is absent",
            )
        version = version_column if version_column in columns else None
        version_type = (
            self._column_type(importer, table, version)
            if version is not None else None
        )
        can_delta = bool(
            resume and baseline_at is not None and version is not None
            and version_type is not None
            and (
                version_type.startswith("timestamp")
                or version_type.startswith("date")
            )
        )
        progress = self._target_progress(job_id, table)
        last_key = progress["bulk_last_key"]
        delta_last = progress["delta_last_key"]
        rows_copied = progress["rows_copied"]
        if resume and not can_delta:
            # Without a timestamp version column there is no safe incremental
            # delta.  Re-scan from the beginning with idempotent UPSERTs rather
            # than silently missing updates made between two snapshots.
            last_key = _MIN_BIGINT
            delta_last = _MIN_BIGINT
        col_sql = sql.SQL(", ").join(sql.Identifier(c) for c in columns)
        table_sql = self._table(table)

        self.store.upsert_table_progress(
            job_id, table, "running", last_key if last_key != _MIN_BIGINT else None,
            rows_copied, time.time(), phase="delta" if can_delta else "bulk",
            delta_last_key=delta_last if delta_last != _MIN_BIGINT else None,
        )
        self.store.commit()

        with self._replica() as rconn:
            # A resumed job refreshes rows that changed since the original
            # cutoff and were in an already-copied key range.
            if can_delta:
                while True:
                    with importer.cursor() as cur:
                        cur.execute(
                            sql.SQL(
                                "SELECT {} FROM {} "
                                "WHERE {} > %s AND {} <= %s "
                                "AND {} >= to_timestamp(%s) "
                                "ORDER BY {} LIMIT %s"
                            ).format(
                                col_sql, table_sql,
                                sql.Identifier(key), sql.Identifier(key),
                                sql.Identifier(version), sql.Identifier(key),
                            ),
                            (
                                delta_last,
                                None if last_key == _MIN_BIGINT else last_key,
                                baseline_at,
                                self.cfg.resync_batch_size,
                            ),
                        )
                        batch = cur.fetchall()
                    if not batch:
                        break
                    delta_last = int(batch[-1][0])
                    rows_copied += len(batch)
                    self._upsert_batch(
                        rconn, job_id, table, columns, key, version, batch,
                        bulk_last=last_key, delta_last=delta_last,
                        rows_copied=rows_copied, phase="delta", status="running",
                    )
                    self.store.upsert_table_progress(
                        job_id, table, "running",
                        last_key if last_key != _MIN_BIGINT else None,
                        rows_copied, time.time(), phase="delta",
                        delta_last_key=delta_last,
                    )
                    self.store.commit()

            while True:
                with importer.cursor() as cur:
                    cur.execute(
                        sql.SQL(
                            "SELECT {} FROM {} WHERE {} > %s "
                            "ORDER BY {} LIMIT %s"
                        ).format(
                            col_sql, table_sql, sql.Identifier(key),
                            sql.Identifier(key),
                        ),
                        (last_key, self.cfg.resync_batch_size),
                    )
                    batch = cur.fetchall()
                if not batch:
                    break
                last_key = int(batch[-1][0])
                rows_copied += len(batch)
                self._upsert_batch(
                    rconn, job_id, table, columns, key, version, batch,
                    bulk_last=last_key, delta_last=delta_last,
                    rows_copied=rows_copied, phase="bulk", status="running",
                )
                self.store.upsert_table_progress(
                    job_id, table, "running",
                    last_key if last_key != _MIN_BIGINT else None,
                    rows_copied, time.time(), phase="bulk",
                    delta_last_key=(
                        delta_last if delta_last != _MIN_BIGINT else None
                    ),
                )
                self.store.commit()

            self._upsert_batch(
                rconn, job_id, table, columns, key, version, [],
                bulk_last=last_key, delta_last=delta_last,
                rows_copied=rows_copied, phase="done", status="done",
            )

        self.store.upsert_table_progress(
            job_id, table, "done",
            last_key if last_key != _MIN_BIGINT else None,
            rows_copied, time.time(), phase="done",
            delta_last_key=delta_last if delta_last != _MIN_BIGINT else None,
        )
        self.store.commit()
        return {
            "table": table,
            "status": "done",
            "last_key": None if last_key == _MIN_BIGINT else last_key,
            "rows_copied": rows_copied,
            "delta_used": can_delta,
        }

    def _truncate_target(self) -> None:
        with self._replica() as conn, conn.cursor() as cur:
            cur.execute(
                sql.SQL("TRUNCATE {}").format(
                    sql.SQL(", ").join(self._table(t) for t in self.cfg.tables)
                )
            )
            conn.commit()

    def _anti_join(self, importer: psycopg.Connection[Any]) -> dict[str, int]:
        """Remove replica rows that are absent at the consistent snapshot."""
        parent_tmp = "slotguard_parent_keys"
        child_tmp = "slotguard_child_keys"
        parent = self._table(self.cfg.parent_table)
        child = self._table(self.cfg.child_table)
        result: dict[str, int] = {}

        with self._replica() as conn, conn.cursor() as cur:
            cur.execute(
                sql.SQL(
                    "CREATE TEMP TABLE {} (key bigint PRIMARY KEY) ON COMMIT DROP"
                ).format(sql.Identifier(parent_tmp))
            )
            cur.execute(
                sql.SQL(
                    "CREATE TEMP TABLE {} (key bigint PRIMARY KEY) ON COMMIT DROP"
                ).format(sql.Identifier(child_tmp))
            )
            for table, temp, key in (
                (self.cfg.parent_table, parent_tmp, self.cfg.parent_key),
                (self.cfg.child_table, child_tmp, self.cfg.child_key),
            ):
                with importer.cursor() as pcur:
                    pcur.execute(
                        sql.SQL("SELECT {} FROM {} ORDER BY {}").format(
                            sql.Identifier(key), self._table(table),
                            sql.Identifier(key),
                        )
                    )
                    with cur.copy(
                        sql.SQL("COPY {} (key) FROM STDIN").format(
                            sql.Identifier(temp)
                        )
                    ) as copy:
                        while True:
                            rows = pcur.fetchmany(5000)
                            if not rows:
                                break
                            for row in rows:
                                copy.write_row((row[0],))

            cur.execute(
                sql.SQL(
                    "DELETE FROM {} d WHERE NOT EXISTS ("
                    "SELECT 1 FROM {} k WHERE k.key=d.{}) "
                    "OR NOT EXISTS (SELECT 1 FROM {} p WHERE p.{}=d.{})"
                ).format(
                    child,
                    sql.Identifier(child_tmp), sql.Identifier(self.cfg.child_key),
                    parent, sql.Identifier(self.cfg.parent_key),
                    sql.Identifier(self.cfg.child_fk),
                )
            )
            result["replica_child_deleted"] = cur.rowcount
            cur.execute(
                sql.SQL(
                    "DELETE FROM {} d WHERE NOT EXISTS ("
                    "SELECT 1 FROM {} k WHERE k.key=d.{})"
                ).format(
                    parent,
                    sql.Identifier(parent_tmp), sql.Identifier(self.cfg.parent_key),
                )
            )
            result["replica_parent_deleted"] = cur.rowcount
            conn.commit()
        return result

    # ---- subscription attach / catch-up --------------------------------
    def advance_slot(self, snapshot_lsn: str) -> str | None:
        with self._primary(autocommit=True) as conn, conn.cursor() as cur:
            cur.execute(
                "SELECT active, confirmed_flush_lsn::text "
                "FROM pg_replication_slots WHERE slot_name=%s",
                (self.cfg.slot_name,),
            )
            row = cur.fetchone()
            if row is None:
                raise SlotGuardError(
                    "SG-E3003", "primary", "pg_replication_slots",
                    f"slot {self.cfg.slot_name!r} disappeared during resync",
                )
            if row[0]:
                raise SlotGuardError(
                    "SG-E3004", "primary", "pg_replication_slots",
                    f"slot {self.cfg.slot_name!r} is still active",
                )
            confirmed = lsn_int(row[1])
            wanted = lsn_int(snapshot_lsn)
            if confirmed is not None and wanted is not None and confirmed >= wanted:
                return row[1]
            cur.execute(
                "SELECT end_lsn::text FROM "
                "pg_replication_slot_advance(%s, %s)",
                (self.cfg.slot_name, snapshot_lsn),
            )
            advanced = cur.fetchone()
            return advanced[0] if advanced else None

    def _sub_conninfo(self) -> str:
        data = conninfo_to_dict(self.cfg.primary_dsn)
        keep = (
            "host", "hostaddr", "port", "user", "dbname", "password",
            "sslmode", "sslrootcert", "sslcert", "sslkey", "connect_timeout",
        )
        selected = {key: data[key] for key in keep if key in data}
        selected.setdefault("connect_timeout", str(self.cfg.connect_timeout_s))
        return make_conninfo("", **selected)

    def attach_subscription(self, snapshot_lsn: str) -> None:
        self.ensure_disabled_subscription()
        conninfo = self._sub_conninfo()
        with self._replica(autocommit=True) as conn, conn.cursor() as cur:
            cur.execute(
                "SELECT oid::text FROM pg_subscription WHERE subname=%s",
                (self.cfg.subscription_name,),
            )
            row = cur.fetchone()
            if row is None:
                cur.execute(
                    sql.SQL(
                        "CREATE SUBSCRIPTION {} CONNECTION {} PUBLICATION {} "
                        "WITH (copy_data=false, create_slot=false, "
                        "slot_name={}, enabled=false)"
                    ).format(
                        sql.Identifier(self.cfg.subscription_name),
                        sql.Literal(conninfo),
                        sql.Identifier(self.cfg.publication_name),
                        sql.Literal(self.cfg.slot_name),
                    )
                )
                cur.execute(
                    "SELECT oid::text FROM pg_subscription WHERE subname=%s",
                    (self.cfg.subscription_name,),
                )
                row = cur.fetchone()
            sub_oid = row[0]
            cur.execute(
                sql.SQL(
                    "ALTER SUBSCRIPTION {} SET (slot_name={})"
                ).format(
                    sql.Identifier(self.cfg.subscription_name),
                    sql.Literal(self.cfg.slot_name),
                )
            )
            cur.execute(
                sql.SQL(
                    "ALTER SUBSCRIPTION {} SET PUBLICATION {} "
                    "WITH (refresh=false)"
                ).format(
                    sql.Identifier(self.cfg.subscription_name),
                    sql.Identifier(self.cfg.publication_name),
                )
            )

            origin_name = f"pg_{sub_oid}"
            cur.execute(
                "SELECT 1 FROM pg_replication_origin WHERE roname=%s",
                (origin_name,),
            )
            if cur.fetchone() is None:
                cur.execute(
                    "SELECT pg_replication_origin_create(%s)", (origin_name,)
                )
            cur.execute(
                "SELECT pg_replication_origin_advance(%s, %s)",
                (origin_name, snapshot_lsn),
            )
            cur.execute(
                sql.SQL("ALTER SUBSCRIPTION {} ENABLE").format(
                    sql.Identifier(self.cfg.subscription_name)
                )
            )

    def restore_foreign_key(self) -> None:
        parent = self._table(self.cfg.parent_table)
        child = self._table(self.cfg.child_table)
        with self._replica(autocommit=True) as conn, conn.cursor() as cur:
            cur.execute(
                "SELECT c.convalidated FROM pg_constraint c "
                "JOIN pg_class t ON t.oid=c.conrelid "
                "WHERE c.conname=%s AND t.oid=%s::regclass",
                (self.cfg.child_constraint_name, self.cfg.child_table),
            )
            row = cur.fetchone()
            if row is None:
                cur.execute(
                    sql.SQL(
                        "ALTER TABLE {} ADD CONSTRAINT {} FOREIGN KEY ({}) "
                        "REFERENCES {} ({}) NOT VALID"
                    ).format(
                        child,
                        sql.Identifier(self.cfg.child_constraint_name),
                        sql.Identifier(self.cfg.child_fk),
                        parent,
                        sql.Identifier(self.cfg.parent_key),
                    )
                )
                cur.execute(
                    sql.SQL("ALTER TABLE {} VALIDATE CONSTRAINT {}").format(
                        child, sql.Identifier(self.cfg.child_constraint_name)
                    )
                )
            elif not row[0]:
                cur.execute(
                    sql.SQL("ALTER TABLE {} VALIDATE CONSTRAINT {}").format(
                        child, sql.Identifier(self.cfg.child_constraint_name)
                    )
                )

    def verify(self) -> dict[str, Any]:
        out: dict[str, Any] = {}
        with self._primary(autocommit=True) as p, self._replica(
            autocommit=True
        ) as r:
            pc, rc = p.cursor(), r.cursor()
            for table, key in (
                (self.cfg.parent_table, self.cfg.parent_key),
                (self.cfg.child_table, self.cfg.child_key),
            ):
                pc.execute(
                    sql.SQL("SELECT count(*), max({}) FROM {}").format(
                        sql.Identifier(key), self._table(table)
                    )
                )
                rc.execute(
                    sql.SQL("SELECT count(*), max({}) FROM {}").format(
                        sql.Identifier(key), self._table(table)
                    )
                )
                p_row, r_row = pc.fetchone(), rc.fetchone()
                out[table] = {
                    "primary_count": p_row[0],
                    "replica_count": r_row[0],
                    "count_match": p_row[0] == r_row[0],
                    "primary_max_key": p_row[1],
                    "replica_max_key": r_row[1],
                }
            rc.execute(
                sql.SQL(
                    "SELECT count(*) FROM {} d WHERE NOT EXISTS ("
                    "SELECT 1 FROM {} o WHERE o.{}=d.{})"
                ).format(
                    self._table(self.cfg.child_table),
                    self._table(self.cfg.parent_table),
                    sql.Identifier(self.cfg.parent_key),
                    sql.Identifier(self.cfg.child_fk),
                )
            )
            out["orphan_details"] = rc.fetchone()[0]
            rc.execute(
                "SELECT r.remote_lsn::text FROM pg_replication_origin_status r "
                "JOIN pg_subscription s ON r.external_id='pg_'||s.oid::text "
                "WHERE s.subname=%s",
                (self.cfg.subscription_name,),
            )
            row = rc.fetchone()
            out["applied_lsn"] = row[0] if row else None
            rc.execute(
                "SELECT pid, received_lsn::text, latest_end_lsn::text "
                "FROM pg_stat_subscription WHERE subname=%s",
                (self.cfg.subscription_name,),
            )
            row = rc.fetchone()
            out["subscription_pid"] = row[0] if row else None
            out["received_lsn"] = row[1] if row else None
            out["latest_end_lsn"] = row[2] if row else None
        out["consistent"] = (
            all(
                value["count_match"]
                for value in out.values() if isinstance(value, dict)
            )
            and out["orphan_details"] == 0
        )
        return out

    def _current_primary_lsn(self) -> str:
        with self._primary(autocommit=True) as conn, conn.cursor() as cur:
            cur.execute("SELECT pg_current_wal_flush_lsn()::text")
            return cur.fetchone()[0]

    def wait_catchup(self, timeout_s: int | None = None) -> dict[str, Any]:
        timeout_s = timeout_s or self.cfg.resync_wait_timeout_s
        deadline = time.time() + timeout_s
        last: dict[str, Any] = {}
        last_error: str | None = None
        while time.time() < deadline:
            try:
                target_lsn = self._current_primary_lsn()
                last = self.verify()
                applied = lsn_int(last.get("applied_lsn"))
                received = lsn_int(last.get("received_lsn"))
                latest_end = lsn_int(last.get("latest_end_lsn"))
                wanted = lsn_int(target_lsn)
                if (
                    wanted is not None
                    and (
                        (applied is not None and applied >= wanted)
                        or (received is not None and received >= wanted)
                        or (latest_end is not None and latest_end >= wanted)
                    )
                    and last.get("consistent")
                ):
                    last["caught_up_lsn"] = target_lsn
                    return last
            except (psycopg.Error, SlotGuardError) as exc:
                last_error = str(exc)
                log.warning("resync catch-up check failed: %s", exc)
            time.sleep(0.5)
        last["timeout"] = True
        if last_error:
            last["last_error"] = last_error
        return last

    # ---- orchestration --------------------------------------------------
    def run(self, reason: str = "manual") -> dict[str, Any]:
        job = self.active_job() or self.start_job(reason)
        job_id = job["job_id"]
        stage = job.get("stage") or "prepare"

        if stage in ("prepare", "copy", "paused"):
            self.ensure_publication()
            self.ensure_disabled_subscription()
            self._ensure_target_meta()
            resume = job.get("baseline_at") is not None
            (
                _slot_created, snapshot_lsn, snapshot_at, importer
            ) = self._open_consistent_snapshot()
            try:
                if not resume and self.cfg.resync_truncate_on_new:
                    self._truncate_target()
                job = self._update_job(
                    job_id,
                    status="running",
                    stage="copy",
                    previous_snapshot_lsn=(
                        job.get("snapshot_lsn") if resume else None
                    ),
                    snapshot_lsn=snapshot_lsn,
                    baseline_at=(
                        job.get("baseline_at")
                        if resume else snapshot_at.timestamp()
                    ),
                )
                for table, key, version in (
                    (
                        self.cfg.parent_table, self.cfg.parent_key,
                        self.cfg.parent_version_column,
                    ),
                    (
                        self.cfg.child_table, self.cfg.child_key,
                        self.cfg.child_version_column,
                    ),
                ):
                    self._copy_one_table(
                        importer, job_id, table, key, version,
                        job.get("baseline_at"), resume,
                    )
                deleted = self._anti_join(importer)
                self.restore_foreign_key()
                self.advance_slot(snapshot_lsn)
                self._update_job(
                    job_id, status="running", stage="attach",
                    snapshot_lsn=snapshot_lsn,
                )
                log.info(
                    "resync copy complete job=%s snapshot=%s deleted=%s",
                    job_id, snapshot_lsn, deleted,
                )
            finally:
                importer.rollback()
                importer.close()
        else:
            snapshot_lsn = job["snapshot_lsn"]

        job = self._update_job(job_id, status="running", stage="attach")
        self.attach_subscription(job["snapshot_lsn"])
        self._update_job(job_id, status="running", stage="verify")

        result = self.wait_catchup()
        status = "done" if result.get("consistent") else "paused"
        self._update_job(job_id, status=status, stage="verify")
        return {
            "job_id": job_id,
            "status": status,
            "snapshot_lsn": job["snapshot_lsn"],
            "verify": result,
        }

    def progress(self) -> dict[str, Any]:
        job = self.active_job()
        latest = self.store.list_resync_jobs()
        chosen = job or (dict(latest[-1]) if latest else None)
        if not chosen:
            return {"job": None, "tables": []}
        return {
            "job": chosen,
            "tables": [
                dict(row)
                for row in self.store.list_table_progress(chosen["job_id"])
            ],
        }
