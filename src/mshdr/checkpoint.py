"""Atomic per-segment checkpoints with whole-snapshot invalidation."""

from __future__ import annotations

import json
import os
import shutil
import tempfile
import threading
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterator

import fcntl
import numpy as np

from .errors import WorkbenchError, require
from .io_utils import (
    canonical_array_digest,
    read_json,
    sha256_bytes,
    value_version,
    write_json,
)


CHECKPOINT_SCHEMA = "1.0"
SEGMENT_ARRAY_FIELDS = (
    "fused_radiance",
    "cleaned_detail",
    "enhanced_detail",
    "chroma_xyz",
    "linear_rgb",
    "gamut_mask",
    "boundary_rgb",
    "gamut_excess",
    "synthetic_rgb",
)


@dataclass
class CheckpointLoadResult:
    entries: dict[int, dict[str, Any]]
    invalid_reason: str | None = None
    invalidated: bool = False
    manifest: dict[str, Any] | None = None

    @property
    def valid(self) -> bool:
        return self.invalid_reason is None


def _manifest_body(
    *,
    scene_id: str,
    metadata_version: str,
    params_version: str,
    algorithm_version: str,
    segment_size: tuple[int, int],
    entries: list[dict[str, Any]],
) -> dict[str, Any]:
    return {
        "schema_version": CHECKPOINT_SCHEMA,
        "scene_id": scene_id,
        "metadata_version": metadata_version,
        "params_version": params_version,
        "algorithm_version": algorithm_version,
        "segment_size": [int(segment_size[0]), int(segment_size[1])],
        "committed_indices": [int(entry["segment_index"]) for entry in entries],
        "entries": entries,
    }


def _seal_manifest(body: dict[str, Any]) -> dict[str, Any]:
    sealed = dict(body)
    sealed["manifest_sha256"] = value_version(body)
    return sealed


