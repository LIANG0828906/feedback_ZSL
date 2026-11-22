from __future__ import annotations

import hashlib
import io
import json
import sqlite3
import threading
import zlib
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .config import CHECKPOINT_SCHEMA_VERSION, AlgorithmInfo
from .errors import E_DECODE, E_STATE, SyncError


@dataclass(frozen=True)
class SegmentCheckpoint:
    channel_id: str
    segment_index: int
    start: int
    stop: int
    params_json: str
    content_hash: str
    params_version: str
    algorithm_version: str
    output: np.ndarray
    output_hash: str
    state: str = "COMMITTED"


@dataclass(frozen=True)
class SnapshotStatus:
    session_id: str
    invalidated: bool
    reason: str | None
    committed_count: int
    schema_version: int
    algorithm_version: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "session_id": self.session_id,
            "invalidated": self.invalidated,
            "reason": self.reason,
            "committed_count": self.committed_count,
            "schema_version": self.schema_version,
            "algorithm_version": self.algorithm_version,
        }


def _encode_array(array: np.ndarray) -> tuple[bytes, str]:
    buffer = io.BytesIO()
    np.save(buffer, np.ascontiguousarray(array, dtype=np.float64), allow_pickle=False)
    raw = buffer.getvalue()
    digest = hashlib.sha256(raw).hexdigest()
    return zlib.compress(raw, level=6), digest


def _decode_array(compressed: bytes, expected_hash: str) -> np.ndarray:
    try:
        raw = zlib.decompress(compressed)
        actual_hash = hashlib.sha256(raw).hexdigest()
        if actual_hash != expected_hash:
            raise ValueError("checkpoint array digest mismatch")
        array = np.load(io.BytesIO(raw), allow_pickle=False)
    except (ValueError, OSError, zlib.error, EOFError) as exc:
        raise SyncError(E_DECODE, "cannot decode checkpoint segment output", details={"error": str(exc)}) from exc
    if not isinstance(array, np.ndarray) or array.ndim != 1 or not np.issubdtype(array.dtype, np.floating):
        raise SyncError(E_DECODE, "checkpoint output must be a one-dimensional floating array")
    return array


