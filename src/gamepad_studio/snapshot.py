"""Atomic JSON snapshots with all-or-nothing version validation."""

from __future__ import annotations

import contextlib
import json
import os
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterator

try:
    import fcntl
except ImportError:  # pragma: no cover - Windows fallback
    fcntl = None  # type: ignore[assignment]

from .errors import ALGORITHM_VERSION, SnapshotError
from .pipeline import CacheEntry, Pipeline

SNAPSHOT_FORMAT_VERSION = 1
METADATA_VERSION_FIELDS = (
    "input_hash",
    "binding_version",
    "algorithm_version",
    "graph_signature",
    "param_version",
)
_PATH_LOCKS: dict[str, threading.RLock] = {}
_PATH_LOCKS_GUARD = threading.Lock()


def _thread_lock_for(path: Path) -> threading.RLock:
    key = str(path.resolve())
    with _PATH_LOCKS_GUARD:
        lock = _PATH_LOCKS.get(key)
        if lock is None:
            lock = threading.RLock()
            _PATH_LOCKS[key] = lock
        return lock


@dataclass(frozen=True)
class SnapshotWriteReport:
    path: str
    node_count: int
    replaced: bool
    previous_readable: bool
    version_changes: tuple[str, ...]
    invalidated_node_count: int | None

    def to_dict(self) -> dict[str, Any]:
        return {
            "path": self.path,
            "node_count": self.node_count,
            "replaced": self.replaced,
            "previous_readable": self.previous_readable,
            "version_changes": list(self.version_changes),
            "invalidated_node_count": self.invalidated_node_count,
        }


@dataclass(frozen=True)
class SnapshotLoadResult:
    path: str
    valid: bool
    reason: str
    invalidated_node_count: int | None
    metadata: dict[str, Any]
    entries: dict[str, CacheEntry]
    version_mismatches: tuple[str, ...] = ()

    def to_dict(self) -> dict[str, Any]:
        return {
            "path": self.path,
            "valid": self.valid,
            "reason": self.reason,
            "invalidated_node_count": self.invalidated_node_count,
            "metadata": self.metadata,
            "version_mismatches": list(self.version_mismatches),
            "entry_count": len(self.entries),
        }


