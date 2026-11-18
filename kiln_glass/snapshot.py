"""Crash-resumable tile snapshots.

Only fully rendered tiles are ever committed.  A snapshot is valid solely
for an identical scene fingerprint, identical fringe seeds and an intact
per-tile colour digest; any mismatch forces a full restart from the first
view, never reuse of stale tile offsets.
"""

from __future__ import annotations

import hashlib
import os
from dataclasses import dataclass

from .io_utils import read_json, write_json

SNAPSHOT_NAME = "progress_snapshot.json"


def scene_fingerprint(canonical_bytes: bytes, fringe_seeds: list[int]) -> str:
    digest = hashlib.sha256()
    digest.update(canonical_bytes)
    digest.update(b"|fringe-seeds|")
    digest.update(",".join(str(s) for s in sorted(fringe_seeds)).encode("ascii"))
    return digest.hexdigest()


@dataclass
class SnapshotState:
    fingerprint: str
    scene_summary: dict
    completed: dict  # key "view:tile" -> tile summary
    current_view: int
    interrupted: bool

    def as_dict(self) -> dict:
        return {
            "version": 1,
            "fingerprint": self.fingerprint,
            "scene_summary": self.scene_summary,
            "completed": self.completed,
            "current_view": self.current_view,
            "interrupted": self.interrupted,
        }


def load_snapshot(output_dir: str, fingerprint: str,
                  scene_summary: dict) -> SnapshotState | None:
    path = os.path.join(output_dir, SNAPSHOT_NAME)
    if not os.path.exists(path):
        return None
    try:
        payload = read_json(path)
        if not isinstance(payload, dict):
            raise ValueError("snapshot root is not an object")
        required = {"version", "fingerprint", "scene_summary",
                    "completed", "current_view"}
        if not required.issubset(payload):
            raise ValueError("snapshot missing required fields")
        if int(payload["version"]) != 1:
            raise ValueError("unsupported snapshot version")
        if payload["fingerprint"] != fingerprint:
            raise ValueError("scene fingerprint/fringe seed mismatch")
        if payload["scene_summary"] != scene_summary:
            raise ValueError("scene content summary mismatch")
        if not isinstance(payload["completed"], dict):
            raise ValueError("completed map corrupt")
        return SnapshotState(
            fingerprint=fingerprint,
            scene_summary=scene_summary,
            completed=dict(payload["completed"]),
            current_view=int(payload["current_view"]),
            interrupted=bool(payload.get("interrupted", False)),
        )
    except (OSError, ValueError, KeyError, TypeError):
        discard_snapshot(output_dir)
        return None


def commit_snapshot(output_dir: str, state: SnapshotState) -> None:
    write_json(os.path.join(output_dir, SNAPSHOT_NAME), state.as_dict())


def discard_snapshot(output_dir: str) -> None:
    path = os.path.join(output_dir, SNAPSHOT_NAME)
    if os.path.exists(path):
        os.remove(path)


def tile_key(view_index: int, tile_index: int) -> str:
    return f"{view_index}:{tile_index}"
