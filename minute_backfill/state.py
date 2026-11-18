"""Durable SQLite state for the minute-window backfill engine.

Design invariants:
* One input unit (history shard or refund batch) is one SQLite transaction.
* Window state, minute output, late statistics, watermark, and the consumed
  input marker become visible together or not at all.
* Once a minute is committed it is never updated.  Late events only touch the
  late-statistics tables.
"""

from __future__ import annotations

import json
import os
import sqlite3
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable, Iterator, Sequence

from .errors import EngineError, state_error
from .timeutil import MICROS_PER_MINUTE, minute_end_us, minute_start_us


STATE_SCHEMA_VERSION = 1


SCHEMA_SQL = """
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS dedup (
    side TEXT NOT NULL,
    payment_id TEXT NOT NULL,
    window_start INTEGER NOT NULL,
    event_time INTEGER NOT NULL,
    arrival_seq INTEGER NOT NULL,
    amount_minor INTEGER NOT NULL,
    currency TEXT NOT NULL,
    event_id TEXT NOT NULL,
    merchant_id TEXT NOT NULL,
    PRIMARY KEY(side, payment_id, window_start)
);
CREATE INDEX IF NOT EXISTS dedup_key_window ON dedup(payment_id, window_start);

CREATE TABLE IF NOT EXISTS key_windows (
    payment_id TEXT NOT NULL,
    window_start INTEGER NOT NULL,
    pay_count INTEGER NOT NULL DEFAULT 0,
    pay_amount_minor INTEGER NOT NULL DEFAULT 0,
    refund_count INTEGER NOT NULL DEFAULT 0,
    refund_amount_minor INTEGER NOT NULL DEFAULT 0,
    currency TEXT NOT NULL,
    closed INTEGER NOT NULL DEFAULT 0,
    close_reason TEXT,
    commit_seq INTEGER,
    PRIMARY KEY(payment_id, window_start)
);
CREATE INDEX IF NOT EXISTS key_windows_open ON key_windows(closed, window_start);
CREATE INDEX IF NOT EXISTS key_windows_minute ON key_windows(window_start);

CREATE TABLE IF NOT EXISTS minute_windows (
    window_start INTEGER PRIMARY KEY,
    pay_count INTEGER NOT NULL,
    pay_amount_minor INTEGER NOT NULL,
    refund_count INTEGER NOT NULL,
    refund_amount_minor INTEGER NOT NULL,
    net_minor INTEGER NOT NULL,
    currency TEXT NOT NULL,
    close_reason TEXT NOT NULL,
    commit_seq INTEGER NOT NULL,
    committed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS closed_minutes (
    window_start INTEGER PRIMARY KEY,
    close_reason TEXT NOT NULL,
    commit_seq INTEGER NOT NULL,
    watermark INTEGER,
    closed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS late_key_stats (
    payment_id TEXT NOT NULL,
    window_start INTEGER NOT NULL,
    late_count INTEGER NOT NULL,
    late_pay_count INTEGER NOT NULL,
    late_refund_count INTEGER NOT NULL,
    late_amount_minor INTEGER NOT NULL,
    first_late_event_time INTEGER,
    last_late_event_time INTEGER,
    PRIMARY KEY(payment_id, window_start)
);

CREATE TABLE IF NOT EXISTS late_stats (
    window_start INTEGER PRIMARY KEY,
    late_count INTEGER NOT NULL,
    late_pay_count INTEGER NOT NULL,
    late_refund_count INTEGER NOT NULL,
    late_amount_minor INTEGER NOT NULL,
    first_late_event_time INTEGER,
    last_late_event_time INTEGER
);

CREATE TABLE IF NOT EXISTS consumed_inputs (
    seq INTEGER PRIMARY KEY,
    input_id TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL,
    batch_index INTEGER,
    declared_event_time INTEGER,
    rows INTEGER NOT NULL,
    sha256 TEXT,
    watermark_after INTEGER,
    committed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS commits (
    seq INTEGER PRIMARY KEY,
    input_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    batch_index INTEGER,
    watermark INTEGER,
    dedup_dropped_total INTEGER NOT NULL,
    late_total INTEGER NOT NULL,
    closed_minute_count INTEGER NOT NULL,
    committed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS counters (
    name TEXT PRIMARY KEY,
    value INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    run_id INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    status TEXT NOT NULL,
    resumed_from_seq INTEGER NOT NULL,
    recovered INTEGER NOT NULL,
    error_code TEXT
);

CREATE TABLE IF NOT EXISTS recoveries (
    run_id INTEGER PRIMARY KEY,
    previous_commit_seq INTEGER NOT NULL,
    previous_input_id TEXT,
    watermark INTEGER,
    open_key_windows INTEGER NOT NULL,
    committed_minutes INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
"""


def utc_now_text() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


@dataclass
class IngestStats:
    affected: set[tuple[str, int]] = field(default_factory=set)
    dedup_dropped: int = 0
    dedup_dropped_payment: int = 0
    dedup_dropped_refund: int = 0
    late_count: int = 0
    late_pay_count: int = 0
    late_refund_count: int = 0
    late_amount_minor: int = 0
    late_windows: set[int] = field(default_factory=set)


