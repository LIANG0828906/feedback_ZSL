from __future__ import annotations

import logging
import queue
import threading
from typing import Any

import psycopg

from .audit import AuditStore, utc_now
from .config import Config
from .db import DatabaseSet

LOGGER = logging.getLogger(__name__)

ROW_TYPES = ("members", "orders")


class ValidationManager:
    def __init__(self, config: Config, databases: DatabaseSet, audit: AuditStore):
        self.config = config
        self.databases = databases
        self.audit = audit
        self._queue: queue.Queue[str | None] = queue.Queue()
        self._thread: threading.Thread | None = None
        self._stop = threading.Event()
        self._active_batch: str | None = None
        self._lock = threading.Lock()

    def start(self, scope: str = "all") -> str:
        if scope not in ("all", "members", "orders"):
            raise ValueError("scope must be all, members or orders")
        batch_id = self.audit.new_batch_id()
        self.audit.create_validation_batch(
            batch_id,
            scope,
            {"captured_at": utc_now(), "cursors": self.audit.cursors()},
        )
        self._queue.put(batch_id)
        return batch_id

    def start_worker(self) -> None:
        if self._thread and self._thread.is_alive():
            return
        self._stop.clear()
        for batch in self.audit.running_validation_batches():
            self._queue.put(batch["batch_id"])
        self._thread = threading.Thread(
            target=self._run, name="dualwrite-validation", daemon=True
        )
        self._thread.start()

    def stop(self, timeout: float = 5.0) -> None:
        self._stop.set()
        self._queue.put(None)
        if self._thread:
            self._thread.join(timeout=timeout)

    @property
    def active_batch(self) -> str | None:
        with self._lock:
            return self._active_batch

    def _run(self) -> None:
        while not self._stop.is_set():
            try:
                batch_id = self._queue.get(timeout=0.5)
            except queue.Empty:
                continue
            if batch_id is None:
                break
            batch = self.audit.validation_batch(batch_id)
            if not batch or batch["status"] == "completed":
                continue
            with self._lock:
                self._active_batch = batch_id
            try:
                self._process(batch)
            except Exception as exc:
                LOGGER.exception("validation batch %s failed", batch_id)
                self.audit.mark_validation_error(
                    batch_id, f"{type(exc).__name__}: {exc}"
                )
                if not self._stop.is_set():
                    self._queue.put(batch_id)
                    self._stop.wait(1.0)
            finally:
                with self._lock:
                    self._active_batch = None

    def _process(self, batch: dict[str, Any]) -> None:
        batch_id = batch["batch_id"]
        scope = batch["scope"]
        if batch["phase"] == "snapshot":
            self.audit.reset_validation_snapshot(batch_id)
            self._collect_snapshot(batch_id, scope)
            batch = self.audit.validation_batch(batch_id)
            assert batch is not None
        if batch["phase"] in ("snapshot_complete", "compare"):
            self._compare(batch)

    def _collect_snapshot(self, batch_id: str, scope: str) -> None:
        cursors = self.audit.cursors()
        east = None
        south = None
        try:
            east = self.databases.pool("east").acquire(
                self.config.limits.sync_acquire_timeout_ms
            )
            south = self.databases.pool("south").acquire(
                self.config.limits.sync_acquire_timeout_ms
            )
            east.execute("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY")
            south.execute("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY")
            snapshot_at = utc_now()
            watermarks: dict[str, Any] = {
                "captured_at": snapshot_at,
                "cursors": cursors,
                "sources": {},
            }
            for site, conn in (("east", east), ("south", south)):
                target = "south" if site == "east" else "east"
                cursor = cursors.get(f"{site}->{target}", 0)
                max_row = conn.execute(
                    "SELECT COALESCE(MAX(change_id), 0) AS max_id FROM sync_change"
                ).fetchone()
                pending = conn.execute(
                    """
                    SELECT table_name, row_id, MIN(change_id) AS min_change_id,
                           MAX(change_id) AS max_change_id, COUNT(*) AS change_count
                    FROM sync_change
                    WHERE change_id > %s
                    GROUP BY table_name, row_id
                    ORDER BY table_name, row_id
                    """,
                    (cursor,),
                ).fetchall()
                self.audit.add_pending_rows(
                    batch_id,
                    [
                        {
                            "row_type": row["table_name"],
                            "row_id": row["row_id"],
                            "source_site": site,
                            "min_change_id": row["min_change_id"],
                            "max_change_id": row["max_change_id"],
                            "change_count": row["change_count"],
                        }
                        for row in pending
                    ],
                )
                watermarks["sources"][site] = {
                    "max_change_id": int(max_row["max_id"]),
                    "cursor": int(cursor),
                    "pending_rows": len(pending),
                }
            self.audit.set_validation_watermarks(batch_id, watermarks)

            row_types = ROW_TYPES if scope == "all" else (scope,)
            for site, conn in (("east", east), ("south", south)):
                for row_type in row_types:
                    if row_type == "members":
                        sql = """
                            SELECT member_id AS row_id, points, row_version, updated_at
                            FROM members ORDER BY member_id
                        """
                    else:
                        sql = """
                            SELECT order_id AS row_id, status, row_version, updated_at
                            FROM orders ORDER BY order_id
                        """
                    with conn.cursor(name=f"snapshot_{batch_id}_{site}_{row_type}") as cur:
                        cur.itersize = self.config.validation.snapshot_chunk_rows
                        cur.execute(sql)
                        while True:
                            rows = cur.fetchmany(self.config.validation.snapshot_chunk_rows)
                            if not rows:
                                break
                            self.audit.add_snapshot_rows(
                                batch_id, site, row_type, [dict(row) for row in rows]
                            )
            east.commit()
            south.commit()
            self.audit.set_validation_phase(batch_id, "compare", snapshot_at)
        finally:
            for conn in (east, south):
                if conn is None:
                    continue
                try:
                    if conn.info.transaction_status != psycopg.pq.TransactionStatus.IDLE:
                        conn.rollback()
                finally:
                    self.databases.pool(
                        "east" if conn is east else "south"
                    ).release(conn)

    def _compare(self, batch: dict[str, Any]) -> None:
        batch_id = batch["batch_id"]
        scope = batch["scope"]
        row_types = ROW_TYPES if scope == "all" else (scope,)
        last_type = batch.get("last_row_type")
        last_id = int(batch.get("last_row_id") or 0)
        for row_type in row_types:
            if last_type and row_type < last_type:
                continue
            after_id = last_id if last_type == row_type else 0
            while True:
                page = self.audit.snapshot_page(
                    batch_id, row_type, after_id, 500
                )
                if not page:
                    break
                pending = self.audit.pending_for_rows(
                    batch_id, row_type, [entry["row_id"] for entry in page]
                )
                mismatches = []
                for entry in page:
                    row_id = int(entry["row_id"])
                    east_row = entry["sites"].get("east")
                    south_row = entry["sites"].get("south")
                    if east_row is None or south_row is None:
                        missing_site = "east" if east_row is None else "south"
                        present_site = "south" if missing_site == "east" else "east"
                        pending_detail = pending.get(row_id)
                        category = "unconverged" if pending_detail else "true_conflict"
                        mismatches.append(
                            {
                                "row_type": row_type,
                                "row_id": row_id,
                                "column_name": "__row__",
                                "east_value": east_row,
                                "south_value": south_row,
                                "category": category,
                                "reason": (
                                    "row missing on one snapshot and a source change is pending"
                                    if pending_detail
                                    else "row missing with no pending source change"
                                ),
                                "details": {
                                    "missing_site": missing_site,
                                    "present_site": present_site,
                                    "pending": pending_detail,
                                },
                            }
                        )
                        continue

                    columns = ("points",) if row_type == "members" else ("status",)
                    for column in columns:
                        if east_row.get(column) == south_row.get(column):
                            continue
                        pending_detail = pending.get(row_id)
                        category = (
                            "unconverged" if pending_detail else "true_conflict"
                        )
                        mismatches.append(
                            {
                                "row_type": row_type,
                                "row_id": row_id,
                                "column_name": column,
                                "east_value": east_row.get(column),
                                "south_value": south_row.get(column),
                                "category": category,
                                "reason": (
                                    "a source change for this row had not been applied "
                                    "when the snapshot was taken"
                                    if pending_detail
                                    else "no pending source change explains the difference"
                                ),
                                "details": {
                                    "east_row_version": east_row["row_version"],
                                    "south_row_version": south_row["row_version"],
                                    "east_updated_at": east_row.get("updated_at"),
                                    "south_updated_at": south_row.get("updated_at"),
                                    "pending": pending_detail,
                                },
                            }
                        )
                self.audit.add_mismatches(batch_id, mismatches)
                after_id = int(page[-1]["row_id"])
                self.audit.set_validation_checkpoint(batch_id, row_type, after_id)

        batch = self.audit.validation_batch(batch_id)
        assert batch is not None
        summary = self.audit.validation_summary(batch_id)
        summary.update(
            {
                "checked_rows": self.audit.snapshot_counts(batch_id),
                "snapshot_at": batch.get("snapshot_at"),
                "scope": scope,
                "watermarks": batch["watermarks"],
            }
        )
        self.audit.complete_validation(batch_id, summary)
