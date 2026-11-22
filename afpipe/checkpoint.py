"""Resumable, crash-safe build checkpoints.

Layout under ``<work>/checkpoint``::

    state.json              # serial journal of committed shards + failures
    shards/<file_id>/00000000.json
    shards/<file_id>/00000001.json
    shards/<file_id>/_complete.json

Guarantees:

* every shard is written serially via temp-file + atomic ``os.replace``;
  worker threads never interleave checkpoint writes (one global lock);
* each shard records its content hash, parameter version and algorithm
  version.  On resume, any mismatch invalidates the *whole* snapshot and
  forces a full rebuild - partial reuse of old shards is forbidden;
* a file is only reusable once its ``_complete`` marker exists, so a kill
  mid-file loses only that file's progress.
"""

from __future__ import annotations

import os
import threading
from dataclasses import dataclass, field

from .config import ALGORITHM_VERSION, Config
from .errors import CorruptionError
from .hashing import atomic_write_json, ensure_dir, read_json, stable_digest
from .fingerprint import FrameFingerprint

STATE_NAME = "state.json"
SHARD_DIR = "shards"


def shard_payload_hash(frames: list[FrameFingerprint]) -> str:
    payload = [(f.index, round(f.time_s, 9), f.hash_hex) for f in frames]
    return stable_digest(payload, prefix="sh-", length=16)


@dataclass
class FileCheckpoint:
    file_id: str
    content_hash: str
    shard_hashes: list[str] = field(default_factory=list)
    complete: bool = False
    frame_count: int = 0
    failed: bool = False
    error_code: str | None = None


