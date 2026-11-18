"""SQLite-backed job state: window aggregates, commit flags, watermark.

All mutations for one batch happen in a single transaction, so a window
state update is only ever visible as a whole.  A locked or corrupted
database aborts the job with the stable business error code ``E_STATE``;
corrupted files are quarantined so later runs never read polluted state.
"""

from __future__ import annotations

import os
import sqlite3
import time
from contextlib import contextmanager
from pathlib import Path

from .errors import StateError

SCHEMA_VERSION = "1"

_SCHEMA = """
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE seen_events (event_id TEXT PRIMARY KEY);
CREATE TABLE window_states (
    window_start INTEGER NOT NULL,
    order_id TEXT NOT NULL,
    o_amt INTEGER NOT NULL,
    o_cnt INTEGER NOT NULL,
    r_amt INTEGER NOT NULL,
    r_cnt INTEGER NOT NULL,
    PRIMARY KEY (window_start, order_id)
);
CREATE TABLE committed_windows (
    window_start INTEGER PRIMARY KEY,
    o_amt INTEGER NOT NULL,
    o_cnt INTEGER NOT NULL,
    r_amt INTEGER NOT NULL,
    r_cnt INTEGER NOT NULL,
    matched INTEGER NOT NULL,
    committed_batch INTEGER NOT NULL
);
CREATE TABLE consumed_batches (batch_seq INTEGER PRIMARY KEY);
CREATE TABLE skew_splits (batch_seq INTEGER NOT NULL, order_id TEXT NOT NULL, parts INTEGER NOT NULL);
CREATE TABLE key_counts (order_id TEXT PRIMARY KEY, cnt INTEGER NOT NULL);
"""


def _is_lock_error(exc: sqlite3.Error) -> bool:
    return "locked" in str(exc).lower()


class StateStore:
    def __init__(self, path: Path):
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        fresh = not self.path.exists()
        try:
            self.conn = sqlite3.connect(str(self.path), timeout=0.5)
            self.conn.execute("PRAGMA busy_timeout=500")
            ok = self.conn.execute("PRAGMA quick_check").fetchone()[0]
            if ok != "ok":
                raise StateError(f"integrity check failed: {ok}")
            if fresh:
                self.conn.executescript(_SCHEMA)
                self.conn.execute(
                    "INSERT INTO meta(key, value) VALUES ('schema_version', ?)",
                    (SCHEMA_VERSION,),
                )
                self.conn.commit()
            else:
                version = self.get_meta("schema_version")
                if version != SCHEMA_VERSION:
                    raise StateError(f"unsupported state schema version: {version!r}")
        except sqlite3.OperationalError as exc:
            self.conn = None
            if _is_lock_error(exc):
                raise StateError(f"state database is locked: {self.path}") from exc
            self._quarantine()
            raise StateError(f"state database unusable: {exc}") from exc
        except sqlite3.DatabaseError as exc:
            self.conn = None
            self._quarantine()
            raise StateError(f"state database corrupted: {exc}") from exc
        except StateError:
            if self.conn is not None:
                try:
                    self.conn.close()
                except Exception:
                    pass
                self.conn = None
                self._quarantine()
            raise

    def _quarantine(self) -> None:
        """Move a polluted state file aside so later runs start clean."""
        if self.conn is not None:
            try:
                self.conn.close()
            except Exception:
                pass
            self.conn = None
        stamp = int(time.time() * 1000)
        for suffix in ("", "-journal", "-wal", "-shm"):
            src = Path(str(self.path) + suffix)
            if src.exists():
                dst = Path(f"{src}.corrupt-{stamp}")
                os.replace(src, dst)

    @contextmanager
    def transaction(self):
        try:
            self.conn.execute("BEGIN IMMEDIATE")
            yield self.conn
            self.conn.commit()
        except sqlite3.IntegrityError:
            self.conn.rollback()
            raise
        except sqlite3.OperationalError as exc:
            self.conn.rollback()
            if _is_lock_error(exc):
                raise StateError("state database became locked mid-job") from exc
            self._quarantine()
            raise StateError(f"state database error: {exc}") from exc
        except sqlite3.DatabaseError as exc:
            self.conn.rollback()
            self._quarantine()
            raise StateError(f"state database corrupted mid-job: {exc}") from exc

    # -- metadata ------------------------------------------------------
    def get_meta(self, key: str, default: str | None = None) -> str | None:
        row = self.conn.execute(
            "SELECT value FROM meta WHERE key = ?", (key,)
        ).fetchone()
        return row[0] if row else default

    def get_meta_int(self, key: str, default: int = 0) -> int:
        value = self.get_meta(key)
        return default if value is None else int(value)

    @staticmethod
    def set_meta(conn: sqlite3.Connection, key: str, value) -> None:
        conn.execute(
            "INSERT INTO meta(key, value) VALUES (?, ?) "
            "ON CONFLICT(key) DO UPDATE SET value = excluded.value",
            (key, str(value)),
        )

    # -- dedup ----------------------------------------------------------
    def load_seen(self) -> set:
        return {r[0] for r in self.conn.execute("SELECT event_id FROM seen_events")}

    def load_key_counts(self) -> dict:
        return {
            r[0]: r[1]
            for r in self.conn.execute("SELECT order_id, cnt FROM key_counts")
        }

    # -- committed output ----------------------------------------------
    def committed_windows(self) -> list[dict]:
        rows = self.conn.execute(
            "SELECT window_start, o_amt, o_cnt, r_amt, r_cnt, matched "
            "FROM committed_windows ORDER BY window_start"
        ).fetchall()
        return [
            {
                "window_start": r[0],
                "order_amount_cents": r[1],
                "order_count": r[2],
                "refund_amount_cents": r[3],
                "refund_count": r[4],
                "matched_keys": r[5],
            }
            for r in rows
        ]

    def skew_splits(self) -> list[dict]:
        rows = self.conn.execute(
            "SELECT batch_seq, order_id, parts FROM skew_splits "
            "ORDER BY batch_seq, order_id"
        ).fetchall()
        return [{"batch": r[0], "key": r[1], "parts": r[2]} for r in rows]

    def consumed_batch_count(self) -> int:
        return self.conn.execute("SELECT COUNT(*) FROM consumed_batches").fetchone()[0]

    def close(self) -> None:
        if self.conn is not None:
            self.conn.close()
            self.conn = None
