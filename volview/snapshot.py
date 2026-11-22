"""Session snapshot persistence.

* Writes are atomic (temp file + ``os.replace``) and serialised through a
  single lock, so concurrent sessions never interleave a snapshot file.
* ``save_ordered`` commits snapshots strictly in session operation order
  even when rendering ran concurrently: a sequence number is assigned per
  operation and commits block until all earlier sequences are written.
* Every committed snapshot is appended to ``history.jsonl`` so the exact
  commit order can be compared between runs.
* On load, the snapshot is accepted only as a whole: content hash, schema
  version and interpolation order must all match, otherwise the snapshot is
  discarded entirely and a default session is rebuilt -- partial reuse of
  stale parameters is forbidden.
"""

from __future__ import annotations

import hashlib
import io
import json
import os
import threading
from typing import Optional

import numpy as np

SNAPSHOT_SCHEMA_VERSION = 3


def _atomic_write_bytes(path: str, payload: bytes) -> None:
    tmp = f"{path}.tmp.{os.getpid()}.{threading.get_ident()}"
    with open(tmp, "wb") as fh:
        fh.write(payload)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


class SnapshotStore:
    """Directory-backed snapshot store with serialised, ordered commits."""

    def __init__(self, directory: str):
        self.directory = directory
        os.makedirs(directory, exist_ok=True)
        self._write_lock = threading.Lock()       # serialises all file writes
        self._order_cond = threading.Condition()  # enforces commit order
        self._next_sequence = 0
        self._committed_sequence = -1

    def _snapshot_path(self, session_id: str) -> str:
        return os.path.join(self.directory, f"snapshot_{session_id}.json")

    def _plane_path(self, session_id: str) -> str:
        return os.path.join(self.directory, f"snapshot_{session_id}_plane.npy")

    def _history_path(self) -> str:
        return os.path.join(self.directory, "history.jsonl")

    def allocate_sequence(self) -> int:
        with self._order_cond:
            seq = self._next_sequence
            self._next_sequence += 1
            return seq

    def save_ordered(self, sequence: int, session_id: str, snapshot: dict,
                     plane_image: np.ndarray) -> None:
        """Atomically commit a snapshot once all earlier sequences committed."""
        plane_image = np.ascontiguousarray(plane_image)
        plane_hash = hashlib.sha256(plane_image.tobytes()).hexdigest()
        snapshot = dict(snapshot)
        snapshot["plane_image"] = {
            "file": os.path.basename(self._plane_path(session_id)),
            "sha256": plane_hash,
            "shape": list(plane_image.shape),
            "dtype": str(plane_image.dtype),
        }
        payload = json.dumps(snapshot, sort_keys=True, indent=2).encode("utf-8")
        plane_buffer = io.BytesIO()
        np.save(plane_buffer, plane_image)

        with self._order_cond:
            self._order_cond.wait_for(
                lambda: sequence == self._committed_sequence + 1)
            with self._write_lock:
                _atomic_write_bytes(self._plane_path(session_id),
                                    plane_buffer.getvalue())
                _atomic_write_bytes(self._snapshot_path(session_id), payload)
                with open(self._history_path(), "a", encoding="utf-8") as fh:
                    fh.write(json.dumps({
                        "sequence": sequence,
                        "session_id": session_id,
                        "plane_sha256": plane_hash,
                    }, sort_keys=True) + "\n")
            self._committed_sequence = sequence
            self._order_cond.notify_all()

    def load(self, session_id: str, content_hash: str,
             interp_order: Optional[int]) -> Optional[dict]:
        """Return the snapshot if fully valid, else discard it entirely."""
        path = self._snapshot_path(session_id)
        if not os.path.exists(path):
            return None
        try:
            with open(path, "r", encoding="utf-8") as fh:
                snapshot = json.load(fh)
        except (OSError, json.JSONDecodeError):
            self._discard(session_id)
            return None

        mismatch = None
        if snapshot.get("schema_version") != SNAPSHOT_SCHEMA_VERSION:
            mismatch = ("schema_version", snapshot.get("schema_version"))
        elif snapshot.get("content_hash") != content_hash:
            mismatch = ("content_hash", snapshot.get("content_hash"))
        elif interp_order is not None and \
                snapshot.get("params", {}).get("interp_order") != interp_order:
            mismatch = ("interp_order",
                        snapshot.get("params", {}).get("interp_order"))
        if mismatch is not None:
            self._discard(session_id)
            return None

        plane_file = snapshot.get("plane_image", {}).get("file")
        plane_path = os.path.join(self.directory, plane_file or "")
        if not plane_file or not os.path.exists(plane_path):
            self._discard(session_id)
            return None
        plane = np.load(plane_path, allow_pickle=False)
        if hashlib.sha256(np.ascontiguousarray(plane).tobytes()).hexdigest() \
                != snapshot["plane_image"]["sha256"]:
            self._discard(session_id)
            return None
        snapshot["_plane_array"] = plane
        return snapshot

    def _discard(self, session_id: str) -> None:
        for path in (self._snapshot_path(session_id),
                     self._plane_path(session_id)):
            try:
                os.remove(path)
            except OSError:
                pass

    def history(self) -> list:
        path = self._history_path()
        if not os.path.exists(path):
            return []
        with open(path, "r", encoding="utf-8") as fh:
            return [json.loads(line) for line in fh if line.strip()]