class CheckpointStore:
    def __init__(self, work_dir: str, cfg: Config, manifest_fingerprint: str):
        self.root = ensure_dir(os.path.join(work_dir, "checkpoint"))
        self.shard_root = ensure_dir(os.path.join(self.root, SHARD_DIR))
        self.cfg = cfg
        self.manifest_fingerprint = manifest_fingerprint
        self.parameter_version = cfg.parameter_version()
        self._lock = threading.Lock()
        self.state_path = os.path.join(self.root, STATE_NAME)
        self.state = self._fresh_state()

    def _fresh_state(self) -> dict:
        return {
            "algorithm_version": ALGORITHM_VERSION,
            "parameter_version": self.parameter_version,
            "manifest_fingerprint": self.manifest_fingerprint,
            "index_format_hint": "2.0.0",
            "files": {},
        }

    # ------------------------------------------------------------------ load
    def load(self) -> dict[str, FileCheckpoint]:
        """Return reusable per-file checkpoints or wipe everything on mismatch."""
        if not os.path.isfile(self.state_path):
            return {}
        try:
            state = read_json(self.state_path)
        except ValueError as exc:
            return self._invalidate("state journal unreadable", str(exc))

        identity_ok = (
            state.get("algorithm_version") == ALGORITHM_VERSION
            and state.get("parameter_version") == self.parameter_version
            and state.get("manifest_fingerprint") == self.manifest_fingerprint
        )
        if not identity_ok:
            return self._invalidate(
                "parameter/algorithm/manifest identity mismatch",
                {
                    "stored": {
                        "algorithm_version": state.get("algorithm_version"),
                        "parameter_version": state.get("parameter_version"),
                        "manifest_fingerprint": state.get("manifest_fingerprint"),
                    },
                    "expected": {
                        "algorithm_version": ALGORITHM_VERSION,
                        "parameter_version": self.parameter_version,
                        "manifest_fingerprint": self.manifest_fingerprint,
                    },
                },
            )

        # Verify every shard file on disk before trusting the journal.
        result: dict[str, FileCheckpoint] = {}
        for file_id, rec in state.get("files", {}).items():
            cp = FileCheckpoint(
                file_id=file_id,
                content_hash=rec["content_hash"],
                shard_hashes=list(rec.get("shard_hashes", [])),
                complete=bool(rec.get("complete")),
                frame_count=int(rec.get("frame_count", 0)),
                failed=bool(rec.get("failed")),
                error_code=rec.get("error_code"),
            )
            if not self._verify_shards(cp):
                return self._invalidate(
                    "shard content hash mismatch", {"file_id": file_id}
                )
            if cp.complete:
                result[file_id] = cp
        self.state = state
        return result

    def _verify_shards(self, cp: FileCheckpoint) -> bool:
        directory = os.path.join(self.shard_root, cp.file_id)
        if not os.path.isdir(directory):
            return False
        on_disk = sorted(
            n for n in os.listdir(directory)
            if n.endswith(".json") and not n.startswith("_")
        )
        if len(on_disk) != len(cp.shard_hashes):
            return False
        for name, expected in zip(on_disk, cp.shard_hashes):
            try:
                doc = read_json(os.path.join(directory, name))
            except ValueError:
                return False
            if doc.get("shard_hash") != expected:
                return False
            if doc.get("parameter_version") != self.parameter_version:
                return False
            if doc.get("algorithm_version") != ALGORITHM_VERSION:
                return False
            if doc.get("content_hash") != cp.content_hash:
                return False
        return True

    def _invalidate(self, reason: str, detail) -> dict:
        """Whole-snapshot invalidation: delete everything and start clean."""
        self.reset()
        self.state["invalidation"] = {"reason": reason, "detail": detail}
        atomic_write_json(self.state_path, self.state)
        return {}

    def reset(self) -> None:
        """Remove all checkpoint bytes (serial, under lock)."""
        with self._lock:
            self.state = self._fresh_state()
            if os.path.isdir(self.shard_root):
                for name in os.listdir(self.shard_root):
                    self._rmtree(os.path.join(self.shard_root, name))
            if os.path.isfile(self.state_path):
                os.unlink(self.state_path)

    @staticmethod
    def _rmtree(path: str) -> None:
        import shutil

        shutil.rmtree(path, ignore_errors=True)

    # ------------------------------------------------------------- committing
    def begin_file(self, file_id: str, content_hash: str) -> None:
        with self._lock:
            directory = os.path.join(self.shard_root, file_id)
            self._rmtree(directory)
            ensure_dir(directory)
            self.state.setdefault("files", {})[file_id] = {
                "content_hash": content_hash,
                "shard_hashes": [],
                "complete": False,
                "frame_count": 0,
                "failed": False,
            }
            atomic_write_json(self.state_path, self.state)

    def append_shard(self, file_id: str, content_hash: str,
                     shard_index: int, frames: list[FrameFingerprint]) -> str:
        """Persist one shard atomically and serially; returns its hash."""
        shash = shard_payload_hash(frames)
        doc = {
            "file_id": file_id,
            "shard_index": shard_index,
            "content_hash": content_hash,
            "parameter_version": self.parameter_version,
            "algorithm_version": ALGORITHM_VERSION,
            "shard_hash": shash,
            "frames": [
                {"i": f.index, "t": round(f.time_s, 9), "h": f.hash_hex}
                for f in frames
            ],
        }
        with self._lock:
            path = os.path.join(
                self.shard_root, file_id, f"{shard_index:08d}.json"
            )
            atomic_write_json(path, doc)
            rec = self.state["files"][file_id]
            rec["shard_hashes"].append(shash)
            rec["frame_count"] = rec.get("frame_count", 0) + len(frames)
            atomic_write_json(self.state_path, self.state)
        return shash

    def complete_file(self, file_id: str, frame_count: int) -> None:
        with self._lock:
            rec = self.state["files"][file_id]
            rec["complete"] = True
            rec["frame_count"] = frame_count
            marker = {
                "file_id": file_id,
                "content_hash": rec["content_hash"],
                "parameter_version": self.parameter_version,
                "algorithm_version": ALGORITHM_VERSION,
                "shard_hashes": rec["shard_hashes"],
                "frame_count": frame_count,
            }
            atomic_write_json(
                os.path.join(self.shard_root, file_id, "_complete.json"), marker
            )
            atomic_write_json(self.state_path, self.state)

    def mark_failed(self, file_id: str, code: str, message: str) -> None:
        with self._lock:
            rec = self.state.setdefault("files", {}).setdefault(file_id, {
                "content_hash": None, "shard_hashes": [],
                "complete": False, "frame_count": 0,
            })
            rec["failed"] = True
            rec["error_code"] = code
            rec["error_message"] = message
            atomic_write_json(self.state_path, self.state)

    def load_frames(self, cp: FileCheckpoint) -> list[FrameFingerprint]:
        frames: list[FrameFingerprint] = []
        directory = os.path.join(self.shard_root, cp.file_id)
        for name in sorted(os.listdir(directory)):
            if not name.endswith(".json") or name.startswith("_"):
                continue
            doc = read_json(os.path.join(directory, name))
            for row in doc["frames"]:
                frames.append(FrameFingerprint(row["i"], row["t"], row["h"]))
        return frames

    def invalidation_record(self) -> dict | None:
        return self.state.get("invalidation")

    def snapshot_summary(self) -> dict:
        files = self.state.get("files", {})
        return {
            "parameter_version": self.parameter_version,
            "algorithm_version": ALGORITHM_VERSION,
            "manifest_fingerprint": self.manifest_fingerprint,
            "files": {
                fid: {
                    "complete": rec.get("complete"),
                    "failed": rec.get("failed"),
                    "shards": len(rec.get("shard_hashes", [])),
                    "frames": rec.get("frame_count", 0),
                }
                for fid, rec in files.items()
            },
        }