class CheckpointStore:
    """Serialized SQLite checkpoint store.

    Every commit is a single BEGIN IMMEDIATE transaction protected by a process-local
    lock. SQLite supplies cross-process serialization. Format or algorithm-version
    mismatch invalidates the whole snapshot before any segment is exposed.
    """

    def __init__(self, path: str | Path, algorithm: AlgorithmInfo | None = None) -> None:
        self.path = Path(path)
        self.algorithm = algorithm or AlgorithmInfo()
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._connection = sqlite3.connect(
            str(self.path),
            timeout=30.0,
            isolation_level=None,
            check_same_thread=False,
        )
        self._connection.execute("PRAGMA journal_mode=WAL")
        self._connection.execute("PRAGMA synchronous=FULL")
        self._connection.execute("PRAGMA busy_timeout=30000")
        self._create_schema()
        self._status: SnapshotStatus | None = None

    def _create_schema(self) -> None:
        with self._lock:
            self._connection.execute(
                """
                CREATE TABLE IF NOT EXISTS meta (
                    key TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                )
                """
            )
            self._connection.execute(
                """
                CREATE TABLE IF NOT EXISTS segments (
                    session_id TEXT NOT NULL,
                    channel_id TEXT NOT NULL,
                    segment_index INTEGER NOT NULL,
                    start INTEGER NOT NULL,
                    stop INTEGER NOT NULL,
                    state TEXT NOT NULL,
                    params_json TEXT NOT NULL,
                    content_hash TEXT NOT NULL,
                    params_version TEXT NOT NULL,
                    algorithm_version TEXT NOT NULL,
                    schema_version INTEGER NOT NULL,
                    output_hash TEXT NOT NULL,
                    output_blob BLOB NOT NULL,
                    PRIMARY KEY (session_id, channel_id, segment_index)
                )
                """
            )
            self._connection.execute(
                "CREATE INDEX IF NOT EXISTS segments_order ON segments(session_id, segment_index, channel_id)"
            )

    def _meta(self, key: str) -> str | None:
        row = self._connection.execute("SELECT value FROM meta WHERE key=?", (key,)).fetchone()
        return None if row is None else str(row[0])

    def _set_meta(self, key: str, value: str) -> None:
        self._connection.execute(
            "INSERT INTO meta(key, value) VALUES(?, ?) "
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (key, value),
        )

    def _wipe_segments(self) -> None:
        self._connection.execute("DELETE FROM segments")

    def load_snapshot(self, session_id: str) -> tuple[SnapshotStatus, dict[str, list[SegmentCheckpoint]]]:
        with self._lock:
            self._connection.execute("BEGIN IMMEDIATE")
            try:
                reason: str | None = None
                meta_schema = self._meta("checkpoint_schema")
                meta_algorithm = self._meta("algorithm_version")
                meta_session = self._meta("session_id")
                expected_schema = str(self.algorithm.checkpoint_schema)
                if meta_schema is not None and meta_schema != expected_schema:
                    reason = f"checkpoint schema {meta_schema} != {expected_schema}"
                elif meta_algorithm is not None and meta_algorithm != self.algorithm.version:
                    reason = f"algorithm {meta_algorithm} != {self.algorithm.version}"
                elif meta_session is not None and meta_session != session_id:
                    reason = f"session {meta_session} != {session_id}"

                if reason is None:
                    bad = self._connection.execute(
                        """
                        SELECT COUNT(*) FROM segments
                        WHERE schema_version != ? OR algorithm_version != ? OR session_id != ?
                        """,
                        (self.algorithm.checkpoint_schema, self.algorithm.version, session_id),
                    ).fetchone()[0]
                    if int(bad) > 0:
                        reason = "segment rows contain mismatched schema, algorithm or session versions"

                if reason is not None:
                    self._wipe_segments()
                self._set_meta("checkpoint_schema", expected_schema)
                self._set_meta("algorithm_version", self.algorithm.version)
                self._set_meta("session_id", session_id)
                rows = self._connection.execute(
                    """
                    SELECT channel_id, segment_index, start, stop, state, params_json,
                           content_hash, params_version, algorithm_version, schema_version,
                           output_hash, output_blob
                    FROM segments
                    WHERE session_id=?
                    ORDER BY segment_index, channel_id
                    """,
                    (session_id,),
                ).fetchall()
                self._connection.execute("COMMIT")
            except Exception:
                self._connection.execute("ROLLBACK")
                raise

            grouped: dict[str, list[SegmentCheckpoint]] = {}
            try:
                for row in rows:
                    output = _decode_array(row[11], row[10])
                    if output.shape[0] != int(row[3]) - int(row[2]):
                        raise SyncError(E_DECODE, "checkpoint output length does not match segment range")
                    checkpoint = SegmentCheckpoint(
                        channel_id=str(row[0]),
                        segment_index=int(row[1]),
                        start=int(row[2]),
                        stop=int(row[3]),
                        state=str(row[4]),
                        params_json=str(row[5]),
                        content_hash=str(row[6]),
                        params_version=str(row[7]),
                        algorithm_version=str(row[8]),
                        output=output,
                        output_hash=str(row[10]),
                    )
                    grouped.setdefault(checkpoint.channel_id, []).append(checkpoint)
            except SyncError:
                with self._lock:
                    self._connection.execute("BEGIN IMMEDIATE")
                    try:
                        self._wipe_segments()
                        self._connection.execute("COMMIT")
                    except Exception:
                        self._connection.execute("ROLLBACK")
                        raise
                raise
            status = SnapshotStatus(
                session_id=session_id,
                invalidated=reason is not None,
                reason=reason,
                committed_count=sum(len(items) for items in grouped.values()),
                schema_version=self.algorithm.checkpoint_schema,
                algorithm_version=self.algorithm.version,
            )
            self._status = status
            return status, grouped

    def _assert_version(self) -> None:
        if self._status is None:
            raise SyncError(E_STATE, "snapshot must be loaded before committing")

    def commit_segment(self, session_id: str, checkpoint: SegmentCheckpoint) -> None:
        self.commit_segment_batch(session_id, [checkpoint])

    def commit_segment_batch(self, session_id: str, checkpoints: Iterable[SegmentCheckpoint]) -> None:
        items = list(checkpoints)
        if not items:
            return
        with self._lock:
            self._assert_version()
            encoded = [(item, *_encode_array(item.output)) for item in items]
            self._connection.execute("BEGIN IMMEDIATE")
            try:
                meta_schema = self._meta("checkpoint_schema")
                meta_algorithm = self._meta("algorithm_version")
                meta_session = self._meta("session_id")
                if (
                    meta_schema != str(self.algorithm.checkpoint_schema)
                    or meta_algorithm != self.algorithm.version
                    or meta_session != session_id
                ):
                    raise SyncError(E_STATE, "snapshot version changed while committing")
                for item, blob, output_hash in encoded:
                    if item.algorithm_version != self.algorithm.version:
                        raise SyncError(E_STATE, "segment algorithm version mismatch")
                    self._connection.execute(
                        """
                        INSERT INTO segments(
                            session_id, channel_id, segment_index, start, stop, state,
                            params_json, content_hash, params_version, algorithm_version,
                            schema_version, output_hash, output_blob
                        ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                        ON CONFLICT(session_id, channel_id, segment_index) DO UPDATE SET
                            start=excluded.start,
                            stop=excluded.stop,
                            state=excluded.state,
                            params_json=excluded.params_json,
                            content_hash=excluded.content_hash,
                            params_version=excluded.params_version,
                            algorithm_version=excluded.algorithm_version,
                            schema_version=excluded.schema_version,
                            output_hash=excluded.output_hash,
                            output_blob=excluded.output_blob
                        """,
                        (
                            session_id,
                            item.channel_id,
                            item.segment_index,
                            item.start,
                            item.stop,
                            item.state,
                            item.params_json,
                            item.content_hash,
                            item.params_version,
                            item.algorithm_version,
                            CHECKPOINT_SCHEMA_VERSION,
                            output_hash,
                            blob,
                        ),
                    )
                self._connection.execute("COMMIT")
            except Exception:
                self._connection.execute("ROLLBACK")
                raise

    def committed_for_channel(self, grouped: dict[str, list[SegmentCheckpoint]], channel_id: str) -> dict[int, SegmentCheckpoint]:
        return {item.segment_index: item for item in grouped.get(channel_id, []) if item.state == "COMMITTED"}

    def close(self) -> None:
        with self._lock:
            self._connection.close()

    def __enter__(self) -> "CheckpointStore":
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        self.close()


def params_payload(
    *,
    model: dict[str, Any],
    start: int,
    stop: int,
    interpolation_order: int,
    params_version: str,
) -> str:
    return json.dumps(
        {
            "model": model,
            "start": start,
            "stop": stop,
            "interpolation_order": interpolation_order,
            "params_version": params_version,
        },
        sort_keys=True,
        separators=(",", ":"),
    )

