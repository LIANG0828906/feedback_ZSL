from __future__ import annotations

import copy
import json
import os
import threading
from pathlib import Path
from typing import Any

from .errors import E_CORRUPT, E_IO, WorkbenchError, fail
from .jsonutil import (
    atomic_write_bytes,
    canonical_json_bytes,
    read_json,
    sha256_bytes,
)
from .model import ContainerModel
from .package import (
    ParsedSegment,
    parse_segment_bytes,
    segment_to_model_track,
    validate_track_payload,
)


CHECKPOINT_FORMAT = "mediaforge-checkpoint/1"
_LOCKS_GUARD = threading.Lock()
_PROCESS_LOCKS: dict[str, threading.RLock] = {}


def _process_lock(path: Path) -> threading.RLock:
    key = str(path.resolve())
    with _LOCKS_GUARD:
        lock = _PROCESS_LOCKS.get(key)
        if lock is None:
            lock = threading.RLock()
            _PROCESS_LOCKS[key] = lock
        return lock


class FileLock:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self._thread_lock = _process_lock(self.path)
        self._handle: Any = None

    def __enter__(self) -> "FileLock":
        self._thread_lock.acquire()
        try:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            self._handle = open(self.path, "a+b")
            self._handle.seek(0, os.SEEK_END)
            if self._handle.tell() == 0:
                self._handle.write(b"\0")
                self._handle.flush()
            if os.name == "nt":
                import msvcrt

                self._handle.seek(0)
                msvcrt.locking(self._handle.fileno(), msvcrt.LK_LOCK, 1)
            else:
                import fcntl

                fcntl.flock(self._handle.fileno(), fcntl.LOCK_EX)
            return self
        except Exception:
            if self._handle is not None:
                self._handle.close()
                self._handle = None
            self._thread_lock.release()
            raise

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        try:
            if self._handle is not None:
                if os.name == "nt":
                    import msvcrt

                    self._handle.seek(0)
                    msvcrt.locking(self._handle.fileno(), msvcrt.LK_UNLCK, 1)
                else:
                    import fcntl

                    fcntl.flock(self._handle.fileno(), fcntl.LOCK_UN)
                self._handle.close()
                self._handle = None
        finally:
            self._thread_lock.release()


def _track_signature(track: Any) -> dict[str, Any]:
    value = track.declaration()
    value["payload_sha256"] = track.payload_sha256
    value["payload_length"] = len(track.payload)
    value["payload_encoding"] = track.payload_encoding
    return value


def checkpoint_signature(
    model: ContainerModel,
    target_version: str,
    parameter_version: str,
    algorithm_version: str,
    per_thread_buffer_limit: int,
) -> str:
    value = {
        "format": CHECKPOINT_FORMAT,
        "model": {
            "model_id": model.model_id,
            "title": model.title,
            "source_version": model.source_version,
            "timebase": model.timebase.to_dict(),
            "duration_ticks": model.duration_ticks,
            "tracks": [_track_signature(track) for track in sorted(model.tracks, key=lambda item: item.track_id)],
            "chapters": [chapter.declaration() for chapter in model.chapters],
            "cover_sha256": sha256_bytes(model.cover),
            "cover_mime": model.cover_mime,
            "tags": dict(sorted(model.tags.items())),
            "editorial": dict(sorted(model.editorial.items())),
        },
        "target_version": target_version,
        "parameter_version": parameter_version,
        "algorithm_version": algorithm_version,
        "per_thread_buffer_limit": per_thread_buffer_limit,
    }
    return sha256_bytes(canonical_json_bytes(value))


def snapshot_identifier(
    request_signature: str,
    metadata_version: str,
    parameter_version: str,
    algorithm_version: str,
) -> str:
    value = {
        "request_signature": request_signature,
        "metadata_version": metadata_version,
        "parameter_version": parameter_version,
        "algorithm_version": algorithm_version,
    }
    return sha256_bytes(canonical_json_bytes(value))[:32]


