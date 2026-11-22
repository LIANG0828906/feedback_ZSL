from __future__ import annotations

import json
import sqlite3
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .analysis import FrameResult
from .errors import ErrorCode, WorkbenchError, meta_error


@dataclass(frozen=True)
class SnapshotStatus:
    valid: bool
    invalidated: bool
    committed_frames: int
    reason: str | None = None


class CheckpointStore:
    """Single-file SQLite checkpoint with serialized atomic frame commits."""

    SCHEMA_VERSION = "1"

    def __init__(
        self,
        path: str | Path,
        *,
        sample_id: str,
        input_hash: str,
        algorithm_version: str,
    ) -> None:
        self.path = Path(path)
        self.sample_id = sample_id
        self.input_hash = input_hash
        self.algorithm_version = algorithm_version
        self._lock = threading.RLock()
        self.path.parent.mkdir(parents=True, exist_ok=True)
        try:
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
        except sqlite3.Error as exc:
            raise meta_error(f"cannot open checkpoint database: {exc}", "checkpoint") from exc

    def _create_schema(self) -> None:
        with self._lock:
            self._connection.executescript(
                """
                CREATE TABLE IF NOT EXISTS snapshot_meta (
                    key TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                );
                CREATE TABLE IF NOT EXISTS frames (
                    frame_index INTEGER PRIMARY KEY,
                    sequence INTEGER NOT NULL UNIQUE,
                    input_hash TEXT NOT NULL,
                    algorithm_version TEXT NOT NULL,
                    payload TEXT NOT NULL,
                    created_ns INTEGER NOT NULL
                );
                """
            )

    def clear(self) -> None:
        with self._lock:
            try:
                self._connection.execute("BEGIN IMMEDIATE")
                self._connection.execute("DELETE FROM frames")
                self._connection.execute("DELETE FROM snapshot_meta")
                self._connection.execute("COMMIT")
            except sqlite3.Error as exc:
                self._rollback()
                raise meta_error(f"cannot clear checkpoint: {exc}", "checkpoint") from exc

    def prepare(self, total_frames: int) -> SnapshotStatus:
        if total_frames < 0:
            raise ValueError("total_frames must be non-negative")
        with self._lock:
            try:
                self._connection.execute("BEGIN IMMEDIATE")
                meta = {
                    str(key): str(value)
                    for key, value in self._connection.execute(
                        "SELECT key, value FROM snapshot_meta"
                    )
                }
                frame_count = int(
                    self._connection.execute(
                        "SELECT COUNT(*) FROM frames"
                    ).fetchone()[0]
                )
                expected = {
                    "schema_version": self.SCHEMA_VERSION,
                    "sample_id": self.sample_id,
                    "input_hash": self.input_hash,
                    "algorithm_version": self.algorithm_version,
                }
                reason: str | None = None
                if frame_count > 0 and not meta:
                    reason = "frames_without_snapshot_metadata"
                if meta:
                    for key, expected_value in expected.items():
                        if meta.get(key) != expected_value:
                            reason = f"{key}_mismatch"
                            break
                if reason is None and meta:
                    rows = self._connection.execute(
                        "SELECT frame_index, sequence, input_hash, algorithm_version "
                        "FROM frames ORDER BY frame_index"
                    ).fetchall()
                    if len(rows) != frame_count:
                        reason = "frame_count_mismatch"
                    else:
                        for position, row in enumerate(rows):
                            if row[0] != position or row[1] != position:
                                reason = "frame_sequence_mismatch"
                                break
                            if row[2] != self.input_hash:
                                reason = "frame_input_hash_mismatch"
                                break
                            if row[3] != self.algorithm_version:
                                reason = "frame_algorithm_version_mismatch"
                                break
                if reason is None and frame_count > total_frames:
                    reason = "frame_count_exceeds_current_input"

                invalidated = reason is not None
                if invalidated:
                    self._connection.execute("DELETE FROM frames")
                    self._connection.execute("DELETE FROM snapshot_meta")
                    frame_count = 0

                stored = dict(expected)
                stored["total_frames"] = str(total_frames)
                stored["committed_frames"] = str(frame_count)
                for key, value in stored.items():
                    self._connection.execute(
                        "INSERT OR REPLACE INTO snapshot_meta(key, value) VALUES (?, ?)",
                        (key, value),
                    )
                self._connection.execute("COMMIT")
                return SnapshotStatus(
                    valid=not invalidated,
                    invalidated=invalidated,
                    committed_frames=frame_count,
                    reason=reason,
                )
            except sqlite3.Error as exc:
                self._rollback()
                raise meta_error(f"cannot prepare checkpoint: {exc}", "checkpoint") from exc

    def load_frames(self) -> tuple[FrameResult, ...]:
        with self._lock:
            try:
                rows = self._connection.execute(
                    "SELECT frame_index, payload FROM frames ORDER BY sequence"
                ).fetchall()
            except sqlite3.Error as exc:
                raise meta_error(f"cannot read checkpoint frames: {exc}", "checkpoint") from exc
            results: list[FrameResult] = []
            for expected, row in enumerate(rows):
                frame_index = int(row[0])
                if frame_index != expected:
                    raise WorkbenchError(
                        ErrorCode.STATE,
                        "checkpoint frame sequence is not contiguous",
                        frame_index=frame_index,
                        details={"expected": expected},
                    )
                try:
                    payload = json.loads(row[1])
                except json.JSONDecodeError as exc:
                    raise meta_error(
                        "checkpoint frame payload is invalid JSON",
                        "checkpoint.frames",
                        frame_index=frame_index,
                    ) from exc
                result = FrameResult.from_dict(payload)
                if result.input_hash != self.input_hash:
                    raise meta_error(
                        "checkpoint frame input hash does not match the snapshot",
                        "checkpoint.input_hash",
                        frame_index=frame_index,
                    )
                if result.algorithm_version != self.algorithm_version:
                    raise meta_error(
                        "checkpoint frame algorithm version does not match",
                        "checkpoint.algorithm_version",
                        frame_index=frame_index,
                    )
                results.append(result)
            return tuple(results)

    def save_frame(self, result: FrameResult) -> None:
        with self._lock:
            if result.input_hash != self.input_hash:
                raise meta_error(
                    "frame input hash does not match checkpoint snapshot",
                    "input_hash",
                    frame_index=result.frame_index,
                )
            if result.algorithm_version != self.algorithm_version:
                raise meta_error(
                    "frame algorithm version does not match checkpoint snapshot",
                    "algorithm_version",
                    frame_index=result.frame_index,
                )
            payload = json.dumps(
                result.to_dict(),
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
                allow_nan=False,
            )
            try:
                self._connection.execute("BEGIN IMMEDIATE")
                row = self._connection.execute(
                    "SELECT COUNT(*), COALESCE(MAX(sequence), -1) FROM frames"
                ).fetchone()
                count = int(row[0])
                last_sequence = int(row[1])
                if result.frame_index != count:
                    raise WorkbenchError(
                        ErrorCode.STATE,
                        "checkpoint writes must follow stable frame order",
                        frame_index=result.frame_index,
                        details={"expected": count},
                    )
                if last_sequence != count - 1:
                    raise WorkbenchError(
                        ErrorCode.STATE,
                        "checkpoint sequence has a gap",
                        frame_index=result.frame_index,
                        details={"last_sequence": last_sequence, "count": count},
                    )
                self._connection.execute(
                    "INSERT INTO frames("
                    "frame_index, sequence, input_hash, algorithm_version, payload, created_ns"
                    ") VALUES (?, ?, ?, ?, ?, ?)",
                    (
                        result.frame_index,
                        count,
                        self.input_hash,
                        self.algorithm_version,
                        payload,
                        __import__("time").time_ns(),
                    ),
                )
                self._connection.execute(
                    "INSERT OR REPLACE INTO snapshot_meta(key, value) VALUES (?, ?)",
                    ("committed_frames", str(count + 1)),
                )
                self._connection.execute("COMMIT")
            except WorkbenchError:
                self._rollback()
                raise
            except (sqlite3.Error, ValueError) as exc:
                self._rollback()
                raise meta_error(
                    f"cannot commit frame checkpoint: {exc}",
                    "checkpoint",
                    frame_index=result.frame_index,
                ) from exc

    def snapshot_meta(self) -> dict[str, str]:
        with self._lock:
            try:
                return {
                    str(key): str(value)
                    for key, value in self._connection.execute(
                        "SELECT key, value FROM snapshot_meta"
                    )
                }
            except sqlite3.Error as exc:
                raise meta_error(f"cannot read checkpoint metadata: {exc}", "checkpoint") from exc

    def close(self) -> None:
        with self._lock:
            try:
                self._connection.close()
            except sqlite3.Error:
                pass

    def __enter__(self) -> "CheckpointStore":
        return self

    def __exit__(self, *_: Any) -> None:
        self.close()

    def _rollback(self) -> None:
        try:
            self._connection.execute("ROLLBACK")
        except sqlite3.Error:
            pass
