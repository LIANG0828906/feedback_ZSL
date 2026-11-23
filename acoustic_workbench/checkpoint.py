"""Atomic, serialized SQLite checkpoint persistence."""

from __future__ import annotations

import hashlib
import json
import sqlite3
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from .codes import E_FRAME, E_IO, E_META
from .errors import error
from .models import FrameEstimate, canonical_json


@dataclass
class Snapshot:
    valid: bool
    reason: str
    committed_records: list[dict[str, Any]] = field(default_factory=list)
    meta: dict[str, Any] = field(default_factory=dict)

    @property
    def committed_count(self) -> int:
        return len(self.committed_records)

    def estimates(self) -> list[FrameEstimate]:
        return [FrameEstimate.from_dict(record["estimate"]) for record in self.committed_records]


def _hash_payload(payload: str) -> str:
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def remove_sqlite_checkpoint(path: str | Path) -> None:
    """Remove a checkpoint and its WAL sidecars as one logical operation."""

    target = Path(path)
    for candidate in (
        target,
        Path(f"{target}-wal"),
        Path(f"{target}-shm"),
    ):
        candidate.unlink(missing_ok=True)


class CheckpointStore:
    """One connection plus one lock makes commits ordered and non-interleaved."""

    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._conn = sqlite3.connect(self.path, timeout=30.0, check_same_thread=False)
        self._conn.row_factory = sqlite3.Row
        with self._lock:
            self._conn.execute("PRAGMA journal_mode=WAL")
            self._conn.execute("PRAGMA synchronous=FULL")
            self._conn.execute("PRAGMA foreign_keys=ON")
            self._conn.executescript(
                """
                CREATE TABLE IF NOT EXISTS meta (
                    key TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                );
                CREATE TABLE IF NOT EXISTS frames (
                    frame_index INTEGER PRIMARY KEY,
                    sequence INTEGER NOT NULL UNIQUE,
                    input_hash TEXT NOT NULL,
                    algorithm_version TEXT NOT NULL,
                    payload TEXT NOT NULL,
                    payload_hash TEXT NOT NULL,
                    committed_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
                );
                """
            )
            self._conn.commit()

    def initialize(self, meta: dict[str, Any]) -> None:
        encoded = {key: canonical_json(value) for key, value in meta.items()}
        identity_keys = {
            "schema_version",
            "input_hash",
            "algorithm_version",
            "sample_id",
            "total_frames",
        }
        with self._lock:
            try:
                self._conn.execute("BEGIN IMMEDIATE")
                existing_rows = self._conn.execute("SELECT key, value FROM meta").fetchall()
                existing = {row["key"]: row["value"] for row in existing_rows}
                if existing and any(
                    key in existing and existing[key] != encoded.get(key)
                    for key in identity_keys
                ):
                    self._invalidate_locked("checkpoint identity metadata changed")
                for key, value in encoded.items():
                    self._conn.execute(
                        "INSERT INTO meta(key, value) VALUES(?, ?) "
                        "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                        (key, value),
                    )
                self._conn.commit()
            except Exception:
                self._conn.rollback()
                raise

    def read_meta(self) -> dict[str, Any]:
        with self._lock:
            return self._read_meta_locked()

    def committed_count(self) -> int:
        with self._lock:
            return int(self._conn.execute("SELECT COUNT(*) FROM frames").fetchone()[0])

    def _read_meta_locked(self) -> dict[str, Any]:
        rows = self._conn.execute("SELECT key, value FROM meta").fetchall()
        return {row["key"]: json.loads(row["value"]) for row in rows}

    def _invalidate_locked(self, reason: str) -> None:
        self._conn.execute("DELETE FROM frames")
        self._conn.execute("DELETE FROM meta")
        self._conn.execute(
            "CREATE TABLE IF NOT EXISTS invalidation_log "
            "(id INTEGER PRIMARY KEY AUTOINCREMENT, reason TEXT NOT NULL, "
            "created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)"
        )
        self._conn.execute("INSERT INTO invalidation_log(reason) VALUES(?)", (reason,))
        self._conn.commit()

    def load_snapshot(
        self,
        *,
        input_hash: str,
        algorithm_version: str,
        total_frames: int,
    ) -> Snapshot:
        with self._lock:
            try:
                self._conn.execute("BEGIN IMMEDIATE")
                meta = self._read_meta_locked()
                if not meta:
                    self._conn.commit()
                    return Snapshot(valid=False, reason="empty checkpoint")
                if meta.get("input_hash") != input_hash:
                    self._invalidate_locked("input hash mismatch")
                    return Snapshot(valid=False, reason="input hash mismatch")
                if meta.get("algorithm_version") != algorithm_version:
                    self._invalidate_locked("algorithm version mismatch")
                    return Snapshot(valid=False, reason="algorithm version mismatch")
                if int(meta.get("total_frames", -1)) != total_frames:
                    self._invalidate_locked("frame count mismatch")
                    return Snapshot(valid=False, reason="frame count mismatch")
                rows = self._conn.execute(
                    "SELECT frame_index, sequence, input_hash, algorithm_version, "
                    "payload, payload_hash FROM frames ORDER BY sequence"
                ).fetchall()
                records: list[dict[str, Any]] = []
                for expected_sequence, row in enumerate(rows):
                    if row["sequence"] != expected_sequence:
                        self._invalidate_locked("non-contiguous checkpoint sequence")
                        return Snapshot(valid=False, reason="non-contiguous checkpoint sequence")
                    if row["frame_index"] != expected_sequence:
                        self._invalidate_locked("non-contiguous checkpoint frame index")
                        return Snapshot(valid=False, reason="non-contiguous checkpoint frame index")
                    if row["input_hash"] != input_hash:
                        self._invalidate_locked("per-frame input hash mismatch")
                        return Snapshot(valid=False, reason="per-frame input hash mismatch")
                    if row["algorithm_version"] != algorithm_version:
                        self._invalidate_locked("per-frame algorithm version mismatch")
                        return Snapshot(valid=False, reason="per-frame algorithm version mismatch")
                    if _hash_payload(row["payload"]) != row["payload_hash"]:
                        self._invalidate_locked("checkpoint payload digest mismatch")
                        return Snapshot(valid=False, reason="checkpoint payload digest mismatch")
                    payload = json.loads(row["payload"])
                    if (
                        payload.get("estimate", {}).get("algorithm_version")
                        != algorithm_version
                    ):
                        self._invalidate_locked(
                            "per-frame payload algorithm version mismatch"
                        )
                        return Snapshot(
                            valid=False,
                            reason="per-frame payload algorithm version mismatch",
                        )
                    records.append(
                        {
                            "frame_index": int(row["frame_index"]),
                            "sequence": int(row["sequence"]),
                            "input_hash": row["input_hash"],
                            "algorithm_version": row["algorithm_version"],
                            "estimate": payload["estimate"],
                            "render_stats": payload.get("render_stats"),
                        }
                    )
                self._conn.commit()
                return Snapshot(valid=True, reason="valid", committed_records=records, meta=meta)
            except Exception as exc:
                self._conn.rollback()
                if isinstance(exc, (json.JSONDecodeError, KeyError, TypeError, ValueError)):
                    self._invalidate_locked(f"corrupt checkpoint payload: {exc}")
                    return Snapshot(
                        valid=False,
                        reason=f"corrupt checkpoint payload: {exc}",
                    )
                raise error(
                    E_IO,
                    f"could not load checkpoint: {exc}",
                    stage="checkpoint.load",
                    context={"path": str(self.path)},
                ) from exc

    def commit_frame(
        self,
        *,
        frame_index: int,
        sequence: int,
        estimate: FrameEstimate,
        input_hash: str,
        algorithm_version: str,
        render_stats: dict[str, Any] | None = None,
    ) -> None:
        payload = canonical_json(
            {
                "estimate": estimate.to_dict(),
                "render_stats": render_stats,
            }
        )
        with self._lock:
            try:
                self._conn.execute("BEGIN IMMEDIATE")
                meta = self._read_meta_locked()
                if not meta:
                    raise error(
                        E_META,
                        "checkpoint metadata must be initialized before committing",
                        stage="checkpoint.commit",
                        frame_index=frame_index,
                    )
                if meta.get("input_hash") != input_hash:
                    raise error(
                        E_META,
                        "checkpoint input hash changed during the run",
                        stage="checkpoint.commit",
                        frame_index=frame_index,
                    )
                if meta.get("algorithm_version") != algorithm_version:
                    raise error(
                        E_META,
                        "checkpoint algorithm version changed during the run",
                        stage="checkpoint.commit",
                        frame_index=frame_index,
                    )
                count = int(
                    self._conn.execute("SELECT COUNT(*) FROM frames").fetchone()[0]
                )
                if count != sequence or sequence != frame_index:
                    raise error(
                        E_FRAME,
                        "checkpoint commits must be contiguous and in frame order",
                        stage="checkpoint.commit",
                        frame_index=frame_index,
                        context={"expected_sequence": count, "provided_sequence": sequence},
                    )
                self._conn.execute(
                    "INSERT INTO frames(frame_index, sequence, input_hash, "
                    "algorithm_version, payload, payload_hash) VALUES(?, ?, ?, ?, ?, ?)",
                    (
                        frame_index,
                        sequence,
                        input_hash,
                        algorithm_version,
                        payload,
                        _hash_payload(payload),
                    ),
                )
                self._conn.commit()
            except Exception:
                self._conn.rollback()
                raise

    def commit_many(
        self,
        records: Iterable[tuple[int, FrameEstimate, dict[str, Any] | None]],
        *,
        start_sequence: int,
        input_hash: str,
        algorithm_version: str,
    ) -> None:
        for offset, (frame_index, estimate, render_stats) in enumerate(records):
            self.commit_frame(
                frame_index=frame_index,
                sequence=start_sequence + offset,
                estimate=estimate,
                input_hash=input_hash,
                algorithm_version=algorithm_version,
                render_stats=render_stats,
            )

    def invalidate_all(self, reason: str) -> None:
        with self._lock:
            try:
                self._conn.execute("BEGIN IMMEDIATE")
                self._invalidate_locked(reason)
            except Exception:
                self._conn.rollback()
                raise

    def close(self) -> None:
        with self._lock:
            self._conn.close()

    def __enter__(self) -> "CheckpointStore":
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.close()