class CheckpointManager:
    def __init__(
        self,
        root: str | Path,
        *,
        request_signature: str,
        metadata_version: str,
        source_version: str,
        parameter_version: str,
        algorithm_version: str,
        per_thread_buffer_limit: int,
        track_ids: list[str],
    ) -> None:
        self.root = Path(root)
        self.request_signature = request_signature
        self.metadata_version = metadata_version
        self.source_version = source_version
        self.parameter_version = parameter_version
        self.algorithm_version = algorithm_version
        self.per_thread_buffer_limit = per_thread_buffer_limit
        self.track_ids = sorted(track_ids)
        self.snapshot_id = snapshot_identifier(
            request_signature,
            metadata_version,
            parameter_version,
            algorithm_version,
        )
        self.snapshot_dir = self.root / self.snapshot_id
        self.segment_dir = self.snapshot_dir / "segments"
        self.manifest_path = self.snapshot_dir / "manifest.json"
        self.lock = FileLock(self.snapshot_dir / ".lock")
        self.invalidated = False
        self.invalidation_reason = ""
        try:
            self.snapshot_dir.mkdir(parents=True, exist_ok=True)
            self.segment_dir.mkdir(parents=True, exist_ok=True)
        except OSError as exc:
            fail(E_IO, "$.checkpoint", f"cannot create checkpoint directory: {exc}")
        with self.lock:
            self._open_locked()

    def _initial_manifest(self) -> dict[str, Any]:
        return {
            "format": CHECKPOINT_FORMAT,
            "snapshot_id": self.snapshot_id,
            "request_signature": self.request_signature,
            "metadata_version": self.metadata_version,
            "source_version": self.source_version,
            "parameter_version": self.parameter_version,
            "algorithm_version": self.algorithm_version,
            "per_thread_buffer_limit": self.per_thread_buffer_limit,
            "track_ids": self.track_ids,
            "segments": {
                track_id: {
                    "index": index,
                    "status": "pending",
                    "file": None,
                    "sha256": None,
                    "error": None,
                }
                for index, track_id in enumerate(self.track_ids)
            },
            "state": "partial",
            "completed_count": 0,
        }

    def _open_locked(self) -> None:
        if not self.manifest_path.exists():
            self.manifest = self._initial_manifest()
            self._write_manifest_locked()
            return
        try:
            manifest = read_json(self.manifest_path)
        except (OSError, ValueError) as exc:
            self._invalidate_locked(f"manifest cannot be read: {exc}")
            return
        reason = self._validate_manifest_locked(manifest)
        if reason:
            self._invalidate_locked(reason)
            return
        self.manifest = manifest

    def _validate_manifest_locked(self, manifest: Any) -> str:
        if not isinstance(manifest, dict):
            return "manifest is not an object"
        expected_values = {
            "format": CHECKPOINT_FORMAT,
            "snapshot_id": self.snapshot_id,
            "request_signature": self.request_signature,
            "metadata_version": self.metadata_version,
            "source_version": self.source_version,
            "parameter_version": self.parameter_version,
            "algorithm_version": self.algorithm_version,
            "per_thread_buffer_limit": self.per_thread_buffer_limit,
            "track_ids": self.track_ids,
        }
        for key, expected in expected_values.items():
            if manifest.get(key) != expected:
                return f"manifest {key} mismatch"
        segments = manifest.get("segments")
        if not isinstance(segments, dict):
            return "manifest segment table is missing"
        if sorted(segments) != self.track_ids:
            return "manifest segment id set mismatch"
        expected_files: set[str] = set()
        completed = 0
        failed = 0
        for index, track_id in enumerate(self.track_ids):
            segment = segments.get(track_id)
            if not isinstance(segment, dict):
                return f"segment record for {track_id} is invalid"
            if segment.get("index") != index:
                return f"segment index mismatch for {track_id}"
            status = segment.get("status")
            if status not in {"pending", "complete", "failed"}:
                return f"segment status mismatch for {track_id}"
            file_value = segment.get("file")
            if status == "complete":
                completed += 1
                if not isinstance(file_value, str):
                    return f"completed segment {track_id} has no file"
                relative = Path(file_value)
                if relative.is_absolute() or ".." in relative.parts:
                    return f"completed segment {track_id} has an unsafe path"
                segment_path = self.snapshot_dir / relative
                if not segment_path.exists():
                    return f"completed segment {track_id} file is missing"
                try:
                    segment_bytes = segment_path.read_bytes()
                except OSError:
                    return f"completed segment {track_id} cannot be read"
                measured = sha256_bytes(segment_bytes)
                if measured != segment.get("sha256"):
                    return f"completed segment {track_id} hash mismatch"
                try:
                    parsed = parse_segment_bytes(
                        segment_bytes,
                        expected_track_id=track_id,
                        expected_metadata_version=self.metadata_version,
                        expected_source_version=self.source_version,
                        expected_parameter_version=self.parameter_version,
                        expected_algorithm_version=self.algorithm_version,
                    )
                except WorkbenchError as exc:
                    return f"completed segment {track_id} invalid: {exc.code} {exc.field_path}"
                if parsed.segment_sha256 != measured:
                    return f"completed segment {track_id} content hash mismatch"
                try:
                    validate_track_payload(
                        segment_to_model_track(parsed),
                        f"$.segments.{track_id}",
                    )
                except WorkbenchError as exc:
                    return f"completed segment {track_id} payload invalid: {exc.code} {exc.field_path}"
                expected_files.add(relative.as_posix())
            else:
                if status == "failed":
                    failed += 1
                if file_value is not None:
                    return f"non-complete segment {track_id} references a file"
        actual_files = {
            path.relative_to(self.snapshot_dir).as_posix()
            for path in self.segment_dir.rglob("*")
            if path.is_file()
        }
        if actual_files != expected_files:
            return "segment directory does not match the manifest"
        expected_state = "complete" if completed == len(self.track_ids) else ("failed" if failed else "partial")
        if manifest.get("completed_count") != completed:
            return "completed segment count mismatch"
        if manifest.get("state") != expected_state:
            return "snapshot state mismatch"
        return ""

    def _invalidate_locked(self, reason: str) -> None:
        self.invalidated = True
        self.invalidation_reason = reason
        try:
            for path in sorted(self.segment_dir.rglob("*"), reverse=True):
                if path.is_file():
                    path.unlink()
                elif path.is_dir():
                    path.rmdir()
            if self.manifest_path.exists():
                self.manifest_path.unlink()
        except OSError as exc:
            fail(E_IO, "$.checkpoint", f"cannot invalidate checkpoint snapshot: {exc}")
        self.manifest = self._initial_manifest()
        self._write_manifest_locked()

    def _write_manifest_locked(self) -> None:
        self._update_state()
        try:
            atomic_write_bytes(self.manifest_path, canonical_json_bytes(self.manifest) + b"\n")
        except OSError as exc:
            fail(E_IO, "$.checkpoint.manifest", f"cannot write checkpoint manifest: {exc}")

    def _update_state(self) -> None:
        statuses = [segment["status"] for segment in self.manifest["segments"].values()]
        self.manifest["completed_count"] = statuses.count("complete")
        if self.manifest["completed_count"] == len(statuses):
            self.manifest["state"] = "complete"
        elif "failed" in statuses:
            self.manifest["state"] = "failed"
        else:
            self.manifest["state"] = "partial"

    def _load_manifest_locked(self) -> dict[str, Any]:
        try:
            return read_json(self.manifest_path)
        except (OSError, ValueError) as exc:
            fail(E_CORRUPT, "$.checkpoint.manifest", f"checkpoint manifest became unreadable: {exc}")

    def force_pending(self, track_id: str) -> None:
        with self.lock:
            manifest = self._load_manifest_locked()
            segment = manifest["segments"].get(track_id)
            if segment is None:
                fail(
                    E_CORRUPT,
                    "$.checkpoint.track_id",
                    "unknown checkpoint segment",
                    measured=track_id,
                )
            file_value = segment.get("file")
            if file_value:
                path = self.snapshot_dir / file_value
                if path.exists():
                    path.unlink()
            segment["status"] = "pending"
            segment["file"] = None
            segment["sha256"] = None
            segment["error"] = None
            self.manifest = manifest
            self._write_manifest_locked()

    def record_success(self, track_id: str, segment_bytes: bytes) -> None:
        with self.lock:
            manifest = self._load_manifest_locked()
            segment = manifest["segments"].get(track_id)
            if segment is None:
                fail(E_CORRUPT, "$.checkpoint.track_id", "unknown checkpoint segment", measured=track_id)
            index = segment["index"]
            safe_track_id = "".join(
                character if character.isalnum() or character in {"-", "_"} else "_"
                for character in track_id
            )
            relative = f"segments/{index:03d}_{safe_track_id}.mfs"
            segment_path = self.snapshot_dir / relative
            try:
                atomic_write_bytes(segment_path, segment_bytes)
            except OSError as exc:
                fail(E_IO, f"$.checkpoint.segments.{track_id}", f"cannot write segment: {exc}")
            segment["status"] = "complete"
            segment["file"] = relative
            segment["sha256"] = sha256_bytes(segment_bytes)
            segment["error"] = None
            self.manifest = manifest
            self._write_manifest_locked()

    def record_failure(self, track_id: str, error: WorkbenchError) -> None:
        with self.lock:
            manifest = self._load_manifest_locked()
            segment = manifest["segments"].get(track_id)
            if segment is None:
                return
            file_value = segment.get("file")
            if file_value:
                path = self.snapshot_dir / file_value
                if path.exists():
                    path.unlink()
            segment["status"] = "failed"
            segment["file"] = None
            segment["sha256"] = None
            segment["error"] = error.to_dict()
            self.manifest = manifest
            self._write_manifest_locked()

    def completed_segment_bytes(self) -> dict[str, bytes]:
        result: dict[str, bytes] = {}
        with self.lock:
            manifest = self._load_manifest_locked()
            reason = self._validate_manifest_locked(manifest)
            if reason:
                self._invalidate_locked(reason)
                fail(E_CORRUPT, "$.checkpoint", f"checkpoint snapshot invalidated: {reason}")
            for track_id, segment in manifest["segments"].items():
                if segment["status"] == "complete":
                    result[track_id] = (self.snapshot_dir / segment["file"]).read_bytes()
            self.manifest = manifest
        return result

    def completed_segments(self) -> dict[str, ParsedSegment]:
        result: dict[str, ParsedSegment] = {}
        for track_id, data in self.completed_segment_bytes().items():
            result[track_id] = parse_segment_bytes(
                data,
                expected_track_id=track_id,
                expected_metadata_version=self.metadata_version,
                expected_source_version=self.source_version,
                expected_parameter_version=self.parameter_version,
                expected_algorithm_version=self.algorithm_version,
            )
        return result

    def snapshot(self) -> dict[str, Any]:
        with self.lock:
            manifest = self._load_manifest_locked()
            return copy.deepcopy(manifest)

    def state(self) -> str:
        return str(self.snapshot().get("state", "unknown"))