@dataclass(frozen=True)
class RunContext:
    run_id: int
    recovered: bool
    previous_commit_seq: int
    previous_input_id: str | None
    watermark: int | None


@dataclass(frozen=True)
class ConsumedInput:
    seq: int
    input_id: str
    kind: str
    batch_index: int | None
    declared_event_time: int | None
    rows: int
    sha256: str | None
    watermark_after: int | None
    committed_at: str


class FileLock:
    """Cross-platform non-blocking single-writer lock."""

    def __init__(self, path: Path) -> None:
        self.path = path
        self._fh: Any = None

    def acquire(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        fh = self.path.open("a+b")
        try:
            fh.seek(0)
            if fh.read(1) == b"":
                fh.seek(0)
                fh.write(b"0")
                fh.flush()
            fh.seek(0)
            if os.name == "nt":
                import msvcrt

                msvcrt.locking(fh.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl

                fcntl.flock(fh.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            fh.seek(0)
            fh.truncate()
            fh.write(f"pid={os.getpid()}\n".encode("ascii"))
            fh.flush()
            self._fh = fh
        except (OSError, BlockingIOError) as exc:
            try:
                fh.close()
            except OSError:
                pass
            raise state_error(
                f"state is exclusively locked by another process: {self.path}"
            ) from exc

    def release(self) -> None:
        if self._fh is None:
            return
        try:
            self._fh.seek(0)
            if os.name == "nt":
                import msvcrt

                msvcrt.locking(self._fh.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                import fcntl

                fcntl.flock(self._fh.fileno(), fcntl.LOCK_UN)
        except OSError:
            pass
        finally:
            try:
                self._fh.close()
            except OSError:
                pass
            self._fh = None


class StateStore:
    def __init__(
        self,
        db_path: Path,
        *,
        lock_path: Path | None = None,
        reset: bool = False,
        busy_timeout_ms: int = 5_000,
    ) -> None:
        self.db_path = db_path.resolve()
        self.lock_path = (lock_path or Path(str(self.db_path) + ".lock")).resolve()
        self.poison_path = Path(str(self.db_path) + ".poisoned")
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = FileLock(self.lock_path)
        self._lock.acquire()
        if reset:
            self._remove_state_files()
        if self.poison_path.exists():
            self._lock.release()
            raise state_error(
                f"state was quarantined after corruption; inspect {self.poison_path} "
                "and pass --reset-state explicitly to start over"
            )
        self.conn: sqlite3.Connection | None = None
        self._in_transaction = False
        try:
            self.conn = sqlite3.connect(
                self.db_path,
                timeout=busy_timeout_ms / 1000,
                isolation_level=None,
                check_same_thread=False,
            )
            self.conn.row_factory = sqlite3.Row
            self._configure()
            self._check_and_initialize()
        except EngineError:
            self._close_connection()
            self._lock.release()
            raise
        except Exception as exc:
            self._quarantine(str(exc))
            self._close_connection()
            self._lock.release()
            raise state_error(f"state database cannot be opened: {exc}") from exc

    # ------------------------------------------------------------------
    # lifecycle
    # ------------------------------------------------------------------
    def _configure(self) -> None:
        assert self.conn is not None
        cur = self.conn
        cur.execute("PRAGMA journal_mode=WAL")
        cur.execute("PRAGMA synchronous=FULL")
        cur.execute("PRAGMA foreign_keys=ON")
        cur.execute("PRAGMA busy_timeout=5000")
        cur.execute("PRAGMA temp_store=FILE")
        cur.execute("PRAGMA cache_size=-32768")
        cur.execute("PRAGMA mmap_size=0")

    def _check_and_initialize(self) -> None:
        assert self.conn is not None
        try:
            rows = self.conn.execute("PRAGMA quick_check").fetchall()
        except sqlite3.DatabaseError as exc:
            self._quarantine(f"quick_check failed: {exc}")
            raise state_error(f"state database is corrupt: {exc}") from exc
        if not rows or any(str(row[0]).lower() != "ok" for row in rows):
            detail = "; ".join(str(row[0]) for row in rows)
            self._quarantine(f"quick_check reported: {detail}")
            raise state_error(f"state database failed integrity check: {detail}")
        try:
            has_meta = self.conn.execute(
                "SELECT name FROM sqlite_master WHERE type='table' AND name='meta'"
            ).fetchone()
            if has_meta:
                stored = self.conn.execute(
                    "SELECT value FROM meta WHERE key='schema_version'"
                ).fetchone()
                if stored is not None and int(stored[0]) != STATE_SCHEMA_VERSION:
                    self._quarantine(
                        f"schema version {stored[0]} != {STATE_SCHEMA_VERSION}"
                    )
                    raise state_error(
                        f"state schema version {stored[0]} is not supported"
                    )
            self.conn.executescript(SCHEMA_SQL)
            self.conn.execute(
                "INSERT INTO meta(key,value) VALUES('schema_version',?) "
                "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                (str(STATE_SCHEMA_VERSION),),
            )
        except EngineError:
            raise
        except sqlite3.DatabaseError as exc:
            self._quarantine(f"schema initialization failed: {exc}")
            raise state_error(f"state database is corrupt: {exc}") from exc

    def _remove_state_files(self) -> None:
        for path in (
            self.db_path,
            Path(str(self.db_path) + "-wal"),
            Path(str(self.db_path) + "-shm"),
            self.poison_path,
        ):
            try:
                path.unlink()
            except FileNotFoundError:
                pass

    def _close_connection(self) -> None:
        if self.conn is not None:
            try:
                if self._in_transaction:
                    self.conn.rollback()
            except sqlite3.Error:
                pass
            try:
                self.conn.close()
            except sqlite3.Error:
                pass
            self.conn = None
        self._in_transaction = False

    def _quarantine(self, reason: str) -> None:
        self._close_connection()
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
        moved: list[str] = []
        for suffix in ("", "-wal", "-shm"):
            source = Path(str(self.db_path) + suffix)
            if source.exists():
                target = Path(f"{source}.corrupt.{stamp}")
                try:
                    os.replace(source, target)
                    moved.append(target.name)
                except OSError:
                    pass
        payload = {
            "code": "E_STATE",
            "reason": reason,
            "quarantined_at": utc_now_text(),
            "moved_files": moved,
        }
        try:
            self.poison_path.write_text(
                json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
        except OSError:
            pass

    def quarantine(self, reason: str) -> None:
        self._quarantine(reason)

    def close(self) -> None:
        try:
            self._close_connection()
        finally:
            self._lock.release()

    def __enter__(self) -> "StateStore":
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        if exc is not None:
            self.rollback()
        self.close()

    # ------------------------------------------------------------------
    # transactions and metadata
    # ------------------------------------------------------------------
    def begin(self) -> None:
        if self.conn is None:
            raise state_error("state database is closed")
        if self._in_transaction:
            raise state_error("nested state transactions are not allowed")
        try:
            self.conn.execute("BEGIN IMMEDIATE")
            self._in_transaction = True
        except sqlite3.OperationalError as exc:
            raise state_error(f"state database is locked: {exc}") from exc
        except sqlite3.DatabaseError as exc:
            self._quarantine(f"BEGIN failed: {exc}")
            raise state_error(f"state database is corrupt: {exc}") from exc

    def commit(self) -> None:
        if self.conn is None or not self._in_transaction:
            raise state_error("no active state transaction to commit")
        try:
            self.conn.execute("COMMIT")
            self._in_transaction = False
        except sqlite3.DatabaseError as exc:
            self.rollback()
            self._quarantine(f"COMMIT failed: {exc}")
            raise state_error(f"state commit failed: {exc}") from exc

    def rollback(self) -> None:
        if self.conn is None or not self._in_transaction:
            return
        try:
            self.conn.execute("ROLLBACK")
        except sqlite3.Error:
            pass
        self._in_transaction = False

    @property
    def in_transaction(self) -> bool:
        return self._in_transaction

    def meta_get(self, key: str) -> str | None:
        assert self.conn is not None
        row = self.conn.execute("SELECT value FROM meta WHERE key=?", (key,)).fetchone()
        return None if row is None else str(row[0])

    def meta_set(self, key: str, value: str | int | None) -> None:
        assert self.conn is not None
        if value is None:
            self.conn.execute("DELETE FROM meta WHERE key=?", (key,))
        else:
            self.conn.execute(
                "INSERT INTO meta(key,value) VALUES(?,?) "
                "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                (key, str(value)),
            )

    def watermark(self) -> int | None:
        raw = self.meta_get("watermark_us")
        return None if raw is None else int(raw)

    def set_watermark(self, value: int | None) -> None:
        self.meta_set("watermark_us", value)

    def counter_get(self, name: str) -> int:
        assert self.conn is not None
        row = self.conn.execute("SELECT value FROM counters WHERE name=?", (name,)).fetchone()
        return 0 if row is None else int(row[0])

    def counter_add(self, name: str, delta: int) -> int:
        assert self.conn is not None
        self.conn.execute(
            "INSERT INTO counters(name,value) VALUES(?,?) "
            "ON CONFLICT(name) DO UPDATE SET value=counters.value+excluded.value",
            (name, delta),
        )
        return self.counter_get(name)

    # ------------------------------------------------------------------
    # input checkpoints
    # ------------------------------------------------------------------
    def consumed_inputs(self) -> list[ConsumedInput]:
        assert self.conn is not None
        rows = self.conn.execute(
            "SELECT seq,input_id,kind,batch_index,declared_event_time,rows,sha256,"
            "watermark_after,committed_at FROM consumed_inputs ORDER BY seq"
        ).fetchall()
        return [
            ConsumedInput(
                seq=int(row["seq"]),
                input_id=str(row["input_id"]),
                kind=str(row["kind"]),
                batch_index=row["batch_index"],
                declared_event_time=row["declared_event_time"],
                rows=int(row["rows"]),
                sha256=row["sha256"],
                watermark_after=row["watermark_after"],
                committed_at=str(row["committed_at"]),
            )
            for row in rows
        ]

    def consumed_input_ids(self) -> set[str]:
        return {item.input_id for item in self.consumed_inputs()}

    def last_commit(self) -> ConsumedInput | None:
        items = self.consumed_inputs()
        return items[-1] if items else None

    def next_commit_seq(self) -> int:
        assert self.conn is not None
        row = self.conn.execute(
            "SELECT COALESCE(MAX(seq),0)+1 FROM consumed_inputs"
        ).fetchone()
        return int(row[0])

    def record_consumed(
        self,
        *,
        seq: int,
        input_id: str,
        kind: str,
        batch_index: int | None,
        declared_event_time: int | None,
        rows: int,
        sha256: str | None,
        watermark_after: int | None,
        closed_minute_count: int,
    ) -> None:
        assert self.conn is not None
        now = utc_now_text()
        try:
            self.conn.execute(
                "INSERT INTO consumed_inputs("
                "seq,input_id,kind,batch_index,declared_event_time,rows,sha256,"
                "watermark_after,committed_at) VALUES(?,?,?,?,?,?,?,?,?)",
                (
                    seq,
                    input_id,
                    kind,
                    batch_index,
                    declared_event_time,
                    rows,
                    sha256,
                    watermark_after,
                    now,
                ),
            )
        except sqlite3.IntegrityError as exc:
            raise state_error(f"input {input_id!r} was already committed", subject=input_id) from exc
        self.conn.execute(
            "INSERT INTO commits("
            "seq,input_id,kind,batch_index,watermark,dedup_dropped_total,"
            "late_total,closed_minute_count,committed_at)"
            " VALUES(?,?,?,?,?,?,?,?,?)",
            (
                seq,
                input_id,
                kind,
                batch_index,
                watermark_after,
                self.counter_get("dedup_dropped"),
                self.counter_get("late_total"),
                closed_minute_count,
                now,
            ),
        )

    # ------------------------------------------------------------------
    # runs and recovery
    # ------------------------------------------------------------------
    def start_run(self) -> RunContext:
        assert self.conn is not None
        previous = self.last_commit()
        recovered = previous is not None
        watermark = self.watermark()
        open_windows = self.open_key_windows_count()
        committed_minutes = self.committed_minute_count()
        started = utc_now_text()
        cur = self.conn.execute(
            "INSERT INTO runs(started_at,status,resumed_from_seq,recovered) "
            "VALUES(?,?,?,?)",
            (started, "running", previous.seq if previous else 0, int(recovered)),
        )
        run_id = int(cur.lastrowid)
        if recovered:
            self.conn.execute(
                "INSERT INTO recoveries("
                "run_id,previous_commit_seq,previous_input_id,watermark,"
                "open_key_windows,committed_minutes,created_at)"
                " VALUES(?,?,?,?,?,?,?)",
                (
                    run_id,
                    previous.seq,
                    previous.input_id,
                    watermark,
                    open_windows,
                    committed_minutes,
                    started,
                ),
            )
            self.counter_add("recovery_count", 1)
            self.counter_add("recovery_watermark_sum", watermark or 0)
        self.meta_set("last_run_id", run_id)
        self.meta_set("last_run_recovered", int(recovered))
        return RunContext(
            run_id=run_id,
            recovered=recovered,
            previous_commit_seq=previous.seq if previous else 0,
            previous_input_id=previous.input_id if previous else None,
            watermark=watermark,
        )

    def finish_run(self, run_id: int, status: str, error_code: str | None = None) -> None:
        assert self.conn is not None
        self.conn.execute(
            "UPDATE runs SET finished_at=?,status=?,error_code=? WHERE run_id=?",
            (utc_now_text(), status, error_code, run_id),
        )

    def recovery_report(self) -> dict[str, Any]:
        assert self.conn is not None
        rows = self.conn.execute(
            "SELECT run_id,previous_commit_seq,previous_input_id,watermark,"
            "open_key_windows,committed_minutes,created_at "
            "FROM recoveries ORDER BY run_id"
        ).fetchall()
        checkpoints = [
            {
                "run_id": int(row["run_id"]),
                "resumed_from_commit_seq": int(row["previous_commit_seq"]),
                "resumed_from_input_id": row["previous_input_id"],
                "watermark": (
                    None if row["watermark"] is None
                    else int(row["watermark"])
                ),
                "open_key_windows": int(row["open_key_windows"]),
                "committed_minute_windows": int(row["committed_minutes"]),
                "created_at": str(row["created_at"]),
            }
            for row in rows
        ]
        return {
            "count": self.counter_get("recovery_count"),
            "checkpoints": checkpoints,
            "last_run_id": int(self.meta_get("last_run_id") or 0),
        }

    # ------------------------------------------------------------------
    # dedup / ingestion
    # ------------------------------------------------------------------
    def ingest(
        self,
        events: Sequence[dict[str, Any]],
        *,
        arrival_seq_start: int,
        watermark_before: int | None,
        apply_late: bool = True,
    ) -> IngestStats:
        assert self.conn is not None
        stats = IngestStats()
        for offset, event in enumerate(events):
            side = str(event["side"])
            payment_id = str(event["payment_id"])
            event_time = int(event["event_time"])
            window_start = minute_start_us(event_time)
            arrival_seq = arrival_seq_start + offset
            closed = (
                watermark_before is not None
                and minute_end_us(window_start) <= watermark_before
            )
            existing = self.conn.execute(
                "SELECT event_time,amount_minor,currency,event_id,merchant_id,arrival_seq "
                "FROM dedup WHERE side=? AND payment_id=? AND window_start=?",
                (side, payment_id, window_start),
            ).fetchone()
            if closed and apply_late:
                if existing is not None and event_time >= int(existing["event_time"]):
                    stats.dedup_dropped += 1
                    if side == "PAYMENT":
                        stats.dedup_dropped_payment += 1
                    else:
                        stats.dedup_dropped_refund += 1
                self._record_late(event, stats)
                continue
            if existing is None:
                self.conn.execute(
                    "INSERT INTO dedup("
                    "side,payment_id,window_start,event_time,arrival_seq,"
                    "amount_minor,currency,event_id,merchant_id)"
                    " VALUES(?,?,?,?,?,?,?,?,?)",
                    (
                        side,
                        payment_id,
                        window_start,
                        event_time,
                        arrival_seq,
                        int(event["amount_minor"]),
                        str(event["currency"]),
                        str(event["event_id"]),
                        str(event["merchant_id"]),
                    ),
                )
                stats.affected.add((payment_id, window_start))
                continue
            if event_time < int(existing["event_time"]):
                self.conn.execute(
                    "UPDATE dedup SET event_time=?,arrival_seq=?,amount_minor=?,"
                    "currency=?,event_id=?,merchant_id=? "
                    "WHERE side=? AND payment_id=? AND window_start=?",
                    (
                        event_time,
                        arrival_seq,
                        int(event["amount_minor"]),
                        str(event["currency"]),
                        str(event["event_id"]),
                        str(event["merchant_id"]),
                        side,
                        payment_id,
                        window_start,
                    ),
                )
                stats.affected.add((payment_id, window_start))
                stats.dedup_dropped += 1
                if side == "PAYMENT":
                    stats.dedup_dropped_payment += 1
                else:
                    stats.dedup_dropped_refund += 1
            else:
                # Equal event time keeps the first arrival; later time loses.
                stats.dedup_dropped += 1
                if side == "PAYMENT":
                    stats.dedup_dropped_payment += 1
                else:
                    stats.dedup_dropped_refund += 1
        return stats

    def _record_late(self, event: dict[str, Any], stats: IngestStats) -> None:
        assert self.conn is not None
        side = str(event["side"])
        payment_id = str(event["payment_id"])
        event_time = int(event["event_time"])
        window_start = minute_start_us(event_time)
        amount = int(event["amount_minor"])
        stats.late_count += 1
        stats.late_amount_minor += amount
        stats.late_windows.add(window_start)
        if side == "PAYMENT":
            stats.late_pay_count += 1
        else:
            stats.late_refund_count += 1
        self.conn.execute(
            "INSERT INTO late_key_stats("
            "payment_id,window_start,late_count,late_pay_count,late_refund_count,"
            "late_amount_minor,first_late_event_time,last_late_event_time)"
            " VALUES(?,?,1,?,?,?,?,?) "
            "ON CONFLICT(payment_id,window_start) DO UPDATE SET "
            "late_count=late_key_stats.late_count+1,"
            "late_pay_count=late_key_stats.late_pay_count+excluded.late_pay_count,"
            "late_refund_count=late_key_stats.late_refund_count+excluded.late_refund_count,"
            "late_amount_minor=late_key_stats.late_amount_minor+excluded.late_amount_minor,"
            "first_late_event_time=MIN(late_key_stats.first_late_event_time,"
            "excluded.first_late_event_time),"
            "last_late_event_time=MAX(late_key_stats.last_late_event_time,"
            "excluded.last_late_event_time)",
            (
                payment_id,
                window_start,
                1 if side == "PAYMENT" else 0,
                1 if side == "REFUND" else 0,
                amount,
                event_time,
                event_time,
            ),
        )
        self.conn.execute(
            "INSERT INTO late_stats("
            "window_start,late_count,late_pay_count,late_refund_count,"
            "late_amount_minor,first_late_event_time,last_late_event_time)"
            " VALUES(?,1,?,?,?,?,?) "
            "ON CONFLICT(window_start) DO UPDATE SET "
            "late_count=late_stats.late_count+1,"
            "late_pay_count=late_stats.late_pay_count+excluded.late_pay_count,"
            "late_refund_count=late_stats.late_refund_count+excluded.late_refund_count,"
            "late_amount_minor=late_stats.late_amount_minor+excluded.late_amount_minor,"
            "first_late_event_time=MIN(late_stats.first_late_event_time,"
            "excluded.first_late_event_time),"
            "last_late_event_time=MAX(late_stats.last_late_event_time,"
            "excluded.last_late_event_time)",
            (
                window_start,
                1 if side == "PAYMENT" else 0,
                1 if side == "REFUND" else 0,
                amount,
                event_time,
                event_time,
            ),
        )

    def finalize_ingest(self, stats: IngestStats) -> None:
        if stats.dedup_dropped:
            self.counter_add("dedup_dropped", stats.dedup_dropped)
            self.counter_add("dedup_dropped_payment", stats.dedup_dropped_payment)
            self.counter_add("dedup_dropped_refund", stats.dedup_dropped_refund)
        if stats.late_count:
            self.counter_add("late_total", stats.late_count)
            self.counter_add("late_pay_total", stats.late_pay_count)
            self.counter_add("late_refund_total", stats.late_refund_count)
            self.counter_add("late_amount_minor", stats.late_amount_minor)

    # ------------------------------------------------------------------
    # group loading and window aggregation
    # ------------------------------------------------------------------
    def load_groups(
        self,
        keys: Iterable[tuple[str, int]],
        *,
        chunk_keys: int = 2_000,
    ) -> Iterator[list[dict[str, Any]]]:
        assert self.conn is not None
        batch: list[tuple[str, int]] = []

        def flush() -> Iterator[list[dict[str, Any]]]:
            if not batch:
                return
            values = ", ".join(["(?,?)"] * len(batch))
            params: list[Any] = []
            for payment_id, window_start in batch:
                params.extend([payment_id, window_start])
            rows = self.conn.execute(
                "SELECT side,payment_id,window_start,event_time,amount_minor,currency "
                f"FROM dedup WHERE (payment_id,window_start) IN ({values}) "
                "ORDER BY payment_id,window_start,side",
                params,
            ).fetchall()
            groups: list[dict[str, Any]] = []
            current: dict[str, Any] | None = None
            for row in rows:
                key = (str(row["payment_id"]), int(row["window_start"]))
                if current is None or current["key"] != key:
                    current = {
                        "key": key,
                        "payment_id": key[0],
                        "window_start": key[1],
                        "events": [],
                    }
                    groups.append(current)
                current["events"].append(
                    {
                        "side": str(row["side"]),
                        "event_time": int(row["event_time"]),
                        "amount_minor": int(row["amount_minor"]),
                        "currency": str(row["currency"]),
                    }
                )
            if groups:
                yield groups

        for key in keys:
            batch.append(key)
            if len(batch) >= chunk_keys:
                yield from flush()
                batch.clear()
        if batch:
            yield from flush()

    def apply_key_windows(self, rows: Iterable[dict[str, Any]]) -> int:
        assert self.conn is not None
        count = 0
        for row in rows:
            payment_id = str(row["payment_id"])
            window_start = int(row["window_start"])
            existing = self.conn.execute(
                "SELECT closed FROM key_windows WHERE payment_id=? AND window_start=?",
                (payment_id, window_start),
            ).fetchone()
            if existing is not None and int(existing["closed"]) == 1:
                raise state_error(
                    f"attempted to update committed key window "
                    f"{payment_id!r}/{window_start}",
                    subject=payment_id,
                )
            self.conn.execute(
                "INSERT INTO key_windows("
                "payment_id,window_start,pay_count,pay_amount_minor,refund_count,"
                "refund_amount_minor,currency,closed)"
                " VALUES(?,?,?,?,?,?,?,0) "
                "ON CONFLICT(payment_id,window_start) DO UPDATE SET "
                "pay_count=excluded.pay_count,"
                "pay_amount_minor=excluded.pay_amount_minor,"
                "refund_count=excluded.refund_count,"
                "refund_amount_minor=excluded.refund_amount_minor,"
                "currency=excluded.currency "
                "WHERE key_windows.closed=0",
                (
                    payment_id,
                    window_start,
                    int(row["pay_count"]),
                    int(row["pay_amount_minor"]),
                    int(row["refund_count"]),
                    int(row["refund_amount_minor"]),
                    str(row["currency"]),
                ),
            )
            count += 1
        return count

    def close_windows(
        self,
        *,
        watermark: int | None,
        commit_seq: int,
        reason: str,
    ) -> int:
        assert self.conn is not None
        if watermark is None:
            rows = self.conn.execute(
                "SELECT DISTINCT window_start FROM key_windows WHERE closed=0 "
                "ORDER BY window_start"
            ).fetchall()
        else:
            rows = self.conn.execute(
                "SELECT DISTINCT window_start FROM key_windows "
                "WHERE closed=0 AND window_start + ? <= ? ORDER BY window_start",
                (MICROS_PER_MINUTE, watermark),
            ).fetchall()
        closed = 0
        now = utc_now_text()
        for row in rows:
            window_start = int(row["window_start"])
            existing = self.conn.execute(
                "SELECT 1 FROM closed_minutes WHERE window_start=?", (window_start,)
            ).fetchone()
            if existing is None:
                aggregate = self.conn.execute(
                    "SELECT COALESCE(SUM(pay_count),0) pay_count,"
                    "COALESCE(SUM(pay_amount_minor),0) pay_amount,"
                    "COALESCE(SUM(refund_count),0) refund_count,"
                    "COALESCE(SUM(refund_amount_minor),0) refund_amount,"
                    "COUNT(DISTINCT currency) currency_count,"
                    "MIN(currency) currency "
                    "FROM key_windows WHERE window_start=? AND pay_count>0",
                    (window_start,),
                ).fetchone()
                pay_count = int(aggregate["pay_count"])
                if pay_count > 0:
                    pay_amount = int(aggregate["pay_amount"])
                    refund_count = int(aggregate["refund_count"])
                    refund_amount = int(aggregate["refund_amount"])
                    currency = (
                        str(aggregate["currency"])
                        if int(aggregate["currency_count"]) == 1
                        else "MIXED"
                    )
                    self.conn.execute(
                        "INSERT INTO minute_windows("
                        "window_start,pay_count,pay_amount_minor,refund_count,"
                        "refund_amount_minor,net_minor,currency,close_reason,"
                        "commit_seq,committed_at) VALUES(?,?,?,?,?,?,?,?,?,?) "
                        "ON CONFLICT(window_start) DO NOTHING",
                        (
                            window_start,
                            pay_count,
                            pay_amount,
                            refund_count,
                            refund_amount,
                            pay_amount - refund_amount,
                            currency,
                            reason,
                            commit_seq,
                            now,
                        ),
                    )
            self.conn.execute(
                "UPDATE key_windows SET closed=1,close_reason=?,commit_seq=? "
                "WHERE window_start=? AND closed=0",
                (reason, commit_seq, window_start),
            )
            if existing is None:
                self.conn.execute(
                    "INSERT INTO closed_minutes("
                    "window_start,close_reason,commit_seq,watermark,closed_at)"
                    " VALUES(?,?,?,?,?)",
                    (window_start, reason, commit_seq, watermark, now),
                )
            closed += 1
        return closed

    # ------------------------------------------------------------------
    # reporting queries
    # ------------------------------------------------------------------
    def open_key_windows_count(self) -> int:
        assert self.conn is not None
        row = self.conn.execute(
            "SELECT COUNT(*) FROM key_windows WHERE closed=0"
        ).fetchone()
        return int(row[0])

    def committed_minute_count(self) -> int:
        assert self.conn is not None
        row = self.conn.execute("SELECT COUNT(*) FROM minute_windows").fetchone()
        return int(row[0])

    def canonical_event_count(self) -> int:
        assert self.conn is not None
        row = self.conn.execute("SELECT COUNT(*) FROM dedup").fetchone()
        return int(row[0])

    def canonical_counts_by_side(self) -> dict[str, int]:
        assert self.conn is not None
        rows = self.conn.execute(
            "SELECT side,COUNT(*) FROM dedup GROUP BY side"
        ).fetchall()
        counts = {"PAYMENT": 0, "REFUND": 0}
        for row in rows:
            counts[str(row[0])] = int(row[1])
        return counts

    def join_diagnostics(self) -> dict[str, int]:
        """Final canonical Join diagnostics, independent of aggregation order."""

        assert self.conn is not None
        row = self.conn.execute(
            "SELECT "
            "COALESCE(SUM(CASE WHEN p.event_time IS NULL OR r.event_time < p.event_time "
            "THEN 1 ELSE 0 END),0) unmatched, "
            "COALESCE(SUM(CASE WHEN p.event_time IS NOT NULL "
            "AND r.event_time >= p.event_time AND r.currency <> p.currency "
            "THEN 1 ELSE 0 END),0) currency_mismatches "
            "FROM dedup r LEFT JOIN dedup p "
            "ON p.side='PAYMENT' AND p.payment_id=r.payment_id "
            "AND p.window_start=r.window_start "
            "WHERE r.side='REFUND'"
        ).fetchone()
        return {
            "unmatched_refunds": int(row["unmatched"]),
            "currency_mismatches": int(row["currency_mismatches"]),
        }

    def key_window_count(self) -> int:
        assert self.conn is not None
        row = self.conn.execute("SELECT COUNT(*) FROM key_windows").fetchone()
        return int(row[0])

    def minute_windows(self) -> list[dict[str, Any]]:
        assert self.conn is not None
        rows = self.conn.execute(
            "SELECT m.window_start,m.pay_count,m.pay_amount_minor,m.refund_count,"
            "m.refund_amount_minor,m.net_minor,m.currency,m.close_reason,m.commit_seq,"
            "c.watermark "
            "FROM minute_windows m LEFT JOIN closed_minutes c "
            "ON c.window_start=m.window_start ORDER BY m.window_start"
        ).fetchall()
        return [
            {
                "window_start": int(row["window_start"]),
                "pay_count": int(row["pay_count"]),
                "pay_amount_minor": int(row["pay_amount_minor"]),
                "refund_count": int(row["refund_count"]),
                "refund_amount_minor": int(row["refund_amount_minor"]),
                "net_minor": int(row["net_minor"]),
                "currency": str(row["currency"]),
                "close_reason": str(row["close_reason"]),
                "commit_seq": int(row["commit_seq"]),
                "closed_watermark": row["watermark"],
            }
            for row in rows
        ]

    def key_windows(self) -> Iterator[dict[str, Any]]:
        assert self.conn is not None
        cursor = self.conn.execute(
            "SELECT payment_id,window_start,pay_count,pay_amount_minor,"
            "refund_count,refund_amount_minor,currency,closed,close_reason,commit_seq "
            "FROM key_windows ORDER BY window_start,payment_id"
        )
        for row in cursor:
            yield {
                "payment_id": str(row["payment_id"]),
                "window_start": int(row["window_start"]),
                "pay_count": int(row["pay_count"]),
                "pay_amount_minor": int(row["pay_amount_minor"]),
                "refund_count": int(row["refund_count"]),
                "refund_amount_minor": int(row["refund_amount_minor"]),
                "currency": str(row["currency"]),
                "closed": bool(row["closed"]),
                "close_reason": row["close_reason"],
                "commit_seq": row["commit_seq"],
            }

    def group_meta(self) -> Iterator[dict[str, Any]]:
        """Stream unique key-window groups and their canonical event counts."""

        assert self.conn is not None
        cursor = self.conn.execute(
            "SELECT payment_id,window_start,COUNT(*) event_count "
            "FROM dedup GROUP BY payment_id,window_start "
            "ORDER BY payment_id,window_start"
        )
        for row in cursor:
            yield {
                "payment_id": str(row["payment_id"]),
                "window_start": int(row["window_start"]),
                "event_count": int(row["event_count"]),
            }

    def late_summary(self) -> dict[str, Any]:
        assert self.conn is not None
        rows = self.conn.execute(
            "SELECT window_start,late_count,late_pay_count,late_refund_count,"
            "late_amount_minor,first_late_event_time,last_late_event_time "
            "FROM late_stats ORDER BY window_start"
        ).fetchall()
        by_window = [
            {
                "window_start": int(row["window_start"]),
                "late_count": int(row["late_count"]),
                "late_pay_count": int(row["late_pay_count"]),
                "late_refund_count": int(row["late_refund_count"]),
                "late_amount_minor": int(row["late_amount_minor"]),
                "first_late_event_time": row["first_late_event_time"],
                "last_late_event_time": row["last_late_event_time"],
            }
            for row in rows
        ]
        key_count_row = self.conn.execute(
            "SELECT COUNT(*) FROM late_key_stats"
        ).fetchone()
        return {
            "total": self.counter_get("late_total"),
            "pay": self.counter_get("late_pay_total"),
            "refund": self.counter_get("late_refund_total"),
            "amount_minor": self.counter_get("late_amount_minor"),
            "key_window_count": int(key_count_row[0]),
            "by_window": by_window,
        }

    def dedup_dropped(self) -> int:
        return self.counter_get("dedup_dropped")

    def recovery_count(self) -> int:
        return self.counter_get("recovery_count")

    def spill_bytes(self) -> int:
        return self.counter_get("spill_bytes")

    def last_commits(self, limit: int = 20) -> list[dict[str, Any]]:
        assert self.conn is not None
        rows = self.conn.execute(
            "SELECT seq,input_id,kind,batch_index,watermark,closed_minute_count,"
            "committed_at FROM commits ORDER BY seq DESC LIMIT ?",
            (limit,),
        ).fetchall()
        result = [
            {
                "commit_seq": int(row["seq"]),
                "input_id": str(row["input_id"]),
                "kind": str(row["kind"]),
                "batch_index": row["batch_index"],
                "watermark": row["watermark"],
                "closed_minute_count": int(row["closed_minute_count"]),
                "committed_at": str(row["committed_at"]),
            }
            for row in rows
        ]
        result.reverse()
        return result

    def state_summary(self) -> dict[str, Any]:
        assert self.conn is not None
        last = self.last_commit()
        return {
            "schema_version": STATE_SCHEMA_VERSION,
            "last_commit_seq": last.seq if last else 0,
            "last_commit_input_id": last.input_id if last else None,
            "consumed_input_count": len(self.consumed_inputs()),
            "canonical_event_count": self.canonical_event_count(),
            "key_window_count": self.key_window_count(),
            "open_key_windows": self.open_key_windows_count(),
            "committed_minute_windows": self.committed_minute_count(),
            "watermark": self.watermark(),
            "dedup_dropped": self.dedup_dropped(),
            "late_total": self.counter_get("late_total"),
            "recovery_count": self.recovery_count(),
            "spill_bytes": self.spill_bytes(),
        }

    def validate_resume_inputs(self, catalog: Any, schema_sha256: str) -> None:
        stored_schema = self.meta_get("schema_sha256")
        if stored_schema is None:
            self.meta_set("schema_sha256", schema_sha256)
        elif stored_schema != schema_sha256:
            raise EngineError(
                "E_INPUT",
                "schema manifest changed since the state was created",
                subject=str(catalog.manifest_path),
            )
        shards = catalog.shard_by_id()
        batches = catalog.batch_by_id()
        for consumed in self.consumed_inputs():
            source = shards.get(consumed.input_id) or batches.get(consumed.input_id)
            if source is None:
                raise EngineError(
                    "E_INPUT",
                    f"committed input {consumed.input_id!r} is missing from the current catalog",
                    subject=consumed.input_id,
                )
            if (
                consumed.sha256 is not None
                and source.sha256 is not None
                and consumed.sha256 != source.sha256
            ):
                raise EngineError(
                    "E_INPUT",
                    f"committed input {consumed.input_id!r} content hash changed",
                    subject=consumed.input_id,
                )
            source_declared = getattr(source, "declared_event_time_us", None)
            if (
                consumed.declared_event_time is not None
                and source_declared is not None
                and int(consumed.declared_event_time) != int(source_declared)
            ):
                raise EngineError(
                    "E_INPUT",
                    f"committed input {consumed.input_id!r} declared_event_time changed",
                    subject=consumed.input_id,
                )
