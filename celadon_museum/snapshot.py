"""Atomic tile snapshots and resumable progress manifests."""

from __future__ import annotations

import json
import os
import shutil
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .math3d import sha256_array, sha256_bytes
from .scene import Scene, canonical_json


SNAPSHOT_SCHEMA = "celadon-progress-v1"


def _atomic_write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        os.chmod(path, 0o644)
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def _atomic_save_npy(path: Path, array: np.ndarray) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            np.save(handle, np.ascontiguousarray(array), allow_pickle=False)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        os.chmod(path, 0o644)
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


@dataclass
class TileRecord:
    tile_id: int
    row: int
    col: int
    color_file: str
    depth_file: str
    projection_file: str
    color_digest: str
    depth_digest: str
    projection_digest: str
    stats: dict[str, Any]

    def to_dict(self) -> dict[str, Any]:
        return {
            "tile_id": self.tile_id,
            "row": self.row,
            "col": self.col,
            "color_file": self.color_file,
            "depth_file": self.depth_file,
            "projection_file": self.projection_file,
            "color_digest": self.color_digest,
            "depth_digest": self.depth_digest,
            "projection_digest": self.projection_digest,
            "stats": self.stats,
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> "TileRecord":
        return cls(
            tile_id=int(payload["tile_id"]),
            row=int(payload["row"]),
            col=int(payload["col"]),
            color_file=str(payload["color_file"]),
            depth_file=str(payload["depth_file"]),
            projection_file=str(payload["projection_file"]),
            color_digest=str(payload["color_digest"]),
            depth_digest=str(payload["depth_digest"]),
            projection_digest=str(payload["projection_digest"]),
            stats=dict(payload["stats"]),
        )


@dataclass
class SnapshotLoad:
    state: "SnapshotState | None"
    discarded: bool
    reason: str | None


class SnapshotState:
    def __init__(
        self,
        scene: Scene,
        records: list[TileRecord],
        resume_count: int,
        store: "SnapshotStore",
    ):
        self.scene = scene
        self.records = records
        self.resume_count = resume_count
        self.store = store

    @property
    def completed_ids(self) -> list[int]:
        return [record.tile_id for record in self.records]

    def read_tile(self, record: TileRecord):
        return self.store._read_and_verify_tile(record)


class SnapshotStore:
    def __init__(self, output_dir: str | Path):
        self.output_dir = Path(output_dir)
        self.state_dir = self.output_dir / ".state"
        self.tile_dir = self.state_dir / "tiles"
        self.manifest_path = self.state_dir / "snapshot.json"

    def _empty_manifest(self, scene: Scene, resume_count: int = 0) -> dict[str, Any]:
        tiles_x, tiles_y = scene.tile_grid()
        core = {
            "schema": SNAPSHOT_SCHEMA,
            "scene_digest": scene.digest,
            "image": {
                "width": scene.width,
                "height": scene.height,
                "tile_size": scene.tile_size,
                "tiles_x": tiles_x,
                "tiles_y": tiles_y,
            },
            "resume_count": int(resume_count),
            "completed": [],
        }
        core["payload_digest"] = sha256_bytes(canonical_json(core))
        return core

    def reset(self, scene: Scene, resume_count: int = 0) -> None:
        self.state_dir.mkdir(parents=True, exist_ok=True)
        if self.tile_dir.exists():
            shutil.rmtree(self.tile_dir)
        self.tile_dir.mkdir(parents=True, exist_ok=True)
        self._write_manifest(self._empty_manifest(scene, resume_count=resume_count))

    def discard(self, scene: Scene) -> None:
        if self.state_dir.exists():
            shutil.rmtree(self.state_dir)
        self.reset(scene)

    def _write_manifest(self, manifest: dict[str, Any]) -> None:
        core = dict(manifest)
        core.pop("payload_digest", None)
        core["payload_digest"] = sha256_bytes(canonical_json(core))
        data = json.dumps(
            core,
            ensure_ascii=False,
            sort_keys=True,
            indent=2,
            allow_nan=False,
        ).encode("utf-8")
        _atomic_write_bytes(self.manifest_path, data + b"\n")

    def _read_manifest(self) -> dict[str, Any]:
        raw = self.manifest_path.read_bytes()
        manifest = json.loads(raw.decode("utf-8"))
        if not isinstance(manifest, dict):
            raise ValueError("snapshot root is not an object")
        payload_digest = manifest.get("payload_digest")
        core = dict(manifest)
        core.pop("payload_digest", None)
        if payload_digest != sha256_bytes(canonical_json(core)):
            raise ValueError("snapshot payload digest does not match")
        return manifest

    def load(self, scene: Scene) -> SnapshotLoad:
        if not self.manifest_path.exists():
            self.reset(scene)
            return SnapshotLoad(state=SnapshotState(scene, [], 0, self), discarded=False, reason=None)
        try:
            manifest = self._read_manifest()
            if manifest.get("schema") != SNAPSHOT_SCHEMA:
                raise ValueError("unsupported snapshot schema")
            if manifest.get("scene_digest") != scene.digest:
                raise ValueError("scene digest differs from snapshot")
            tiles_x, tiles_y = scene.tile_grid()
            expected_image = {
                "width": scene.width,
                "height": scene.height,
                "tile_size": scene.tile_size,
                "tiles_x": tiles_x,
                "tiles_y": tiles_y,
            }
            if manifest.get("image") != expected_image:
                raise ValueError("snapshot image geometry differs from scene")
            records: list[TileRecord] = []
            seen: set[int] = set()
            total = tiles_x * tiles_y
            for payload in manifest.get("completed", []):
                record = TileRecord.from_dict(payload)
                if not 0 <= record.tile_id < total or record.tile_id in seen:
                    raise ValueError("snapshot contains an invalid or duplicate tile id")
                expected_row = record.tile_id // tiles_x
                expected_col = record.tile_id % tiles_x
                if record.row != expected_row or record.col != expected_col:
                    raise ValueError("snapshot tile row/column does not match tile id")
                contour = Path(record.color_file)
                depth_path = Path(record.depth_file)
                projection_path = Path(record.projection_file)
                if contour.is_absolute() or depth_path.is_absolute() or projection_path.is_absolute():
                    raise ValueError("snapshot tile paths must be relative")
                color, depth, projection = self._read_and_verify_tile(record)
                expected_shape = (
                    min(scene.tile_size, scene.height - record.row * scene.tile_size),
                    min(scene.tile_size, scene.width - record.col * scene.tile_size),
                )
                if (
                    color.shape[:2] != expected_shape
                    or depth.shape != expected_shape
                    or projection.shape != expected_shape
                ):
                    raise ValueError("tile array shape does not match image geometry")
                seen.add(record.tile_id)
                records.append(record)
            if [record.tile_id for record in records] != list(range(len(records))):
                raise ValueError("snapshot completed tiles are not a committed prefix")
            resume_count = int(manifest.get("resume_count", 0)) + 1
            state = SnapshotState(scene, records, resume_count, self)
            self._write_state_manifest(state)
            return SnapshotLoad(state=state, discarded=False, reason=None)
        except Exception as exc:
            self.discard(scene)
            return SnapshotLoad(
                state=SnapshotState(scene, [], 0, self),
                discarded=True,
                reason=f"{type(exc).__name__}: {exc}",
            )

    def _tile_file_names(self, tile_id: int, row: int, col: int) -> tuple[str, str, str]:
        base = f"tile_{tile_id:06d}_r{row}_c{col}"
        return (
            f"tiles/{base}_rgba.npy",
            f"tiles/{base}_depth.npy",
            f"tiles/{base}_projection.npy",
        )

    def commit_tile(
        self,
        scene: Scene,
        state: SnapshotState,
        tile_id: int,
        row: int,
        col: int,
        color: np.ndarray,
        depth: np.ndarray,
        projection: np.ndarray,
        stats: dict[str, Any],
    ) -> TileRecord:
        color_file, depth_file, projection_file = self._tile_file_names(tile_id, row, col)
        color = np.ascontiguousarray(color.astype(np.uint8, copy=False))
        depth = np.ascontiguousarray(depth.astype(np.float32, copy=False))
        projection = np.ascontiguousarray(projection.astype(np.float32, copy=False))
        _atomic_save_npy(self.state_dir / color_file, color)
        _atomic_save_npy(self.state_dir / depth_file, depth)
        _atomic_save_npy(self.state_dir / projection_file, projection)
        record = TileRecord(
            tile_id=tile_id,
            row=row,
            col=col,
            color_file=color_file,
            depth_file=depth_file,
            projection_file=projection_file,
            color_digest=sha256_array(color),
            depth_digest=sha256_array(depth),
            projection_digest=sha256_array(projection),
            stats=stats,
        )
        state.records.append(record)
        state.resume_count = state.resume_count
        self._write_state_manifest(state)
        return record

    def _write_state_manifest(self, state: SnapshotState) -> None:
        manifest = self._empty_manifest(state.scene, resume_count=state.resume_count)
        manifest["completed"] = [record.to_dict() for record in state.records]
        self._write_manifest(manifest)

    def _read_and_verify_tile(
        self, record: TileRecord
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        paths = [Path(record.color_file), Path(record.depth_file), Path(record.projection_file)]
        for path in paths:
            if path.is_absolute() or ".." in path.parts:
                raise ValueError("tile path escapes snapshot directory")
        try:
            color = np.load(self.state_dir / paths[0], allow_pickle=False)
            depth = np.load(self.state_dir / paths[1], allow_pickle=False)
            projection = np.load(self.state_dir / paths[2], allow_pickle=False)
        except Exception as exc:
            raise ValueError(f"tile file cannot be loaded: {exc}") from exc
        if color.dtype != np.uint8 or color.ndim != 3 or color.shape[2] != 4:
            raise ValueError("tile color array has an invalid dtype or shape")
        if depth.dtype != np.float32 or depth.ndim != 2:
            raise ValueError("tile depth array has an invalid dtype or shape")
        if projection.dtype != np.float32 or projection.ndim != 2:
            raise ValueError("tile projection array has an invalid dtype or shape")
        if depth.shape != projection.shape or color.shape[:2] != depth.shape:
            raise ValueError("tile arrays have inconsistent shapes")
        if sha256_array(color) != record.color_digest:
            raise ValueError("tile color digest differs from snapshot")
        if sha256_array(depth) != record.depth_digest:
            raise ValueError("tile depth digest differs from snapshot")
        if sha256_array(projection) != record.projection_digest:
            raise ValueError("tile projection digest differs from snapshot")
        return color, depth, projection

    def verify_recorded_tile(self, record: TileRecord) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        return self._read_and_verify_tile(record)

