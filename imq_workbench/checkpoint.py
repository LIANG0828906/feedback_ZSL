"""Atomic, serialized SQLite checkpoints with whole-snapshot validation."""

from __future__ import annotations

import hashlib
import json
import sqlite3
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .compression import BlockGeometry, block_input_hash, partition_image
from .errors import ALGORITHM_VERSION, WorkbenchError
from .util import bytes_hash, json_bytes, npz_bytes, npz_load

SCHEMA_VERSION = 1

_SCHEMA = """
CREATE TABLE IF NOT EXISTS snapshots (
    snapshot_id TEXT PRIMARY KEY,
    image_key TEXT NOT NULL,
    image_hash TEXT NOT NULL,
    algorithm_version TEXT NOT NULL,
    block_size INTEGER NOT NULL,
    shape_json TEXT NOT NULL,
    grid_json TEXT NOT NULL,
    total_blocks INTEGER NOT NULL,
    config_json TEXT NOT NULL,
    complete INTEGER NOT NULL DEFAULT 0,
    invalidated_reason TEXT,
    created_utc TEXT NOT NULL,
    updated_utc TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_snapshots_image ON snapshots(image_key, block_size, updated_utc);
CREATE TABLE IF NOT EXISTS blocks (
    snapshot_id TEXT NOT NULL,
    block_index INTEGER NOT NULL,
    state TEXT NOT NULL,
    geometry_json TEXT NOT NULL,
    params_json TEXT NOT NULL,
    input_hash TEXT NOT NULL,
    quantized_blob BLOB NOT NULL,
    reconstruction_blob BLOB NOT NULL,
    evaluation_blob BLOB NOT NULL,
    stats_json TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    committed_utc TEXT NOT NULL,
    PRIMARY KEY(snapshot_id, block_index),
    FOREIGN KEY(snapshot_id) REFERENCES snapshots(snapshot_id) ON DELETE CASCADE
);
"""


@dataclass
class StoredBlock:
    index: int
    geometry: BlockGeometry
    params: dict[str, Any]
    input_hash: str
    quantized: np.ndarray
    reconstruction: np.ndarray
    ssim_slice: np.ndarray
    error_slice: np.ndarray
    edge_slice: np.ndarray
    stats: dict[str, Any]
    payload_hash: str

    def as_evaluation_entry(self) -> dict[str, Any]:
        return {
            **self.stats,
            "ssim_slice": self.ssim_slice,
            "error_slice": self.error_slice,
            "edge_slice": self.edge_slice,
        }


@dataclass
class SnapshotLoad:
    snapshot_id: str | None
    valid: bool
    invalidated_reason: str | None
    blocks: dict[int, StoredBlock]


def snapshot_identity(
    image_key: str,
    image_hash: str,
    block_size: int,
    shape: tuple[int, ...],
    algorithm_version: str = ALGORITHM_VERSION,
) -> str:
    material = json_bytes(
        {
            "image_key": image_key,
            "image_hash": image_hash,
            "block_size": block_size,
            "shape": list(shape),
            "algorithm_version": algorithm_version,
        }
    )
    return hashlib.sha256(material).hexdigest()


