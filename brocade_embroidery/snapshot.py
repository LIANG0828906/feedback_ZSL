"""Atomic tile snapshots and crash/restart bookkeeping."""

from __future__ import annotations

import json
import os
import tempfile
from dataclasses import dataclass

import numpy as np

from .scene_gen import embroidery_seed_signature, scene_digest

SNAPSHOT_NAME = "snapshot.json"


class SnapshotMismatch(RuntimeError):
    """Raised when an existing snapshot belongs to another scene/seed."""


def _atomic_write(path: str, payload: bytes) -> None:
    directory = os.path.dirname(path) or "."
    os.makedirs(directory, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=".tmp-", dir=directory)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def color_digest(rgba: np.ndarray) -> str:
    import hashlib

    quantized = np.clip(np.rint(rgba * 255.0), 0, 255).astype(np.uint8)
    return hashlib.sha256(quantized.tobytes()).hexdigest()


def block_summary(rgba: np.ndarray, ownership: np.ndarray) -> dict:
    return {
        "mean_rgba": [round(float(x), 6) for x in rgba.mean(axis=(0, 1))],
        "color_digest": color_digest(rgba),
        "ownership_digest": _array_digest(ownership.astype("<u2")),
    }


def _array_digest(array: np.ndarray) -> str:
    import hashlib

    return hashlib.sha256(array.tobytes()).hexdigest()


@dataclass
class SnapshotStore:
    output_dir: str
    scene: dict

    def __post_init__(self) -> None:
        self.snapshot_dir = os.path.join(self.output_dir, "snapshot")
        self.snapshot_path = os.path.join(self.snapshot_dir, SNAPSHOT_NAME)
        self.scene_digest = scene_digest(self.scene)
        self.seed_signature = embroidery_seed_signature(self.scene)

    def reset(self) -> None:
        if os.path.isdir(self.snapshot_dir):
            for name in sorted(os.listdir(self.snapshot_dir)):
                os.unlink(os.path.join(self.snapshot_dir, name))
        os.makedirs(self.snapshot_dir, exist_ok=True)
        self._write_snapshot(completed=[], commits=0)

    def _write_snapshot(self, completed: list[dict], commits: int) -> None:
        payload = {
            "schema_version": 1,
            "scene_digest": self.scene_digest,
            "seed_signature": self.seed_signature,
            "frame": self.scene["frame"],
            "commit_count": commits,
            "completed_blocks": completed,
        }
        _atomic_write(
            self.snapshot_path,
            json.dumps(payload, sort_keys=True, indent=2).encode("utf-8"),
        )

    def load(self) -> dict:
        """Load and validate a snapshot.

        Any digest mismatch, seed change, malformed JSON or missing tile blob
        discards the entire snapshot and restarts from block zero.
        """
        if not os.path.exists(self.snapshot_path):
            return {"completed": [], "commits": 0, "restarted": False}
        try:
            with open(self.snapshot_path, "rb") as handle:
                payload = json.loads(handle.read().decode("utf-8"))
            if payload["scene_digest"] != self.scene_digest:
                raise SnapshotMismatch("scene content digest changed")
            if payload["seed_signature"] != self.seed_signature:
                raise SnapshotMismatch("embroidery/hole seed changed")
            if payload["frame"] != self.scene["frame"]:
                raise SnapshotMismatch("frame/tile parameters changed")
            completed_ids = []
            for block in payload["completed_blocks"]:
                rgba_path = os.path.join(
                    self.snapshot_dir, f"tile_{block['tile_id']:04d}.rgba")
                owner_path = os.path.join(
                    self.snapshot_dir, f"tile_{block['tile_id']:04d}.owner")
                expected_rgba = os.path.getsize(rgba_path)
                expected_owner = os.path.getsize(owner_path)
                if expected_rgba <= 0 or expected_owner <= 0:
                    raise SnapshotMismatch("tile blob is empty")
                completed_ids.append(int(block["tile_id"]))
            if completed_ids != sorted(completed_ids):
                raise SnapshotMismatch("tile order is not contiguous")
            return {
                "completed": payload["completed_blocks"],
                "commits": int(payload["commit_count"]),
                "restarted": False,
            }
        except (OSError, ValueError, KeyError, SnapshotMismatch) as exc:
            self.reset()
            return {
                "completed": [],
                "commits": 0,
                "restarted": True,
                "discard_reason": str(exc),
            }

    def commit_tile(self, tile_id: int, tile_shape: tuple[int, int],
                    rgba: np.ndarray, ownership: np.ndarray,
                    stats: dict, completed: list[dict],
                    commits: int) -> tuple[list[dict], int]:
        rgba_path = os.path.join(self.snapshot_dir,
                                 f"tile_{tile_id:04d}.rgba")
        owner_path = os.path.join(self.snapshot_dir,
                                  f"tile_{tile_id:04d}.owner")
        _atomic_write(rgba_path, np.ascontiguousarray(
            rgba, dtype="<f4").tobytes())
        _atomic_write(owner_path, np.ascontiguousarray(
            ownership, dtype="<u2").tobytes())
        record = {
            "tile_id": tile_id,
            "tile_shape": list(tile_shape),
            "summary": block_summary(rgba, ownership),
            "stats": stats,
        }
        completed = [*completed, record]
        commits += 1
        # Whole-snapshot replacement: block offsets are never patched in place.
        self._write_snapshot(completed, commits)
        return completed, commits

    def read_tile(self, tile_id: int, shape: tuple[int, int]) -> tuple[np.ndarray, np.ndarray]:
        rgba = np.fromfile(
            os.path.join(self.snapshot_dir, f"tile_{tile_id:04d}.rgba"),
            dtype="<f4").reshape(shape[0], shape[1], 4).copy()
        owner = np.fromfile(
            os.path.join(self.snapshot_dir, f"tile_{tile_id:04d}.owner"),
            dtype="<u2").reshape(shape[0], shape[1]).copy()
        return rgba, owner
