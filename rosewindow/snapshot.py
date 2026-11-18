"""Crash-safe, whole-file snapshots for chunked rendering."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .io_utils import canonical_json_bytes
from .scene import Scene


SNAPSHOT_FORMAT = "rosewindow-snapshot-v1"
RENDERER_SNAPSHOT_VERSION = "1.0.0"


@dataclass
class SnapshotState:
    rgb: np.ndarray
    owner: np.ndarray
    transmission_sums: np.ndarray
    reflection_sums: np.ndarray
    pixel_counts: np.ndarray
    max_linear_value: float = 0.0
    completed_chunks: set[int] = field(default_factory=set)
    chunk_summaries: dict[int, dict[str, Any]] = field(default_factory=dict)


@dataclass(frozen=True)
class SnapshotLoad:
    state: SnapshotState
    status: str
    detail: str | None = None


def empty_state(scene: Scene) -> SnapshotState:
    piece_count = len(scene.pieces)
    return SnapshotState(
        rgb=np.zeros((scene.height, scene.width, 3), dtype=np.uint8),
        owner=np.zeros((scene.height, scene.width), dtype=np.int32),
        transmission_sums=np.zeros(piece_count, dtype=np.float64),
        reflection_sums=np.zeros(piece_count, dtype=np.float64),
        pixel_counts=np.zeros(piece_count, dtype=np.int64),
    )


def chunk_summary(
    chunk_id: int, rgb: np.ndarray, owner: np.ndarray
) -> dict[str, Any]:
    mean_rgb = rgb.reshape(-1, 3).mean(axis=0)
    return {
        "chunk_id": chunk_id,
        "rgb_sha256": hashlib.sha256(
            np.ascontiguousarray(rgb, dtype=np.uint8).tobytes()
        ).hexdigest(),
        "owner_sha256": hashlib.sha256(
            np.ascontiguousarray(owner, dtype=np.int32).tobytes()
        ).hexdigest(),
        "mean_rgb_0_255": [round(float(value), 9) for value in mean_rgb],
        "pixel_count": int(rgb.shape[0] * rgb.shape[1]),
    }


def summaries_for_completed(
    scene: Scene,
    state: SnapshotState,
    completed: set[int] | None = None,
) -> dict[int, dict[str, Any]]:
    selected = state.completed_chunks if completed is None else completed
    summaries: dict[int, dict[str, Any]] = {}
    for chunk_id in sorted(selected):
        x0, y0, x1, y1 = scene.chunk_bounds(chunk_id)
        summaries[chunk_id] = chunk_summary(
            chunk_id,
            state.rgb[y0:y1, x0:x1],
            state.owner[y0:y1, x0:x1],
        )
    return summaries


class SnapshotStore:
    def __init__(self, scene: Scene, directory: str | Path) -> None:
        self.scene = scene
        self.directory = Path(directory)
        self.path = self.directory / "snapshot.npz"

    def discard(self) -> None:
        self.directory.mkdir(parents=True, exist_ok=True)
        try:
            if self.path.exists():
                self.path.unlink()
        except OSError:
            pass
        for temporary in self.directory.glob(".snapshot.npz.tmp-*"):
            try:
                temporary.unlink()
            except OSError:
                pass

    def load(self, *, fresh: bool = False) -> SnapshotLoad:
        if fresh:
            self.discard()
            return SnapshotLoad(empty_state(self.scene), "fresh")
        if not self.path.exists():
            return SnapshotLoad(empty_state(self.scene), "fresh")
        try:
            with np.load(self.path, allow_pickle=False) as archive:
                required = {
                    "meta",
                    "rgb",
                    "owner",
                    "transmission_sums",
                    "reflection_sums",
                    "pixel_counts",
                    "max_linear_value",
                }
                if not required.issubset(set(archive.files)):
                    raise ValueError("snapshot is missing required arrays")
                meta_bytes = np.ascontiguousarray(archive["meta"], dtype=np.uint8).tobytes()
                metadata = json.loads(meta_bytes.decode("utf-8"))
                rgb = np.array(archive["rgb"], dtype=np.uint8, copy=True)
                owner = np.array(archive["owner"], dtype=np.int32, copy=True)
                transmission_sums = np.array(
                    archive["transmission_sums"], dtype=np.float64, copy=True
                )
                reflection_sums = np.array(
                    archive["reflection_sums"], dtype=np.float64, copy=True
                )
                pixel_counts = np.array(
                    archive["pixel_counts"], dtype=np.int64, copy=True
                )
                max_linear_array = np.array(
                    archive["max_linear_value"], dtype=np.float64, copy=True
                )
            if metadata.get("format") != SNAPSHOT_FORMAT:
                raise ValueError("snapshot format mismatch")
            if metadata.get("renderer_version") != RENDERER_SNAPSHOT_VERSION:
                raise ValueError("renderer version mismatch")
            if metadata.get("scene_digest") != self.scene.digest:
                self.discard()
                return SnapshotLoad(
                    empty_state(self.scene),
                    "discarded_scene_mismatch",
                    "scene digest differs from the committed snapshot",
                )
            expected_shape = (self.scene.height, self.scene.width, 3)
            if rgb.shape != expected_shape or rgb.dtype != np.uint8:
                raise ValueError("snapshot RGB shape or dtype mismatch")
            if owner.shape != expected_shape[:2] or owner.dtype != np.int32:
                raise ValueError("snapshot owner shape or dtype mismatch")
            piece_count = len(self.scene.pieces)
            for array, label in (
                (transmission_sums, "transmission_sums"),
                (reflection_sums, "reflection_sums"),
                (pixel_counts, "pixel_counts"),
            ):
                if array.shape != (piece_count,):
                    raise ValueError(f"snapshot {label} shape mismatch")
            if metadata.get("width") != self.scene.width:
                raise ValueError("snapshot width mismatch")
            if metadata.get("height") != self.scene.height:
                raise ValueError("snapshot height mismatch")
            if metadata.get("chunk_size") != self.scene.chunk_size:
                raise ValueError("snapshot chunk size mismatch")
            if max_linear_array.size != 1:
                raise ValueError("snapshot max_linear_value shape mismatch")

            completed_raw = metadata.get("completed_chunks")
            if not isinstance(completed_raw, list):
                raise ValueError("snapshot completed_chunks is invalid")
            completed: set[int] = set()
            for value in completed_raw:
                if not isinstance(value, int) or isinstance(value, bool):
                    raise ValueError("snapshot chunk id is not an integer")
                if not 0 <= value < self.scene.chunk_count:
                    raise ValueError("snapshot chunk id is out of range")
                if value in completed:
                    raise ValueError("snapshot contains a duplicate chunk id")
                completed.add(value)

            summary_raw = metadata.get("chunk_summaries")
            if not isinstance(summary_raw, dict):
                raise ValueError("snapshot chunk_summaries is invalid")
            if set(summary_raw) != {str(chunk_id) for chunk_id in completed}:
                raise ValueError("snapshot chunk summary set is inconsistent")

            payload_sha = metadata.pop("payload_sha256", None)
            if not isinstance(payload_sha, str):
                raise ValueError("snapshot payload hash is missing")
            digest = hashlib.sha256()
            for array in (
                rgb,
                owner,
                transmission_sums,
                reflection_sums,
                pixel_counts,
                max_linear_array,
            ):
                digest.update(np.ascontiguousarray(array).tobytes())
            digest.update(canonical_json_bytes(metadata))
            if digest.hexdigest() != payload_sha:
                raise ValueError("snapshot payload checksum mismatch")

            state = SnapshotState(
                rgb=rgb,
                owner=owner,
                transmission_sums=transmission_sums,
                reflection_sums=reflection_sums,
                pixel_counts=pixel_counts,
                max_linear_value=float(max_linear_array.reshape(-1)[0]),
                completed_chunks=completed,
                chunk_summaries={},
            )
            recomputed = summaries_for_completed(self.scene, state)
            for chunk_id in sorted(completed):
                stored = summary_raw[str(chunk_id)]
                if stored != recomputed[chunk_id]:
                    raise ValueError(f"snapshot chunk {chunk_id} color summary mismatch")
            state.chunk_summaries = recomputed
            return SnapshotLoad(state, "resumed")
        except Exception as exc:
            self.discard()
            return SnapshotLoad(
                empty_state(self.scene),
                "discarded_corrupt",
                f"{type(exc).__name__}: {exc}",
            )

    def commit(self, state: SnapshotState) -> None:
        self.directory.mkdir(parents=True, exist_ok=True)
        summaries = {
            str(chunk_id): summary
            for chunk_id, summary in sorted(state.chunk_summaries.items())
        }
        max_linear_array = np.asarray([state.max_linear_value], dtype=np.float64)
        metadata: dict[str, Any] = {
            "format": SNAPSHOT_FORMAT,
            "renderer_version": RENDERER_SNAPSHOT_VERSION,
            "scene_digest": self.scene.digest,
            "width": self.scene.width,
            "height": self.scene.height,
            "chunk_size": self.scene.chunk_size,
            "chunks_x": self.scene.chunks_x,
            "chunks_y": self.scene.chunks_y,
            "chunk_count": self.scene.chunk_count,
            "completed_chunks": sorted(state.completed_chunks),
            "chunk_summaries": summaries,
        }
        digest = hashlib.sha256()
        for array in (
            state.rgb,
            state.owner,
            state.transmission_sums,
            state.reflection_sums,
            state.pixel_counts,
            max_linear_array,
        ):
            digest.update(np.ascontiguousarray(array).tobytes())
        digest.update(canonical_json_bytes(metadata))
        metadata["payload_sha256"] = digest.hexdigest()
        meta_array = np.frombuffer(canonical_json_bytes(metadata), dtype=np.uint8)

        descriptor, temp_name = tempfile.mkstemp(
            prefix=".snapshot.npz.tmp-", dir=str(self.directory)
        )
        try:
            with os.fdopen(descriptor, "wb") as handle:
                np.savez(
                    handle,
                    meta=meta_array,
                    rgb=state.rgb,
                    owner=state.owner,
                    transmission_sums=state.transmission_sums,
                    reflection_sums=state.reflection_sums,
                    pixel_counts=state.pixel_counts,
                    max_linear_value=max_linear_array,
                )
                handle.flush()
                os.fsync(handle.fileno())
            try:
                os.chmod(temp_name, 0o644)
            except OSError:
                pass
            os.replace(temp_name, self.path)
            try:
                directory_fd = os.open(self.directory, os.O_RDONLY)
                try:
                    os.fsync(directory_fd)
                finally:
                    os.close(directory_fd)
            except OSError:
                pass
        except BaseException:
            try:
                os.unlink(temp_name)
            except OSError:
                pass
            raise
