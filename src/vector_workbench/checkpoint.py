from __future__ import annotations

import contextlib
import hashlib
import json
import os
import re
import shutil
import threading
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterator, Mapping, Sequence

from . import CHECKPOINT_SCHEMA_VERSION
from .errors import ErrorCode, WorkbenchError
from .model import Stage

_SAFE_ID = re.compile(r"[^A-Za-z0-9_.-]+")


class _InterProcessLock:
    def __init__(self, path: Path) -> None:
        self.path = path
        self._handle: Any = None

    def __enter__(self) -> "_InterProcessLock":
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._handle = self.path.open("a+b")
        self._handle.seek(0, os.SEEK_END)
        if self._handle.tell() == 0:
            self._handle.write(b"\0")
            self._handle.flush()
        self._handle.seek(0)
        if os.name == "nt":
            import msvcrt

            msvcrt.locking(self._handle.fileno(), msvcrt.LK_LOCK, 1)
        else:
            import fcntl

            fcntl.flock(self._handle.fileno(), fcntl.LOCK_EX)
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        if self._handle is None:
            return
        try:
            if os.name == "nt":
                import msvcrt

                self._handle.seek(0)
                msvcrt.locking(self._handle.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                import fcntl

                fcntl.flock(self._handle.fileno(), fcntl.LOCK_UN)
        finally:
            self._handle.close()
            self._handle = None


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _fsync_directory(path: Path) -> None:
    if os.name == "nt":
        return
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _atomic_write(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.{os.getpid()}.{threading.get_ident()}.tmp")
    with temporary.open("wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)
    _fsync_directory(path.parent)


@dataclass(frozen=True)
class CheckpointEntry:
    sequence: int
    path_id: str
    stage: Stage
    sha256: str
    size: int
    file: str
    payload: bytes = field(repr=False)

    def token(self) -> tuple[str, str]:
        return self.path_id, self.stage.value


@dataclass
class SnapshotLoad:
    entries: list[CheckpointEntry]
    invalidated: bool = False
    reason: str | None = None
    metadata: dict[str, Any] | None = None

    def tokens(self) -> list[tuple[str, str]]:
        return [entry.token() for entry in self.entries]


class CheckpointStore:
    """Versioned, atomically written, path-order serialized checkpoint snapshot."""

    def __init__(
        self,
        root: str | Path,
        *,
        scene_id: str,
        scene_hash: str,
        param_hash: str,
        algorithm_version: str,
        expected_order: Sequence[tuple[str, str]],
        parameter_details: Mapping[str, Any] | None = None,
    ) -> None:
        self.root = Path(root)
        self.snapshot_dir = self.root / "snapshot"
        self.manifest_path = self.snapshot_dir / "manifest.json"
        self.scene_id = scene_id
        self.scene_hash = scene_hash
        self.param_hash = param_hash
        self.algorithm_version = algorithm_version
        self.parameter_details = dict(parameter_details or {})
        self.expected_order = [
            (
                path_id,
                stage.value if isinstance(stage, Stage) else str(stage),
            )
            for path_id, stage in expected_order
        ]
        self._thread_lock = threading.RLock()
        self._process_lock = _InterProcessLock(self.root / ".checkpoint.lock")
        self.root.mkdir(parents=True, exist_ok=True)

    def _metadata(self) -> dict[str, Any]:
        return {
            "schema": "vector-workbench-checkpoint",
            "schema_version": CHECKPOINT_SCHEMA_VERSION,
            "scene_id": self.scene_id,
            "scene_hash": self.scene_hash,
            "param_hash": self.param_hash,
            "parameter_version": self.param_hash,
            "parameters": self.parameter_details,
            "algorithm_version": self.algorithm_version,
            "entries": [],
        }

    @contextlib.contextmanager
    def _locked(self) -> Iterator[None]:
        with self._thread_lock:
            with self._process_lock:
                yield

    def _serialize_manifest(self, metadata: dict[str, Any]) -> bytes:
        return (
            json.dumps(
                metadata,
                indent=2,
                ensure_ascii=False,
                sort_keys=False,
            )
            + "\n"
        ).encode("utf-8")

    def _write_manifest_locked(self, metadata: dict[str, Any]) -> None:
        _atomic_write(self.manifest_path, self._serialize_manifest(metadata))

    def _invalidated_snapshot_locked(self, reason: str) -> None:
        temporary = self.root / f".snapshot.new.{os.getpid()}.{uuid.uuid4().hex}"
        temporary.mkdir(parents=True, exist_ok=False)
        metadata = self._metadata()
        _atomic_write(temporary / "manifest.json", self._serialize_manifest(metadata))
        old = self.root / f".snapshot.old.{os.getpid()}.{uuid.uuid4().hex}"
        if self.snapshot_dir.exists():
            os.replace(self.snapshot_dir, old)
        os.replace(temporary, self.snapshot_dir)
        if old.exists():
            shutil.rmtree(old, ignore_errors=True)
        self.invalidated_reason = reason

    def reset(self, reason: str = "explicit full recompute") -> None:
        with self._locked():
            self._invalidated_snapshot_locked(reason)

    def load(self) -> SnapshotLoad:
        with self._locked():
            if not self.manifest_path.exists():
                self._invalidated_snapshot_locked("snapshot manifest was absent")
                return SnapshotLoad(
                    entries=[],
                    invalidated=True,
                    reason=self.invalidated_reason,
                    metadata=self._metadata(),
                )
            try:
                metadata = json.loads(self.manifest_path.read_text(encoding="utf-8"))
            except (OSError, UnicodeError, json.JSONDecodeError) as exc:
                self._invalidated_snapshot_locked(f"manifest decode failed: {exc}")
                return SnapshotLoad(
                    entries=[],
                    invalidated=True,
                    reason=self.invalidated_reason,
                    metadata=self._metadata(),
                )

            mismatch = self._version_mismatch(metadata)
            if mismatch is None:
                try:
                    entries = self._load_entries_locked(metadata)
                except WorkbenchError as exc:
                    mismatch = exc.message
            if mismatch is not None:
                self._invalidated_snapshot_locked(mismatch)
                return SnapshotLoad(
                    entries=[],
                    invalidated=True,
                    reason=mismatch,
                    metadata=self._metadata(),
                )
            return SnapshotLoad(entries=entries, metadata=metadata)

    def _version_mismatch(self, metadata: Any) -> str | None:
        if not isinstance(metadata, dict):
            return "manifest root is not an object"
        if metadata.get("schema") != "vector-workbench-checkpoint":
            return "checkpoint schema identity mismatch"
        if metadata.get("schema_version") != CHECKPOINT_SCHEMA_VERSION:
            return "checkpoint schema version mismatch"
        if metadata.get("scene_id") != self.scene_id:
            return "scene id mismatch"
        if metadata.get("scene_hash") != self.scene_hash:
            return "scene content hash mismatch"
        if metadata.get("param_hash") != self.param_hash:
            return "parameter version mismatch"
        if metadata.get("algorithm_version") != self.algorithm_version:
            return "algorithm version mismatch"
        return None

    def _load_entries_locked(self, metadata: dict[str, Any]) -> list[CheckpointEntry]:
        raw_entries = metadata.get("entries")
        if not isinstance(raw_entries, list):
            raise WorkbenchError(ErrorCode.E_STATE, "checkpoint entries must be an array")
        if len(raw_entries) > len(self.expected_order):
            raise WorkbenchError(ErrorCode.E_STATE, "checkpoint has too many stage entries")
        entries: list[CheckpointEntry] = []
        for sequence, raw in enumerate(raw_entries):
            if not isinstance(raw, dict):
                raise WorkbenchError(ErrorCode.E_STATE, "checkpoint entry is not an object")
            token = (raw.get("path_id"), raw.get("stage"))
            expected_token = self.expected_order[sequence]
            if token != expected_token:
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    f"checkpoint order mismatch at sequence {sequence}",
                )
            try:
                stage = Stage(str(raw["stage"]))
                relative = Path(str(raw["file"]))
                if relative.is_absolute() or ".." in relative.parts:
                    raise ValueError("unsafe payload path")
                file_path = self.snapshot_dir / relative
                payload = file_path.read_bytes()
                digest = _sha256(payload)
                if digest != raw.get("sha256"):
                    raise ValueError("payload hash mismatch")
                if len(payload) != int(raw.get("size", -1)):
                    raise ValueError("payload size mismatch")
            except (KeyError, ValueError, OSError) as exc:
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    f"checkpoint payload validation failed: {exc}",
                ) from exc
            entries.append(
                CheckpointEntry(
                    sequence=sequence,
                    path_id=str(raw["path_id"]),
                    stage=stage,
                    sha256=str(raw["sha256"]),
                    size=int(raw["size"]),
                    file=str(raw["file"]),
                    payload=payload,
                )
            )
        return entries

    def commit(self, path_id: str, stage: Stage, payload: bytes) -> CheckpointEntry:
        with self._locked():
            if not self.manifest_path.exists():
                self._invalidated_snapshot_locked("manifest missing before commit")
            try:
                metadata = json.loads(self.manifest_path.read_text(encoding="utf-8"))
            except (OSError, UnicodeError, json.JSONDecodeError) as exc:
                raise WorkbenchError(
                    ErrorCode.E_STAGE,
                    f"checkpoint manifest could not be read: {exc}",
                    path_id=path_id,
                    stage=stage.value,
                ) from exc
            mismatch = self._version_mismatch(metadata)
            if mismatch is not None:
                raise WorkbenchError(
                    ErrorCode.E_STAGE,
                    f"checkpoint metadata changed unexpectedly: {mismatch}",
                    path_id=path_id,
                    stage=stage.value,
                )
            entries = metadata.get("entries")
            if not isinstance(entries, list):
                raise WorkbenchError(
                    ErrorCode.E_STAGE,
                    "checkpoint entry list is invalid",
                    path_id=path_id,
                    stage=stage.value,
                )
            sequence = len(entries)
            if sequence >= len(self.expected_order):
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "checkpoint is already complete",
                    path_id=path_id,
                    stage=stage.value,
                )
            expected = self.expected_order[sequence]
            if expected != (path_id, stage.value):
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    f"out-of-order checkpoint commit; expected {expected[0]}:{expected[1]}",
                    path_id=path_id,
                    stage=stage.value,
                )
            digest = _sha256(payload)
            safe_path = _SAFE_ID.sub("_", path_id)[:80]
            relative = f"{sequence:06d}_{safe_path}_{stage.value}_{digest[:12]}.bin"
            _atomic_write(self.snapshot_dir / relative, payload)
            entry = {
                "sequence": sequence,
                "path_id": path_id,
                "stage": stage.value,
                "file": relative,
                "sha256": digest,
                "size": len(payload),
                "parameter_version": self.param_hash,
                "algorithm_version": self.algorithm_version,
            }
            metadata["entries"] = [*entries, entry]
            self._write_manifest_locked(metadata)
            return CheckpointEntry(
                sequence=sequence,
                path_id=path_id,
                stage=stage,
                sha256=digest,
                size=len(payload),
                file=relative,
                payload=payload,
            )

    def commit_sequence(self) -> list[tuple[str, str]]:
        with self._locked():
            if not self.manifest_path.exists():
                return []
            try:
                metadata = json.loads(self.manifest_path.read_text(encoding="utf-8"))
            except (OSError, UnicodeError, json.JSONDecodeError):
                return []
            if self._version_mismatch(metadata) is not None:
                return []
            entries = metadata.get("entries")
            if not isinstance(entries, list):
                return []
            return [(str(item.get("path_id")), str(item.get("stage"))) for item in entries]