class CheckpointStore:
    """SQLite store whose public writes are serialized by one process lock."""

    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._connection = sqlite3.connect(
            str(self.path),
            timeout=30.0,
            check_same_thread=False,
            isolation_level=None,
        )
        with self._lock:
            self._connection.execute("PRAGMA journal_mode=WAL")
            self._connection.execute("PRAGMA synchronous=FULL")
            self._connection.execute("PRAGMA foreign_keys=ON")
            self._connection.executescript(_SCHEMA)

    def close(self) -> None:
        with self._lock:
            self._connection.close()

    def _now(self) -> str:
        from datetime import datetime, timezone

        return datetime.now(timezone.utc).isoformat()

    def _execute(self, sql: str, parameters: tuple[Any, ...] = ()) -> sqlite3.Cursor:
        try:
            return self._connection.execute(sql, parameters)
        except sqlite3.Error as exc:
            raise WorkbenchError("E_IO", "checkpoint database operation failed", {"path": str(self.path), "reason": str(exc)}) from exc

    def find_snapshot(self, image_key: str, block_size: int) -> str | None:
        with self._lock:
            row = self._execute(
                "SELECT snapshot_id FROM snapshots WHERE image_key=? AND block_size=? ORDER BY updated_utc DESC LIMIT 1",
                (image_key, block_size),
            ).fetchone()
        return None if row is None else str(row[0])

    def has_committed_snapshot_with_other_block_size(self, image_key: str, block_size: int) -> bool:
        with self._lock:
            row = self._execute(
                """
                SELECT 1 FROM snapshots AS s
                WHERE s.image_key=? AND s.block_size<>?
                  AND EXISTS (
                    SELECT 1 FROM blocks AS b
                    WHERE b.snapshot_id=s.snapshot_id AND b.state='committed'
                  )
                LIMIT 1
                """,
                (image_key, block_size),
            ).fetchone()
        return row is not None

    def create_snapshot(
        self,
        *,
        snapshot_id: str,
        image_key: str,
        image_hash: str,
        block_size: int,
        shape: tuple[int, ...],
        grid: tuple[int, int],
        total_blocks: int,
        config: dict[str, Any],
        algorithm_version: str = ALGORITHM_VERSION,
        replace: bool = False,
    ) -> None:
        now = self._now()
        with self._lock:
            try:
                self._connection.execute("BEGIN IMMEDIATE")
                if replace:
                    self._connection.execute("DELETE FROM blocks WHERE snapshot_id=?", (snapshot_id,))
                    self._connection.execute("DELETE FROM snapshots WHERE snapshot_id=?", (snapshot_id,))
                self._connection.execute(
                    """
                    INSERT INTO snapshots(
                        snapshot_id, image_key, image_hash, algorithm_version, block_size,
                        shape_json, grid_json, total_blocks, config_json, complete,
                        created_utc, updated_utc
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
                    """,
                    (
                        snapshot_id,
                        image_key,
                        image_hash,
                        algorithm_version,
                        block_size,
                        json.dumps(list(shape)),
                        json.dumps(list(grid)),
                        total_blocks,
                        json.dumps(config, sort_keys=True),
                        now,
                        now,
                    ),
                )
                self._connection.execute("COMMIT")
            except sqlite3.IntegrityError as exc:
                self._connection.execute("ROLLBACK")
                raise WorkbenchError("E_STATE", "snapshot already exists", {"snapshot_id": snapshot_id}) from exc
            except sqlite3.Error as exc:
                self._connection.execute("ROLLBACK")
                raise WorkbenchError("E_IO", "cannot create checkpoint snapshot", {"reason": str(exc)}) from exc

    def invalidate_snapshot(self, snapshot_id: str, reason: str) -> None:
        with self._lock:
            try:
                self._connection.execute("BEGIN IMMEDIATE")
                self._connection.execute("UPDATE snapshots SET invalidated_reason=?, complete=0, updated_utc=? WHERE snapshot_id=?", (reason, self._now(), snapshot_id))
                self._connection.execute("DELETE FROM blocks WHERE snapshot_id=?", (snapshot_id,))
                self._connection.execute("COMMIT")
            except sqlite3.Error as exc:
                self._connection.execute("ROLLBACK")
                raise WorkbenchError("E_IO", "cannot invalidate checkpoint snapshot", {"reason": str(exc)}) from exc

    def delete_snapshot(self, snapshot_id: str) -> None:
        with self._lock:
            try:
                self._connection.execute("BEGIN IMMEDIATE")
                self._connection.execute("DELETE FROM blocks WHERE snapshot_id=?", (snapshot_id,))
                self._connection.execute("DELETE FROM snapshots WHERE snapshot_id=?", (snapshot_id,))
                self._connection.execute("COMMIT")
            except sqlite3.Error as exc:
                self._connection.execute("ROLLBACK")
                raise WorkbenchError("E_IO", "cannot clear checkpoint snapshot", {"reason": str(exc)}) from exc

    def load_compatible(
        self,
        *,
        image: np.ndarray,
        image_key: str,
        image_hash: str,
        block_size: int,
        shape: tuple[int, ...],
        algorithm_version: str = ALGORITHM_VERSION,
    ) -> SnapshotLoad:
        snapshot_id = self.find_snapshot(image_key, block_size)
        if snapshot_id is None:
            return SnapshotLoad(None, False, None, {})

        with self._lock:
            row = self._execute(
                "SELECT snapshot_id, image_hash, algorithm_version, shape_json, grid_json, total_blocks FROM snapshots WHERE snapshot_id=?",
                (snapshot_id,),
            ).fetchone()
            if row is None:
                return SnapshotLoad(None, False, None, {})
            stored_hash = str(row[1])
            stored_version = str(row[2])
            stored_shape = tuple(json.loads(row[3]))
            stored_grid = tuple(json.loads(row[4]))
            total_blocks = int(row[5])

            reason = None
            if stored_hash != image_hash:
                reason = "image_content_hash_mismatch"
            elif stored_version != algorithm_version:
                reason = "algorithm_version_mismatch"
            elif stored_shape != tuple(shape):
                reason = "image_shape_mismatch"

            if reason is not None:
                self.invalidate_snapshot(snapshot_id, reason)
                return SnapshotLoad(snapshot_id, False, reason, {})

            expected_blocks, expected_grid = partition_image(shape[0], shape[1], block_size)
            if tuple(stored_grid) != tuple(expected_grid) or total_blocks != len(expected_blocks):
                self.invalidate_snapshot(snapshot_id, "block_grid_mismatch")
                return SnapshotLoad(snapshot_id, False, "block_grid_mismatch", {})

            rows = self._execute(
                """
                SELECT block_index, state, geometry_json, params_json, input_hash,
                       quantized_blob, reconstruction_blob, evaluation_blob, stats_json, payload_hash
                FROM blocks WHERE snapshot_id=? ORDER BY block_index
                """,
                (snapshot_id,),
            ).fetchall()

            blocks: dict[int, StoredBlock] = {}
            try:
                for block_row in rows:
                    self._decode_row(image, block_row, blocks, expected_blocks, block_size)
            except (WorkbenchError, KeyError, ValueError, TypeError, json.JSONDecodeError):
                self.invalidate_snapshot(snapshot_id, "block_payload_validation_failed")
                return SnapshotLoad(snapshot_id, False, "block_payload_validation_failed", {})

        return SnapshotLoad(snapshot_id, True, None, blocks)

    def _decode_row(
        self,
        image: np.ndarray,
        row: tuple[Any, ...],
        blocks: dict[int, StoredBlock],
        expected_blocks: list[BlockGeometry],
        block_size: int,
    ) -> None:
        block_index = int(row[0])
        if row[1] != "committed":
            raise ValueError("stored block is not committed")
        if block_index < 0 or block_index >= len(expected_blocks):
            raise ValueError("stored block index outside grid")
        geometry = expected_blocks[block_index]
        stored_geometry = json.loads(row[2])
        if stored_geometry != geometry.to_dict():
            raise ValueError("stored geometry mismatch")
        params = json.loads(row[3])
        if int(params.get("block_size", -1)) != block_size:
            raise ValueError("stored block size mismatch")
        if params.get("algorithm_version") != ALGORITHM_VERSION:
            raise ValueError("stored block algorithm version mismatch")
        input_hash = str(row[4])
        if input_hash != block_input_hash(image, geometry):
            raise ValueError("stored block input hash mismatch")
        quantized_blob = bytes(row[5])
        reconstruction_blob = bytes(row[6])
        evaluation_blob = bytes(row[7])
        stats_blob = bytes(row[8])
        payload_hash = str(row[9])
        actual_payload_hash = bytes_hash(
            json_bytes(stored_geometry)
            + json_bytes(params)
            + quantized_blob
            + reconstruction_blob
            + evaluation_blob
            + stats_blob
        )
        if actual_payload_hash != payload_hash:
            raise ValueError("stored block payload hash mismatch")

        quantized = npz_load(quantized_blob)["quantized"]
        reconstruction = npz_load(reconstruction_blob)["reconstruction"]
        evaluation = npz_load(evaluation_blob)
        expected_shape = (geometry.height, geometry.width)
        if reconstruction.shape[:2] != expected_shape:
            raise ValueError("stored reconstruction shape mismatch")
        if quantized.ndim not in (2, 3) or quantized.shape[0] != int(params["block_size"]) or quantized.shape[1] != int(params["block_size"]):
            raise ValueError("stored coefficient shape mismatch")
        if evaluation["ssim_slice"].shape != expected_shape:
            raise ValueError("stored ssim slice shape mismatch")
        if evaluation["error_slice"].shape != expected_shape:
            raise ValueError("stored error slice shape mismatch")
        if evaluation["edge_slice"].shape != expected_shape:
            raise ValueError("stored edge slice shape mismatch")
        blocks[block_index] = StoredBlock(
            index=block_index,
            geometry=geometry,
            params=params,
            input_hash=input_hash,
            quantized=quantized,
            reconstruction=reconstruction,
            ssim_slice=evaluation["ssim_slice"],
            error_slice=evaluation["error_slice"],
            edge_slice=evaluation["edge_slice"],
            stats=json.loads(stats_blob),
            payload_hash=payload_hash,
        )

    def commit_block(self, snapshot_id: str, outcome: Any) -> str:
        if outcome.state != "committed":
            raise WorkbenchError("E_STATE", "only committed blocks may be persisted", {"block_index": outcome.index})
        if outcome.quantized is None or outcome.reconstruction is None:
            raise WorkbenchError("E_STATE", "committed block has no reconstruction payload", {"block_index": outcome.index})
        quantized_blob = npz_bytes(quantized=np.asarray(outcome.quantized, dtype=np.int32))
        reconstruction_blob = npz_bytes(reconstruction=np.asarray(outcome.reconstruction, dtype=np.uint8))
        evaluation_blob = npz_bytes(
            ssim_slice=np.asarray(outcome.ssim_slice, dtype=np.float32),
            error_slice=np.asarray(outcome.error_slice, dtype=np.float32),
            edge_slice=np.asarray(outcome.edge_slice, dtype=np.float32),
        )
        stats_blob = json_bytes(outcome.evaluation_json())
        geometry_blob = json_bytes(outcome.geometry.to_dict())
        params_blob = json_bytes({**outcome.param, "algorithm_version": ALGORITHM_VERSION})
        payload_hash = bytes_hash(
            geometry_blob
            + params_blob
            + quantized_blob
            + reconstruction_blob
            + evaluation_blob
            + stats_blob
        )
        now = self._now()
        with self._lock:
            try:
                self._connection.execute("BEGIN IMMEDIATE")
                self._connection.execute(
                    """
                    INSERT INTO blocks(
                        snapshot_id, block_index, state, geometry_json, params_json, input_hash,
                        quantized_blob, reconstruction_blob, evaluation_blob, stats_json,
                        payload_hash, committed_utc
                    ) VALUES (?, ?, 'committed', ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    ON CONFLICT(snapshot_id, block_index) DO UPDATE SET
                        state=excluded.state,
                        geometry_json=excluded.geometry_json,
                        params_json=excluded.params_json,
                        input_hash=excluded.input_hash,
                        quantized_blob=excluded.quantized_blob,
                        reconstruction_blob=excluded.reconstruction_blob,
                        evaluation_blob=excluded.evaluation_blob,
                        stats_json=excluded.stats_json,
                        payload_hash=excluded.payload_hash,
                        committed_utc=excluded.committed_utc
                    """,
                    (
                        snapshot_id,
                        outcome.index,
                        geometry_blob,
                        params_blob,
                        outcome.input_hash,
                        quantized_blob,
                        reconstruction_blob,
                        evaluation_blob,
                        stats_blob,
                        payload_hash,
                        now,
                    ),
                )
                self._connection.execute("UPDATE snapshots SET updated_utc=? WHERE snapshot_id=?", (now, snapshot_id))
                self._connection.execute("COMMIT")
            except sqlite3.Error as exc:
                self._connection.execute("ROLLBACK")
                raise WorkbenchError("E_IO", "atomic block checkpoint write failed", {"block_index": outcome.index, "reason": str(exc)}) from exc
        return payload_hash

    def count_committed(self, snapshot_id: str) -> int:
        with self._lock:
            row = self._execute(
                "SELECT COUNT(*) FROM blocks WHERE snapshot_id=? AND state='committed'",
                (snapshot_id,),
            ).fetchone()
        return int(row[0])

    def mark_complete(self, snapshot_id: str, complete: bool) -> None:
        with self._lock:
            self._execute(
                "UPDATE snapshots SET complete=?, updated_utc=? WHERE snapshot_id=?",
                (1 if complete else 0, self._now(), snapshot_id),
            )

    def committed_indices(self, snapshot_id: str) -> list[int]:
        with self._lock:
            rows = self._execute(
                "SELECT block_index FROM blocks WHERE snapshot_id=? AND state='committed' ORDER BY block_index",
                (snapshot_id,),
            ).fetchall()
        return [int(row[0]) for row in rows]
