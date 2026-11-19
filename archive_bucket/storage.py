"""Independent revision-aware buckets backed by SQLite and blobs."""

from __future__ import annotations

import hashlib
import json
import os
import re
import secrets
import sqlite3
import threading
import time
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterator, Mapping, Sequence

from .errors import BadRequest, Conflict, Gone, NotFound, SyncIncomplete
from .lock import RWLock

_KEY_RE = re.compile(r"^[^\x00-\x1f]{1,1024}$")
_HASH_RE = re.compile(r"^[0-9a-f]{64}$")


def utcnow() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def _json_loads(value: str | None, default: Any = None) -> Any:
    if value is None:
        return default
    try:
        return json.loads(value)
    except json.JSONDecodeError:
        return default


def _json_dumps(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


class BucketStore:
    """One storage domain.

    Metadata lives in a private SQLite database. Revision bodies are stored by
    content hash in a private directory. Commit and revision identifiers are
    independent counters for each bucket.
    """

    def __init__(self, name: str, root: Path, *, busytimeout_ms: int = 10000) -> None:
        self.name = name
        self.root = Path(root)
        self.db_path = self.root / "bucket.sqlite3"
        self.blob_root = self.root / "blobs"
        self.tmp_root = self.root / "tmp"
        self.lock = RWLock()
        self._busytimeout_ms = busytimeout_ms
        self.root.mkdir(parents=True, exist_ok=True)
        self.blob_root.mkdir(parents=True, exist_ok=True)
        self.tmp_root.mkdir(parents=True, exist_ok=True)
        self._initialize()

    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(
            self.db_path,
            timeout=self._busytimeout_ms / 1000,
            isolation_level=None,
            check_same_thread=False,
        )
        conn.row_factory = sqlite3.Row
        conn.execute(f"PRAGMA busy_timeout={self._busytimeout_ms}")
        conn.execute("PRAGMA synchronous=NORMAL")
        conn.execute("PRAGMA foreign_keys=ON")
        return conn

    @contextmanager
    def _connection(self) -> Iterator[sqlite3.Connection]:
        conn = self._connect()
        try:
            yield conn
        finally:
            conn.close()

    @contextmanager
    def _transaction(self, conn: sqlite3.Connection, immediate: bool = True) -> Iterator[None]:
        conn.execute("BEGIN IMMEDIATE" if immediate else "BEGIN")
        try:
            yield
        except BaseException:
            conn.execute("ROLLBACK")
            raise
        else:
            conn.execute("COMMIT")

    def _initialize(self) -> None:
        with self._connection() as conn:
            conn.execute("PRAGMA journal_mode=WAL")
            conn.executescript(
                """
                CREATE TABLE IF NOT EXISTS meta (
                    key TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                );
                CREATE TABLE IF NOT EXISTS commits (
                    seq INTEGER PRIMARY KEY,
                    op TEXT NOT NULL CHECK (op IN ('put','delete','restore')),
                    object_key TEXT,
                    revision_id INTEGER,
                    content_hash TEXT,
                    size INTEGER,
                    payload TEXT,
                    created_at TEXT NOT NULL
                );
                CREATE TABLE IF NOT EXISTS revisions (
                    revision_id INTEGER PRIMARY KEY,
                    object_key TEXT NOT NULL,
                    commit_seq INTEGER NOT NULL,
                    content_hash TEXT NOT NULL,
                    size INTEGER NOT NULL,
                    created_at TEXT NOT NULL,
                    reclaimed INTEGER NOT NULL DEFAULT 0,
                    reclaimed_at TEXT
                );
                CREATE INDEX IF NOT EXISTS idx_revisions_key
                    ON revisions(object_key, revision_id);
                CREATE INDEX IF NOT EXISTS idx_revisions_hash
                    ON revisions(content_hash);
                CREATE TABLE IF NOT EXISTS object_state (
                    object_key TEXT PRIMARY KEY,
                    visible_revision_id INTEGER,
                    updated_commit_seq INTEGER NOT NULL
                );
                CREATE INDEX IF NOT EXISTS idx_object_state_visible
                    ON object_state(visible_revision_id);
                CREATE TABLE IF NOT EXISTS snapshots (
                    snapshot_id TEXT PRIMARY KEY,
                    commit_seq INTEGER NOT NULL,
                    created_at TEXT NOT NULL,
                    object_count INTEGER NOT NULL,
                    revision_count INTEGER NOT NULL
                );
                CREATE TABLE IF NOT EXISTS snapshot_entries (
                    snapshot_id TEXT NOT NULL,
                    object_key TEXT NOT NULL,
                    revision_id INTEGER NOT NULL,
                    content_hash TEXT NOT NULL,
                    size INTEGER NOT NULL,
                    PRIMARY KEY (snapshot_id, object_key)
                );
                CREATE INDEX IF NOT EXISTS idx_snapshot_entries_revision
                    ON snapshot_entries(revision_id);
                """
            )
            for key, value in (
                ("next_commit_seq", "1"),
                ("next_revision_id", "1"),
                ("applied_seq", "0"),
                ("reclaimed_revision_count", "0"),
                ("reclaimed_bytes", "0"),
            ):
                conn.execute(
                    "INSERT OR IGNORE INTO meta(key, value) VALUES(?, ?)",
                    (key, value),
                )

    @staticmethod
    def _meta_get(conn: sqlite3.Connection, key: str, default: str = "0") -> str:
        row = conn.execute("SELECT value FROM meta WHERE key=?", (key,)).fetchone()
        return row["value"] if row else default

    @staticmethod
    def _meta_set(conn: sqlite3.Connection, key: str, value: str) -> None:
        conn.execute(
            "INSERT INTO meta(key,value) VALUES(?,?) "
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (key, value),
        )

    @staticmethod
    def _next_id(conn: sqlite3.Connection, key: str) -> int:
        value = int(BucketStore._meta_get(conn, key))
        BucketStore._meta_set(conn, key, str(value + 1))
        return value

    @staticmethod
    def validate_key(key: str) -> str:
        if not isinstance(key, str) or not _KEY_RE.fullmatch(key):
            raise BadRequest("invalid object key", key=key)
        return key

    @staticmethod
    def _blob_relpath(content_hash: str) -> str:
        return f"{content_hash[:2]}/{content_hash}.bin"

    def _blob_path(self, content_hash: str) -> Path:
        return self.blob_root / content_hash[:2] / f"{content_hash}.bin"

    def _ensure_blob(self, content_hash: str, data: bytes) -> None:
        path = self._blob_path(content_hash)
        if path.exists():
            return
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = self.tmp_root / f".{content_hash}.{secrets.token_hex(8)}.tmp"
        try:
            with tmp.open("wb") as handle:
                handle.write(data)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(tmp, path)
        finally:
            try:
                tmp.unlink()
            except FileNotFoundError:
                pass

    def _revision_row(self, conn: sqlite3.Connection, revision_id: int) -> sqlite3.Row | None:
        return conn.execute(
            "SELECT * FROM revisions WHERE revision_id=?",
            (revision_id,),
        ).fetchone()

    @staticmethod
    def _revision_meta(row: Mapping[str, Any]) -> dict[str, Any]:
        return {
            "content_hash": row["content_hash"],
            "reclaimed": bool(row["reclaimed"]),
            "revision_id": int(row["revision_id"]),
            "size": int(row["size"]),
        }

    def latest_seq(self) -> int:
        with self.lock.read(), self._connection() as conn:
            row = conn.execute("SELECT COALESCE(MAX(seq),0) AS seq FROM commits").fetchone()
            return int(row["seq"])

    def applied_seq(self) -> int:
        with self.lock.read(), self._connection() as conn:
            return int(self._meta_get(conn, "applied_seq"))

    def _visible_rows(
        self,
        conn: sqlite3.Connection,
        *,
        prefix: str = "",
        limit: int | None = None,
    ) -> list[sqlite3.Row]:
        sql = (
            "SELECT s.object_key, s.visible_revision_id AS revision_id, "
            "r.content_hash, r.size, r.commit_seq, r.reclaimed "
            "FROM object_state s JOIN revisions r "
            "ON r.revision_id=s.visible_revision_id "
            "WHERE s.visible_revision_id IS NOT NULL AND r.reclaimed=0 "
        )
        args: list[Any] = []
        if prefix:
            sql += "AND s.object_key LIKE ? ESCAPE '\\' "
            escaped = prefix.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
            args.append(escaped + "%")
        sql += "ORDER BY s.object_key"
        if limit is not None:
            sql += " LIMIT ?"
            args.append(limit)
        return list(conn.execute(sql, args).fetchall())

    def list_visible(self, prefix: str = "", limit: int | None = None) -> dict[str, Any]:
        with self.lock.read(), self._connection() as conn:
            seq = int(
                conn.execute("SELECT COALESCE(MAX(seq),0) AS seq FROM commits").fetchone()["seq"]
            )
            rows = self._visible_rows(conn, prefix=prefix, limit=limit)
            return {
                "commit_seq": seq,
                "objects": [self._revision_meta(row) | {"key": row["object_key"]} for row in rows],
            }

    def get_latest(self, key: str) -> tuple[dict[str, Any], bytes]:
        self.validate_key(key)
        with self.lock.read(), self._connection() as conn:
            row = conn.execute(
                "SELECT s.visible_revision_id AS revision_id, r.* "
                "FROM object_state s JOIN revisions r "
                "ON r.revision_id=s.visible_revision_id "
                "WHERE s.object_key=? AND s.visible_revision_id IS NOT NULL",
                (key,),
            ).fetchone()
            if row is None:
                raise NotFound("visible revision not found", key=key)
            if row["reclaimed"]:
                raise Gone("revision was reclaimed", key=key, revision_id=row["revision_id"])
            return self._read_revision_locked(row)

    def get_revision(self, key: str, revision_id: int) -> tuple[dict[str, Any], bytes]:
        self.validate_key(key)
        with self.lock.read(), self._connection() as conn:
            row = self._revision_row(conn, revision_id)
            if row is None or row["object_key"] != key:
                raise NotFound(
                    "revision not found",
                    key=key,
                    revision_id=revision_id,
                )
            if row["reclaimed"]:
                raise Gone(
                    "revision was reclaimed",
                    key=key,
                    revision_id=revision_id,
                )
            meta, data = self._read_revision_locked(row)
            meta["visible"] = self._is_visible(conn, key, revision_id)
            return meta, data

    def _read_revision_locked(
        self,
        row: Mapping[str, Any],
    ) -> tuple[dict[str, Any], bytes]:
        path = self._blob_path(row["content_hash"])
        try:
            data = path.read_bytes()
        except FileNotFoundError as exc:
            raise Conflict(
                "revision metadata exists but content is missing",
                key=row["object_key"],
                revision_id=row["revision_id"],
            ) from exc
        meta = self._revision_meta(row)
        meta["commit_seq"] = int(row["commit_seq"])
        meta["key"] = row["object_key"]
        return meta, data

    @staticmethod
    def _is_visible(conn: sqlite3.Connection, key: str, revision_id: int) -> bool:
        row = conn.execute(
            "SELECT visible_revision_id FROM object_state WHERE object_key=?",
            (key,),
        ).fetchone()
        return row is not None and row["visible_revision_id"] == revision_id

    def list_revisions(self, key: str) -> dict[str, Any]:
        self.validate_key(key)
        with self.lock.read(), self._connection() as conn:
            visible = conn.execute(
                "SELECT visible_revision_id FROM object_state WHERE object_key=?",
                (key,),
            ).fetchone()
            visible_id = visible["visible_revision_id"] if visible else None
            rows = conn.execute(
                "SELECT * FROM revisions WHERE object_key=? ORDER BY revision_id",
                (key,),
            ).fetchall()
            if not rows:
                raise NotFound("object has no revisions", key=key)
            return {
                "key": key,
                "visible_revision_id": visible_id,
                "revisions": [
                    self._revision_meta(row)
                    | {
                        "commit_seq": int(row["commit_seq"]),
                        "visible": row["revision_id"] == visible_id,
                    }
                    for row in rows
                ],
            }

    def put(
        self,
        key: str,
        data: bytes,
        *,
        expected_sha256: str | None = None,
    ) -> dict[str, Any]:
        self.validate_key(key)
        content_hash = hashlib.sha256(data).hexdigest()
        if expected_sha256 is not None and expected_sha256.lower() != content_hash:
            raise BadRequest(
                "content hash mismatch",
                expected=expected_sha256,
                actual=content_hash,
            )
        with self.lock.write():
            self._ensure_blob(content_hash, data)
            with self._connection() as conn, self._transaction(conn):
                seq = self._next_id(conn, "next_commit_seq")
                revision_id = self._next_id(conn, "next_revision_id")
                created_at = utcnow()
                conn.execute(
                    "INSERT INTO commits(seq,op,object_key,revision_id,content_hash,size,payload,created_at) "
                    "VALUES(?,?,?,?,?,?,?,?)",
                    (
                        seq,
                        "put",
                        key,
                        revision_id,
                        content_hash,
                        len(data),
                        None,
                        created_at,
                    ),
                )
                conn.execute(
                    "INSERT INTO revisions"
                    "(revision_id,object_key,commit_seq,content_hash,size,created_at,reclaimed) "
                    "VALUES(?,?,?,?,?,?,0)",
                    (revision_id, key, seq, content_hash, len(data), created_at),
                )
                conn.execute(
                    "INSERT INTO object_state(object_key,visible_revision_id,updated_commit_seq) "
                    "VALUES(?,?,?) ON CONFLICT(object_key) DO UPDATE SET "
                    "visible_revision_id=excluded.visible_revision_id, "
                    "updated_commit_seq=excluded.updated_commit_seq",
                    (key, revision_id, seq),
                )
        return {
            "commit_seq": seq,
            "content_hash": content_hash,
            "key": key,
            "revision_id": revision_id,
            "size": len(data),
        }

    def delete(self, key: str) -> dict[str, Any]:
        self.validate_key(key)
        with self.lock.write(), self._connection() as conn, self._transaction(conn):
            seq = self._next_id(conn, "next_commit_seq")
            created_at = utcnow()
            conn.execute(
                "INSERT INTO commits(seq,op,object_key,revision_id,content_hash,size,payload,created_at) "
                "VALUES(?,?,?,?,?,?,?,?)",
                (seq, "delete", key, None, None, None, None, created_at),
            )
            conn.execute(
                "INSERT INTO object_state(object_key,visible_revision_id,updated_commit_seq) "
                "VALUES(?,NULL,?) ON CONFLICT(object_key) DO UPDATE SET "
                "visible_revision_id=NULL, updated_commit_seq=excluded.updated_commit_seq",
                (key, seq),
            )
        return {"commit_seq": seq, "key": key, "visible": False}

    def create_snapshot(self, snapshot_id: str | None = None) -> dict[str, Any]:
        snapshot_id = snapshot_id or f"snap_{time.strftime('%Y%m%dT%H%M%S', time.gmtime())}_{secrets.token_hex(6)}"
        if not re.fullmatch(r"[A-Za-z0-9_.:-]{1,160}", snapshot_id):
            raise BadRequest("invalid snapshot id", snapshot_id=snapshot_id)
        with self.lock.write(), self._connection() as conn, self._transaction(conn):
            if conn.execute(
                "SELECT 1 FROM snapshots WHERE snapshot_id=?", (snapshot_id,)
            ).fetchone():
                raise Conflict("snapshot already exists", snapshot_id=snapshot_id)
            commit_seq = int(
                conn.execute("SELECT COALESCE(MAX(seq),0) AS seq FROM commits").fetchone()["seq"]
            )
            rows = self._visible_rows(conn)
            created_at = utcnow()
            conn.execute(
                "INSERT INTO snapshots"
                "(snapshot_id,commit_seq,created_at,object_count,revision_count) "
                "VALUES(?,?,?,?,?)",
                (snapshot_id, commit_seq, created_at, len(rows), len(rows)),
            )
            conn.executemany(
                "INSERT INTO snapshot_entries"
                "(snapshot_id,object_key,revision_id,content_hash,size) VALUES(?,?,?,?,?)",
                [
                    (
                        snapshot_id,
                        row["object_key"],
                        row["revision_id"],
                        row["content_hash"],
                        row["size"],
                    )
                    for row in rows
                ],
            )
            entries = [
                {
                    "content_hash": row["content_hash"],
                    "key": row["object_key"],
                    "revision_id": int(row["revision_id"]),
                    "size": int(row["size"]),
                }
                for row in rows
            ]
        return {
            "commit_seq": commit_seq,
            "created_at": created_at,
            "entries": entries,
            "object_count": len(entries),
            "revision_count": len(entries),
            "snapshot_id": snapshot_id,
            "visible": {entry["key"]: entry["revision_id"] for entry in entries},
        }

    def _snapshot_locked(self, conn: sqlite3.Connection, snapshot_id: str) -> dict[str, Any]:
        row = conn.execute(
            "SELECT * FROM snapshots WHERE snapshot_id=?",
            (snapshot_id,),
        ).fetchone()
        if row is None:
            raise NotFound("snapshot not found", snapshot_id=snapshot_id)
        entry_rows = conn.execute(
            "SELECT * FROM snapshot_entries WHERE snapshot_id=? ORDER BY object_key",
            (snapshot_id,),
        ).fetchall()
        entries = [
            {
                "content_hash": entry["content_hash"],
                "key": entry["object_key"],
                "revision_id": int(entry["revision_id"]),
                "size": int(entry["size"]),
            }
            for entry in entry_rows
        ]
        return {
            "commit_seq": int(row["commit_seq"]),
            "created_at": row["created_at"],
            "entries": entries,
            "object_count": int(row["object_count"]),
            "revision_count": int(row["revision_count"]),
            "snapshot_id": row["snapshot_id"],
            "visible": {entry["key"]: entry["revision_id"] for entry in entries},
        }

    def get_snapshot(self, snapshot_id: str) -> dict[str, Any]:
        with self.lock.read(), self._connection() as conn:
            return self._snapshot_locked(conn, snapshot_id)

    def delete_snapshot(self, snapshot_id: str) -> dict[str, Any]:
        with self.lock.write(), self._connection() as conn, self._transaction(conn):
            snapshot = self._snapshot_locked(conn, snapshot_id)
            conn.execute(
                "DELETE FROM snapshot_entries WHERE snapshot_id=?",
                (snapshot_id,),
            )
            conn.execute("DELETE FROM snapshots WHERE snapshot_id=?", (snapshot_id,))
        return {
            "deleted": True,
            "object_count": snapshot["object_count"],
            "snapshot_id": snapshot_id,
        }

    def list_snapshots(self) -> dict[str, Any]:
        with self.lock.read(), self._connection() as conn:
            rows = conn.execute(
                "SELECT * FROM snapshots ORDER BY created_at, snapshot_id"
            ).fetchall()
            return {
                "snapshots": [
                    {
                        "commit_seq": int(row["commit_seq"]),
                        "created_at": row["created_at"],
                        "object_count": int(row["object_count"]),
                        "revision_count": int(row["revision_count"]),
                        "snapshot_id": row["snapshot_id"],
                    }
                    for row in rows
                ]
            }

    def restore_snapshot(self, snapshot_id: str) -> dict[str, Any]:
        with self.lock.write(), self._connection() as conn, self._transaction(conn):
            snapshot = self._snapshot_locked(conn, snapshot_id)
            visible = snapshot["visible"]
            if visible:
                placeholders = ",".join("?" for _ in visible)
                present = {
                    int(row["revision_id"])
                    for row in conn.execute(
                        f"SELECT revision_id FROM revisions WHERE revision_id IN ({placeholders})",
                        list(visible.values()),
                    ).fetchall()
                }
                missing = sorted(set(visible.values()) - present)
                if missing:
                    raise Conflict(
                        "snapshot references revisions absent from this bucket",
                        missing_revision_ids=missing,
                        snapshot_id=snapshot_id,
                    )
            all_states = {
                row["object_key"]
                for row in conn.execute("SELECT object_key FROM object_state").fetchall()
            }
            seq = self._next_id(conn, "next_commit_seq")
            created_at = utcnow()
            payload = _json_dumps(visible)
            conn.execute(
                "INSERT INTO commits(seq,op,object_key,revision_id,content_hash,size,payload,created_at) "
                "VALUES(?,?,?,?,?,?,?,?)",
                (seq, "restore", None, None, None, None, payload, created_at),
            )
            for key in sorted(all_states - set(visible)):
                conn.execute(
                    "UPDATE object_state SET visible_revision_id=NULL, updated_commit_seq=? "
                    "WHERE object_key=?",
                    (seq, key),
                )
            for key, revision_id in sorted(visible.items()):
                conn.execute(
                    "INSERT INTO object_state(object_key,visible_revision_id,updated_commit_seq) "
                    "VALUES(?,?,?) ON CONFLICT(object_key) DO UPDATE SET "
                    "visible_revision_id=excluded.visible_revision_id, "
                    "updated_commit_seq=excluded.updated_commit_seq",
                    (key, revision_id, seq),
                )
        return {
            "commit_seq": seq,
            "created_at": created_at,
            "snapshot_commit_seq": snapshot["commit_seq"],
            "snapshot_id": snapshot_id,
            "visible_count": len(visible),
        }

    def commit_events(self, after_seq: int = 0, limit: int = 100) -> dict[str, Any]:
        if after_seq < 0:
            raise BadRequest("after_seq must be >= 0")
        limit = max(1, min(int(limit), 500))
        with self.lock.read(), self._connection() as conn:
            latest = int(
                conn.execute("SELECT COALESCE(MAX(seq),0) AS seq FROM commits").fetchone()["seq"]
            )
            rows = conn.execute(
                "SELECT * FROM commits WHERE seq>? ORDER BY seq LIMIT ?",
                (after_seq, limit),
            ).fetchall()
            events: list[dict[str, Any]] = []
            for row in rows:
                event: dict[str, Any] = {
                    "commit_seq": int(row["seq"]),
                    "op": row["op"],
                }
                if row["object_key"] is not None:
                    event["key"] = row["object_key"]
                if row["revision_id"] is not None:
                    event["revision_id"] = int(row["revision_id"])
                if row["content_hash"] is not None:
                    event["content_hash"] = row["content_hash"]
                if row["size"] is not None:
                    event["size"] = int(row["size"])
                if row["op"] == "restore":
                    event["visible"] = _json_loads(row["payload"], {})
                events.append(event)
            last = events[-1]["commit_seq"] if events else after_seq
            return {
                "after_seq": after_seq,
                "events": events,
                "has_more": last < latest,
                "latest_seq": latest,
            }

    def apply_sync_event(self, event: Mapping[str, Any], data: bytes | None = None) -> dict[str, Any]:
        op = event.get("op")
        seq = int(event.get("commit_seq", -1))
        if op not in {"put", "delete", "restore"}:
            raise SyncIncomplete("unknown commit operation", op=op, commit_seq=seq)
        with self.lock.write(), self._connection() as conn:
            applied = int(self._meta_get(conn, "applied_seq"))
            if seq <= applied:
                return {"applied": False, "applied_seq": applied, "commit_seq": seq}
            if seq != applied + 1:
                raise SyncIncomplete(
                    "commit sequence gap",
                    expected_seq=applied + 1,
                    received_seq=seq,
                )
            key = event.get("key")
            if op in {"put", "delete"}:
                if not isinstance(key, str):
                    raise SyncIncomplete("commit has no object key", commit_seq=seq)
                self.validate_key(key)

            prepared_hash: str | None = None
            if op == "put":
                if data is None:
                    raise SyncIncomplete("put commit has no content", commit_seq=seq)
                prepared_hash = hashlib.sha256(data).hexdigest()
                if prepared_hash != event.get("content_hash"):
                    raise SyncIncomplete(
                        "source content hash mismatch",
                        expected=event.get("content_hash"),
                        actual=prepared_hash,
                        commit_seq=seq,
                    )
                self._ensure_blob(prepared_hash, data)

            try:
                with self._transaction(conn):
                    existing_commit = conn.execute(
                        "SELECT * FROM commits WHERE seq=?", (seq,)
                    ).fetchone()
                    if existing_commit is not None:
                        raise Conflict("backup commit exists before applied_seq", commit_seq=seq)
                    created_at = utcnow()
                    revision_id = event.get("revision_id")
                    content_hash = event.get("content_hash")
                    size = event.get("size")
                    payload = None
                    if op == "put":
                        revision_id = int(revision_id)
                        size = int(size)
                        existing = self._revision_row(conn, revision_id)
                        if existing is not None:
                            if (
                                existing["object_key"] != key
                                or existing["content_hash"] != content_hash
                                or int(existing["size"]) != size
                            ):
                                raise Conflict(
                                    "backup revision id collision",
                                    revision_id=revision_id,
                                )
                        else:
                            conn.execute(
                                "INSERT INTO revisions"
                                "(revision_id,object_key,commit_seq,content_hash,size,created_at,reclaimed) "
                                "VALUES(?,?,?,?,?,?,0)",
                                (revision_id, key, seq, content_hash, size, created_at),
                            )
                        next_revision = int(self._meta_get(conn, "next_revision_id"))
                        if revision_id >= next_revision:
                            self._meta_set(conn, "next_revision_id", str(revision_id + 1))
                        visible_id: int | None = revision_id
                    elif op == "delete":
                        visible_id = None
                    else:
                        visible = event.get("visible")
                        if not isinstance(visible, dict):
                            raise SyncIncomplete("restore commit has invalid visible map", commit_seq=seq)
                        normalized: dict[str, int] = {}
                        for restore_key, restore_revision in visible.items():
                            if not isinstance(restore_key, str) or not isinstance(restore_revision, int):
                                raise SyncIncomplete(
                                    "restore commit contains invalid entry",
                                    commit_seq=seq,
                                )
                            normalized[restore_key] = restore_revision
                        payload = _json_dumps(normalized)
                        missing: list[int] = []
                        for restore_revision in sorted(set(normalized.values())):
                            row = self._revision_row(conn, restore_revision)
                            if row is None:
                                missing.append(restore_revision)
                        if missing:
                            raise SyncIncomplete(
                                "restore commit references revisions not yet synchronized",
                                missing_revision_ids=missing,
                                commit_seq=seq,
                            )
                        conn.execute(
                            "UPDATE object_state SET visible_revision_id=NULL, "
                            "updated_commit_seq=?",
                            (seq,),
                        )
                        for restore_key, restore_revision in sorted(normalized.items()):
                            conn.execute(
                                "INSERT INTO object_state"
                                "(object_key,visible_revision_id,updated_commit_seq) "
                                "VALUES(?,?,?) ON CONFLICT(object_key) DO UPDATE SET "
                                "visible_revision_id=excluded.visible_revision_id, "
                                "updated_commit_seq=excluded.updated_commit_seq",
                                (restore_key, restore_revision, seq),
                            )
                        visible_id = None

                    conn.execute(
                        "INSERT INTO commits"
                        "(seq,op,object_key,revision_id,content_hash,size,payload,created_at) "
                        "VALUES(?,?,?,?,?,?,?,?)",
                        (
                            seq,
                            op,
                            key,
                            int(revision_id) if op == "put" else None,
                            content_hash if op == "put" else None,
                            int(size) if op == "put" else None,
                            payload,
                            created_at,
                        ),
                    )
                    if op == "put":
                        conn.execute(
                            "INSERT INTO object_state"
                            "(object_key,visible_revision_id,updated_commit_seq) "
                            "VALUES(?,?,?) ON CONFLICT(object_key) DO UPDATE SET "
                            "visible_revision_id=excluded.visible_revision_id, "
                            "updated_commit_seq=excluded.updated_commit_seq",
                            (key, visible_id, seq),
                        )
                    elif op == "delete":
                        conn.execute(
                            "INSERT INTO object_state"
                            "(object_key,visible_revision_id,updated_commit_seq) "
                            "VALUES(?,NULL,?) ON CONFLICT(object_key) DO UPDATE SET "
                            "visible_revision_id=NULL, updated_commit_seq=excluded.updated_commit_seq",
                            (key, seq),
                        )
                    next_commit = int(self._meta_get(conn, "next_commit_seq"))
                    if seq >= next_commit:
                        self._meta_set(conn, "next_commit_seq", str(seq + 1))
                    self._meta_set(conn, "applied_seq", str(seq))
            except BaseException:
                # Blob insertion is idempotent; no metadata was committed.
                raise
        return {"applied": True, "applied_seq": seq, "commit_seq": seq}

    def visible_map_as_of(self, commit_seq: int) -> dict[str, int]:
        if commit_seq < 0:
            raise BadRequest("commit_seq must be >= 0")
        with self.lock.read(), self._connection() as conn:
            rows = conn.execute(
                "SELECT * FROM commits WHERE seq<=? ORDER BY seq",
                (commit_seq,),
            ).fetchall()
            visible: dict[str, int] = {}
            for row in rows:
                if row["op"] == "put":
                    visible[row["object_key"]] = int(row["revision_id"])
                elif row["op"] == "delete":
                    visible.pop(row["object_key"], None)
                else:
                    restored = _json_loads(row["payload"], {})
                    visible = {
                        str(key): int(revision)
                        for key, revision in restored.items()
                    }
            return visible

    def view_at(self, commit_seq: int | None = None) -> dict[str, Any]:
        with self.lock.read(), self._connection() as conn:
            latest = int(
                conn.execute("SELECT COALESCE(MAX(seq),0) AS seq FROM commits").fetchone()["seq"]
            )
            if commit_seq is None:
                rows = self._visible_rows(conn)
                entries = [
                    self._revision_meta(row)
                    | {"commit_seq": int(row["commit_seq"]), "key": row["object_key"]}
                    for row in rows
                ]
                effective = latest
            else:
                if commit_seq < 0 or commit_seq > latest:
                    raise BadRequest(
                        "commit_seq is outside bucket history",
                        commit_seq=commit_seq,
                        latest_seq=latest,
                    )
                visible = self._visible_map_as_of_locked(conn, commit_seq)
                entries = []
                for key, revision_id in sorted(visible.items()):
                    row = self._revision_row(conn, revision_id)
                    if row is None:
                        raise Conflict(
                            "historical view references missing revision",
                            key=key,
                            revision_id=revision_id,
                        )
                    entries.append(
                        {
                            "content_hash": row["content_hash"],
                            "commit_seq": int(row["commit_seq"]),
                            "key": key,
                            "reclaimed": bool(row["reclaimed"]),
                            "revision_id": int(revision_id),
                            "size": int(row["size"]),
                        }
                    )
                effective = commit_seq
            return {
                "commit_seq": effective,
                "entries": entries,
                "visible": {entry["key"]: entry["revision_id"] for entry in entries},
            }

    def _visible_map_as_of_locked(
        self,
        conn: sqlite3.Connection,
        commit_seq: int,
    ) -> dict[str, int]:
        rows = conn.execute(
            "SELECT * FROM commits WHERE seq<=? ORDER BY seq",
            (commit_seq,),
        ).fetchall()
        visible: dict[str, int] = {}
        for row in rows:
            if row["op"] == "put":
                visible[row["object_key"]] = int(row["revision_id"])
            elif row["op"] == "delete":
                visible.pop(row["object_key"], None)
            else:
                restored = _json_loads(row["payload"], {})
                visible = {str(key): int(revision) for key, revision in restored.items()}
        return visible

    def revision_hashes(
        self,
        revision_ids: Sequence[int] | set[int],
        *,
        verify_content: bool = True,
    ) -> dict[int, dict[str, Any]]:
        wanted = sorted({int(value) for value in revision_ids})
        with self.lock.read(), self._connection() as conn:
            result: dict[int, dict[str, Any]] = {}
            for revision_id in wanted:
                row = self._revision_row(conn, revision_id)
                if row is None:
                    continue
                actual_hash: str | None = None
                if verify_content and not row["reclaimed"]:
                    path = self._blob_path(row["content_hash"])
                    try:
                        digest = hashlib.sha256()
                        with path.open("rb") as handle:
                            for chunk in iter(lambda: handle.read(1024 * 1024), b""):
                                digest.update(chunk)
                        actual_hash = digest.hexdigest()
                    except FileNotFoundError:
                        actual_hash = None
                result[revision_id] = {
                    "actual_content_hash": actual_hash,
                    "content_hash": row["content_hash"],
                    "reclaimed": bool(row["reclaimed"]),
                    "size": int(row["size"]),
                }
            return result

    def _hash_row_locked(
        self,
        row: Mapping[str, Any],
        *,
        verify_content: bool,
    ) -> dict[str, Any]:
        actual_hash: str | None = None
        if verify_content and not row["reclaimed"]:
            path = self._blob_path(row["content_hash"])
            try:
                digest = hashlib.sha256()
                with path.open("rb") as handle:
                    for chunk in iter(lambda: handle.read(1024 * 1024), b""):
                        digest.update(chunk)
                actual_hash = digest.hexdigest()
            except FileNotFoundError:
                actual_hash = None
        return {
            "actual_content_hash": actual_hash,
            "content_hash": row["content_hash"],
            "reclaimed": bool(row["reclaimed"]),
            "size": int(row["size"]),
        }

    def capture_state(
        self,
        commit_seq: int | None = None,
        *,
        use_current: bool = False,
        verify_content: bool = True,
    ) -> dict[str, Any]:
        """Capture a coherent view plus hashes while holding one read lock."""
        with self.lock.read(), self._connection() as conn:
            latest = int(
                conn.execute("SELECT COALESCE(MAX(seq),0) AS seq FROM commits").fetchone()["seq"]
            )
            applied = int(self._meta_get(conn, "applied_seq"))
            effective = latest if commit_seq is None else int(commit_seq)
            if effective < 0 or effective > latest:
                raise BadRequest(
                    "commit_seq is outside bucket history",
                    commit_seq=effective,
                    latest_seq=latest,
                )
            use_actual = effective == latest and (use_current or commit_seq is None)
            if use_actual:
                rows = self._visible_rows(conn)
                visible = {row["object_key"]: int(row["revision_id"]) for row in rows}
                row_by_revision = {int(row["revision_id"]): row for row in rows}
            else:
                visible = self._visible_map_as_of_locked(conn, effective)
                row_by_revision = {}
                for revision_id in visible.values():
                    row = self._revision_row(conn, revision_id)
                    if row is not None:
                        row_by_revision[revision_id] = row
            hashes = {
                revision_id: self._hash_row_locked(
                    row,
                    verify_content=verify_content,
                )
                for revision_id, row in row_by_revision.items()
            }
            return {
                "applied_seq": applied,
                "hashes": hashes,
                "latest_commit_seq": latest,
                "visible": visible,
            }

    def protected_revision_ids(self) -> set[int]:
        with self.lock.read(), self._connection() as conn:
            return {
                int(row["revision_id"])
                for row in conn.execute(
                    "SELECT DISTINCT revision_id FROM snapshot_entries"
                ).fetchall()
            }

    def stats(self) -> dict[str, Any]:
        with self.lock.read(), self._connection() as conn:
            visible_count = int(
                conn.execute(
                    "SELECT COUNT(*) AS count FROM object_state "
                    "WHERE visible_revision_id IS NOT NULL"
                ).fetchone()["count"]
            )
            latest_seq = int(
                conn.execute("SELECT COALESCE(MAX(seq),0) AS seq FROM commits").fetchone()["seq"]
            )
            snapshot_count = int(
                conn.execute("SELECT COUNT(*) AS count FROM snapshots").fetchone()["count"]
            )
            snapshot_refs = int(
                conn.execute(
                    "SELECT COUNT(DISTINCT revision_id) AS count FROM snapshot_entries"
                ).fetchone()["count"]
            )
            reclaimed_revisions = int(self._meta_get(conn, "reclaimed_revision_count"))
            reclaimed_bytes = int(self._meta_get(conn, "reclaimed_bytes"))
            applied_seq = int(self._meta_get(conn, "applied_seq"))
            revision_count = int(
                conn.execute("SELECT COUNT(*) AS count FROM revisions").fetchone()["count"]
            )
            occupancy = sum(
                path.stat().st_size for path in self.blob_root.glob("*/*.bin") if path.is_file()
            )
            return {
                "applied_seq": applied_seq,
                "latest_commit_seq": latest_seq,
                "occupancy_bytes": occupancy,
                "reclaimed_bytes": reclaimed_bytes,
                "reclaimed_revision_count": reclaimed_revisions,
                "revision_count": revision_count,
                "snapshot_count": snapshot_count,
                "snapshot_referenced_revisions": snapshot_refs,
                "visible_object_count": visible_count,
            }

    def gc(
        self,
        *,
        target_bytes: int | None = None,
        protect_after_seq: int | None = None,
        protect_revision_ids: set[int] | None = None,
    ) -> dict[str, Any]:
        protect_ids = protect_revision_ids or set()
        with self.lock.write(), self._connection() as conn:
            before = sum(
                path.stat().st_size for path in self.blob_root.glob("*/*.bin") if path.is_file()
            )
            if target_bytes is not None and before <= target_bytes:
                return {
                    "after_bytes": before,
                    "before_bytes": before,
                    "reclaimed_bytes": 0,
                    "reclaimed_revision_count": 0,
                }

            sql = (
                "SELECT r.* FROM revisions r "
                "WHERE r.reclaimed=0 "
                "AND NOT EXISTS ("
                " SELECT 1 FROM object_state s "
                " WHERE s.visible_revision_id=r.revision_id"
                ") "
                "AND NOT EXISTS ("
                " SELECT 1 FROM snapshot_entries e "
                " WHERE e.revision_id=r.revision_id"
                ") "
            )
            args: list[Any] = []
            if protect_after_seq is not None:
                sql += "AND r.commit_seq<=? "
                args.append(protect_after_seq)
            if protect_ids:
                placeholders = ",".join("?" for _ in protect_ids)
                sql += f"AND r.revision_id NOT IN ({placeholders}) "
                args.extend(sorted(protect_ids))
            sql += "ORDER BY r.revision_id"
            candidates = conn.execute(sql, args).fetchall()

            live_hash_counts: dict[str, int] = {
                row["content_hash"]: int(row["count"])
                for row in conn.execute(
                    "SELECT content_hash, COUNT(*) AS count FROM revisions "
                    "WHERE reclaimed=0 GROUP BY content_hash"
                ).fetchall()
            }
            selected: list[sqlite3.Row] = []
            freed_hashes: set[str] = set()
            estimated_freed = 0
            for row in candidates:
                selected.append(row)
                content_hash = row["content_hash"]
                live_hash_counts[content_hash] = live_hash_counts.get(content_hash, 0) - 1
                if live_hash_counts[content_hash] <= 0 and content_hash not in freed_hashes:
                    path = self._blob_path(content_hash)
                    if path.exists():
                        estimated_freed += path.stat().st_size
                    freed_hashes.add(content_hash)
                if target_bytes is not None and before - estimated_freed <= target_bytes:
                    break

            if not selected:
                return {
                    "after_bytes": before,
                    "before_bytes": before,
                    "reclaimed_bytes": 0,
                    "reclaimed_revision_count": 0,
                }

            with self._transaction(conn):
                now = utcnow()
                conn.executemany(
                    "UPDATE revisions SET reclaimed=1, reclaimed_at=? WHERE revision_id=?",
                    [(now, row["revision_id"]) for row in selected],
                )
                old_count = int(self._meta_get(conn, "reclaimed_revision_count"))
                self._meta_set(
                    conn,
                    "reclaimed_revision_count",
                    str(old_count + len(selected)),
                )

            reclaimed_bytes = 0
            for content_hash in sorted(freed_hashes):
                path = self._blob_path(content_hash)
                try:
                    size = path.stat().st_size
                    path.unlink()
                    reclaimed_bytes += size
                except FileNotFoundError:
                    pass
            with self._transaction(conn):
                old_bytes = int(self._meta_get(conn, "reclaimed_bytes"))
                self._meta_set(conn, "reclaimed_bytes", str(old_bytes + reclaimed_bytes))

            after = sum(
                path.stat().st_size for path in self.blob_root.glob("*/*.bin") if path.is_file()
            )
            return {
                "after_bytes": after,
                "before_bytes": before,
                "reclaimed_bytes": reclaimed_bytes,
                "reclaimed_revision_count": len(selected),
            }

    def get_meta(self, key: str, default: Any = None) -> Any:
        with self.lock.read(), self._connection() as conn:
            return _json_loads(self._meta_get(conn, key, ""), default)

    def set_meta(self, key: str, value: Any) -> None:
        with self.lock.write(), self._connection() as conn, self._transaction(conn):
            if isinstance(value, str):
                encoded = value
            else:
                encoded = _json_dumps(value)
            self._meta_set(conn, key, encoded)