class CheckpointStore:
    """One scene checkpoint. All writes are serialized with both a thread lock
    and an advisory process lock, and each commit is made durable before the
    manifest is advanced."""

    def __init__(self, root: Path, scene_id: str):
        self.scene_id = scene_id
        self.root = Path(root) / scene_id
        self.segments_dir = self.root / "segments"
        self.state_path = self.root / "state.json"
        self.lock_path = self.root / ".lock"
        self.root.mkdir(parents=True, exist_ok=True)
        self.segments_dir.mkdir(parents=True, exist_ok=True)
        self._thread_lock = threading.RLock()
        self._manifest: dict[str, Any] | None = None
        self._entries: dict[int, dict[str, Any]] = {}

    @property
    def committed_indices(self) -> list[int]:
        return sorted(self._entries)

    @property
    def committed_count(self) -> int:
        return len(self._entries)

    def _array_payload(
        self,
        *,
        segment_index: int,
        arrays: dict[str, np.ndarray],
        metadata_snapshot: dict[str, Any],
        params_snapshot: dict[str, Any],
        metadata_version: str,
        params_version: str,
        algorithm_version: str,
    ) -> tuple[dict[str, np.ndarray], dict[str, Any]]:
        missing = [name for name in SEGMENT_ARRAY_FIELDS if name not in arrays]
        if missing:
            raise WorkbenchError("E_SEG", f"Segment payload is missing array(s): {', '.join(missing)}", field=missing[0])
        ordered = {name: np.ascontiguousarray(arrays[name]) for name in SEGMENT_ARRAY_FIELDS}
        content_metadata = {
            "segment_index": int(segment_index),
            "metadata_version": metadata_version,
            "params_version": params_version,
            "algorithm_version": algorithm_version,
            "metadata_snapshot": metadata_snapshot,
            "params_snapshot": params_snapshot,
        }
        content_hash = canonical_array_digest(ordered.items(), content_metadata)
        entry = {
            "segment_index": int(segment_index),
            "file": (Path("segments") / f"seg_{segment_index:05d}.npz").as_posix(),
            "content_sha256": content_hash,
            "metadata_version": metadata_version,
            "params_version": params_version,
            "algorithm_version": algorithm_version,
            "metadata_snapshot": metadata_snapshot,
            "params_snapshot": params_snapshot,
            "array_shapes": {name: list(array.shape) for name, array in ordered.items()},
            "array_dtypes": {name: array.dtype.str for name, array in ordered.items()},
        }
        return ordered, entry

    @contextmanager
    def _exclusive(self) -> Iterator[None]:
        with self._thread_lock:
            self.root.mkdir(parents=True, exist_ok=True)
            with self.lock_path.open("a+b") as lock_handle:
                fcntl.flock(lock_handle.fileno(), fcntl.LOCK_EX)
                try:
                    yield
                finally:
                    fcntl.flock(lock_handle.fileno(), fcntl.LOCK_UN)

    def reset(self) -> None:
        with self._exclusive():
            self._manifest = None
            self._entries = {}
            if self.state_path.exists():
                self.state_path.unlink()
            for path in self.segments_dir.glob("seg_*.npz"):
                path.unlink()

    def commit_segment(
        self,
        *,
        segment_index: int,
        arrays: dict[str, np.ndarray],
        metadata_snapshot: dict[str, Any],
        params_snapshot: dict[str, Any],
        metadata_version: str,
        params_version: str,
        algorithm_version: str,
        segment_size: tuple[int, int],
    ) -> dict[str, Any]:
        with self._exclusive():
            ordered, entry = self._array_payload(
                segment_index=segment_index,
                arrays=arrays,
                metadata_snapshot=metadata_snapshot,
                params_snapshot=params_snapshot,
                metadata_version=metadata_version,
                params_version=params_version,
                algorithm_version=algorithm_version,
            )
            segment_path = self.root / entry["file"]
            self._atomic_save_npz(segment_path, ordered)
            self._entries[int(segment_index)] = entry
            entries = [self._entries[index] for index in sorted(self._entries)]
            body = _manifest_body(
                scene_id=self.scene_id,
                metadata_version=metadata_version,
                params_version=params_version,
                algorithm_version=algorithm_version,
                segment_size=segment_size,
                entries=entries,
            )
            manifest = _seal_manifest(body)
            self._atomic_write_json(self.state_path, manifest)
            self._manifest = manifest
            return entry

    def load(
        self,
        *,
        metadata_version: str,
        params_version: str,
        algorithm_version: str,
        segment_size: tuple[int, int],
        total_segments: int,
        invalidate_on_mismatch: bool = True,
    ) -> CheckpointLoadResult:
        with self._exclusive():
            if not self.state_path.exists():
                self._manifest = None
                self._entries = {}
                return CheckpointLoadResult(entries={})
            try:
                manifest = read_json(self.state_path, field="checkpoint.state")
                self._validate_manifest(
                    manifest,
                    metadata_version=metadata_version,
                    params_version=params_version,
                    algorithm_version=algorithm_version,
                    segment_size=segment_size,
                    total_segments=total_segments,
                )
                entries: dict[int, dict[str, Any]] = {}
                for entry in manifest["entries"]:
                    index = int(entry["segment_index"])
                    segment_path = self.root / str(entry["file"])
                    if not segment_path.exists():
                        raise ValueError(f"missing committed segment file {segment_path}")
                    try:
                        with np.load(segment_path, allow_pickle=False) as payload:
                            arrays = {name: np.array(payload[name], copy=True) for name in SEGMENT_ARRAY_FIELDS}
                    except Exception as exc:
                        raise ValueError(f"cannot decode segment {index}: {exc}") from exc
                    for name, array in arrays.items():
                        if list(array.shape) != entry["array_shapes"][name]:
                            raise ValueError(f"shape mismatch for segment {index} field {name}")
                        if array.dtype.str != entry["array_dtypes"][name]:
                            raise ValueError(f"dtype mismatch for segment {index} field {name}")
                    content_metadata = {
                        "segment_index": index,
                        "metadata_version": entry["metadata_version"],
                        "params_version": entry["params_version"],
                        "algorithm_version": entry["algorithm_version"],
                        "metadata_snapshot": entry["metadata_snapshot"],
                        "params_snapshot": entry["params_snapshot"],
                    }
                    actual_hash = canonical_array_digest(arrays.items(), content_metadata)
                    if actual_hash != entry["content_sha256"]:
                        raise ValueError(f"content hash mismatch for segment {index}")
                    entries[index] = {**entry, "arrays": arrays}
                self._manifest = manifest
                self._entries = {index: {k: v for k, v in entry.items() if k != "arrays"} for index, entry in entries.items()}
                return CheckpointLoadResult(entries=entries, manifest=manifest)
            except Exception as exc:
                reason = f"{type(exc).__name__}: {exc}"
                self._manifest = None
                self._entries = {}
                if invalidate_on_mismatch:
                    self._archive_invalid_snapshot(reason)
                return CheckpointLoadResult(entries={}, invalid_reason=reason, invalidated=invalidate_on_mismatch)

    def _validate_manifest(
        self,
        manifest: dict[str, Any],
        *,
        metadata_version: str,
        params_version: str,
        algorithm_version: str,
        segment_size: tuple[int, int],
        total_segments: int,
    ) -> None:
        sealed_hash = manifest.get("manifest_sha256")
        body = {key: value for key, value in manifest.items() if key != "manifest_sha256"}
        if sealed_hash != value_version(body):
            raise ValueError("manifest self-hash mismatch")
        if manifest.get("schema_version") != CHECKPOINT_SCHEMA:
            raise ValueError("checkpoint schema mismatch")
        if manifest.get("scene_id") != self.scene_id:
            raise ValueError("scene id mismatch")
        if manifest.get("metadata_version") != metadata_version:
            raise ValueError("metadata version mismatch")
        if manifest.get("params_version") != params_version:
            raise ValueError("parameter version mismatch")
        if manifest.get("algorithm_version") != algorithm_version:
            raise ValueError("algorithm version mismatch")
        if manifest.get("segment_size") != [int(segment_size[0]), int(segment_size[1])]:
            raise ValueError("segment size mismatch")
        entries = manifest.get("entries")
        if not isinstance(entries, list):
            raise ValueError("entries must be a list")
        indices: list[int] = []
        for entry in entries:
            index = int(entry["segment_index"])
            if index < 0 or index >= total_segments:
                raise ValueError(f"segment index {index} out of range")
            if entry["metadata_version"] != metadata_version:
                raise ValueError(f"segment {index} metadata version mismatch")
            if entry["params_version"] != params_version:
                raise ValueError(f"segment {index} parameter version mismatch")
            if entry["algorithm_version"] != algorithm_version:
                raise ValueError(f"segment {index} algorithm version mismatch")
            indices.append(index)
        if len(indices) != len(set(indices)):
            raise ValueError("duplicate committed segment index")
        if manifest.get("committed_indices") != sorted(indices):
            raise ValueError("committed index list mismatch")

    def _archive_invalid_snapshot(self, reason: str) -> None:
        archive_root = self.root.parent / ".invalidated"
        archive_root.mkdir(parents=True, exist_ok=True)
        safe_reason = sha256_bytes(reason.encode("utf-8"))[:10]
        sequence = 0
        while True:
            destination = archive_root / f"{self.scene_id}-{safe_reason}-{sequence:03d}"
            if not destination.exists():
                break
            sequence += 1
        try:
            os.replace(self.root, destination)
        except OSError:
            # If a rename is not possible (e.g. another descriptor is open),
            # remove the snapshot rather than ever mixing old and new segments.
            shutil.rmtree(self.root, ignore_errors=True)
        self.root.mkdir(parents=True, exist_ok=True)
        self.segments_dir.mkdir(parents=True, exist_ok=True)

    @staticmethod
    def _atomic_save_npz(path: Path, arrays: dict[str, np.ndarray]) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        fd, tmp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
        tmp_path = Path(tmp_name)
        try:
            with os.fdopen(fd, "wb") as handle:
                np.savez_compressed(handle, **arrays)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(tmp_path, path)
            _fsync_directory(path.parent)
        except Exception:
            tmp_path.unlink(missing_ok=True)
            raise

    @staticmethod
    def _atomic_write_json(path: Path, value: Any) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        fd, tmp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
        tmp_path = Path(tmp_name)
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as handle:
                json.dump(value, handle, indent=2, sort_keys=True, ensure_ascii=False, allow_nan=False)
                handle.write("\n")
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(tmp_path, path)
            _fsync_directory(path.parent)
        except Exception:
            tmp_path.unlink(missing_ok=True)
            raise


def _fsync_directory(path: Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)