class SnapshotStore:
    """Serialized snapshots guarded by a process thread lock and file lock."""

    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.lock_path = self.path.with_suffix(self.path.suffix + ".lock")
        self._thread_lock = _thread_lock_for(self.path)

    @contextlib.contextmanager
    def _locked(self) -> Iterator[None]:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        with self._thread_lock:
            with self.lock_path.open("a+", encoding="utf-8") as lock_handle:
                if fcntl is not None:
                    fcntl.flock(lock_handle.fileno(), fcntl.LOCK_EX)
                try:
                    yield
                finally:
                    if fcntl is not None:
                        fcntl.flock(lock_handle.fileno(), fcntl.LOCK_UN)

    @staticmethod
    def _metadata_from_pipeline(pipeline: Pipeline) -> dict[str, Any]:
        return {
            "format_version": SNAPSHOT_FORMAT_VERSION,
            "input_hash": pipeline.bundle.input_hash,
            "binding_version": pipeline.bundle.binding_version,
            "algorithm_version": ALGORITHM_VERSION,
            "graph_signature": pipeline.graph_signature,
            "param_version": pipeline.param_version,
            "device_id": pipeline.device.id,
            "event_count": len(pipeline.bundle.events),
            "node_count": len(pipeline.order),
        }

    @staticmethod
    def _read_payload_unsafe(path: Path) -> dict[str, Any]:
        try:
            with path.open("r", encoding="utf-8") as handle:
                payload = json.load(handle)
        except FileNotFoundError as exc:
            raise SnapshotError("snapshot does not exist", field="snapshot") from exc
        except (OSError, json.JSONDecodeError) as exc:
            raise SnapshotError("snapshot is unreadable", field="snapshot") from exc
        if not isinstance(payload, dict):
            raise SnapshotError("snapshot root must be an object", field="snapshot")
        if payload.get("format_version") != SNAPSHOT_FORMAT_VERSION:
            raise SnapshotError("snapshot format version is unsupported", field="format_version")
        return payload

    def _version_changes(
        self, previous: dict[str, Any], current: dict[str, Any]
    ) -> tuple[str, ...]:
        changes = []
        for field in METADATA_VERSION_FIELDS:
            if previous.get(field) != current.get(field):
                changes.append(field)
        return tuple(changes)

    def write(
        self,
        pipeline: Pipeline,
        *,
        extra_metadata: dict[str, Any] | None = None,
    ) -> SnapshotWriteReport:
        metadata = self._metadata_from_pipeline(pipeline)
        if extra_metadata:
            metadata.update(extra_metadata)
        manifest = pipeline.cache_manifest()
        if len(manifest["entries"]) != len(pipeline.order):
            raise SnapshotError(
                "pipeline cache is incomplete; refusing to write a partial snapshot",
                field="cache",
                context={
                    "expected_nodes": len(pipeline.order),
                    "cached_nodes": len(manifest["entries"]),
                },
            )
        payload = {
            "format_version": SNAPSHOT_FORMAT_VERSION,
            "algorithm_version": ALGORITHM_VERSION,
            "created_by": "padweave-studio",
            "metadata": metadata,
            "entries": manifest["entries"],
        }
        text = json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        with self._locked():
            previous_payload: dict[str, Any] | None = None
            previous_readable = False
            previous_exists = self.path.exists()
            if previous_exists:
                try:
                    previous_payload = self._read_payload_unsafe(self.path)
                    previous_readable = True
                except SnapshotError:
                    previous_payload = None
            if previous_payload is None:
                version_changes = ("initial_write",) if not previous_exists else ("previous_snapshot_corrupt",)
                invalidated_nodes = None if previous_exists else 0
            else:
                previous_metadata = previous_payload.get("metadata", {})
                version_changes = self._version_changes(previous_metadata, metadata)
                invalidated_nodes = (
                    int(previous_metadata.get("node_count", 0))
                    if version_changes
                    else 0
                )
            temporary = self.path.with_name(
                f".{self.path.name}.{os.getpid()}.{threading.get_ident()}.tmp"
            )
            try:
                with temporary.open("w", encoding="utf-8") as handle:
                    handle.write(text)
                    handle.flush()
                    os.fsync(handle.fileno())
                os.replace(temporary, self.path)
                try:
                    directory_fd = os.open(self.path.parent, os.O_DIRECTORY)
                except (AttributeError, OSError):
                    directory_fd = None
                if directory_fd is not None:
                    try:
                        os.fsync(directory_fd)
                    finally:
                        os.close(directory_fd)
            finally:
                if temporary.exists():
                    try:
                        temporary.unlink()
                    except OSError:
                        pass
        return SnapshotWriteReport(
            path=str(self.path),
            node_count=len(manifest["entries"]),
            replaced=previous_exists,
            previous_readable=previous_readable if previous_exists else False,
            version_changes=version_changes,
            invalidated_node_count=invalidated_nodes,
        )

    def load(self, pipeline: Pipeline) -> SnapshotLoadResult:
        with self._locked():
            if not self.path.exists():
                return SnapshotLoadResult(
                    path=str(self.path),
                    valid=False,
                    reason="snapshot_missing",
                    invalidated_node_count=0,
                    metadata={},
                    entries={},
                )
            try:
                payload = self._read_payload_unsafe(self.path)
            except SnapshotError as exc:
                return SnapshotLoadResult(
                    path=str(self.path),
                    valid=False,
                    reason="snapshot_corrupt",
                    invalidated_node_count=None,
                    metadata={},
                    entries={},
                    version_mismatches=(exc.message,),
                )
            metadata = payload.get("metadata", {})
            expected = self._metadata_from_pipeline(pipeline)
            mismatches = self._version_changes(metadata, expected)
            raw_entries = payload.get("entries", {})
            node_count = (
                len(raw_entries)
                if isinstance(raw_entries, dict)
                else int(metadata.get("node_count", 0) or 0)
            )
            if mismatches:
                return SnapshotLoadResult(
                    path=str(self.path),
                    valid=False,
                    reason="version_mismatch",
                    invalidated_node_count=node_count,
                    metadata=metadata,
                    entries={},
                    version_mismatches=mismatches,
                )
            if not isinstance(raw_entries, dict):
                return SnapshotLoadResult(
                    path=str(self.path),
                    valid=False,
                    reason="snapshot_corrupt",
                    invalidated_node_count=None,
                    metadata=metadata,
                    entries={},
                )
            try:
                entries = {
                    str(node_id): CacheEntry.from_dict(entry)
                    for node_id, entry in raw_entries.items()
                }
            except (KeyError, TypeError, ValueError):
                return SnapshotLoadResult(
                    path=str(self.path),
                    valid=False,
                    reason="snapshot_corrupt",
                    invalidated_node_count=len(raw_entries),
                    metadata=metadata,
                    entries={},
                )
            errors = pipeline.validate_cache_entries(entries)
            if errors:
                return SnapshotLoadResult(
                    path=str(self.path),
                    valid=False,
                    reason="cache_chain_mismatch",
                    invalidated_node_count=len(entries),
                    metadata=metadata,
                    entries={},
                    version_mismatches=tuple(errors),
                )
            return SnapshotLoadResult(
                path=str(self.path),
                valid=True,
                reason="snapshot_valid",
                invalidated_node_count=0,
                metadata=metadata,
                entries=entries,
            )


__all__ = [
    "SNAPSHOT_FORMAT_VERSION",
    "SnapshotLoadResult",
    "SnapshotStore",
    "SnapshotWriteReport",
]
