"""Version-gated, atomically committed project snapshots."""

from __future__ import annotations

import json
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .constants import (
    CONVERSION_RULES_VERSION,
    DOMAIN_MODEL_VERSION,
    SNAPSHOT_FORMAT_VERSION,
)
from .errors import SnapshotError
from .model import Score
from .resources import serial_persist_write

try:  # fcntl is POSIX-only; the workbench targets Windows too.
    import fcntl
except ImportError:  # pragma: no cover - exercised on Windows
    fcntl = None


SNAPSHOT_NAME = "snapshot.json"
LOCK_NAME = "snapshot.lock"


@dataclass
class SnapshotBundle:
    score: Score
    source_version: str | None
    source_path: str | None
    envelope: dict[str, Any]


class SnapshotStore:
    def __init__(self, directory: str | Path):
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.snapshot_path = self.directory / SNAPSHOT_NAME
        self.lock_path = self.directory / LOCK_NAME

    # ---- locking ------------------------------------------------------
    def _file_lock(self):
        handle = open(self.lock_path, "a+", encoding="utf-8")
        if fcntl is not None:
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
        return handle

    # ---- write --------------------------------------------------------
    def save(self, score: Score) -> dict[str, Any]:
        envelope = {
            "snapshot_format_version": SNAPSHOT_FORMAT_VERSION,
            "domain_model_version": DOMAIN_MODEL_VERSION,
            "conversion_rules_version": CONVERSION_RULES_VERSION,
            "source_version": score.source_version,
            "source_path": score.source_path,
            "score": score.to_dict(),
        }
        payload = json.dumps(envelope, indent=2, sort_keys=True)
        with serial_persist_write():
            lock_handle = self._file_lock()
            try:
                fd, tmp_name = tempfile.mkstemp(
                    prefix=".snapshot-", suffix=".tmp", dir=str(self.directory)
                )
                try:
                    with os.fdopen(fd, "w", encoding="utf-8") as handle:
                        handle.write(payload)
                        handle.flush()
                        os.fsync(handle.fileno())
                    os.replace(tmp_name, self.snapshot_path)
                    directory_fd = os.open(str(self.directory), os.O_RDONLY)
                    try:
                        os.fsync(directory_fd)
                    finally:
                        os.close(directory_fd)
                except BaseException:
                    if os.path.exists(tmp_name):
                        os.unlink(tmp_name)
                    raise
            finally:
                if fcntl is not None:
                    fcntl.flock(lock_handle.fileno(), fcntl.LOCK_UN)
                lock_handle.close()
        return envelope

    # ---- read ---------------------------------------------------------
    def exists(self) -> bool:
        return self.snapshot_path.exists()

    def load(self) -> SnapshotBundle:
        if not self.exists():
            raise SnapshotError(
                "No snapshot exists in the project directory",
                {"directory": str(self.directory)},
            )
        try:
            with open(self.snapshot_path, "r", encoding="utf-8") as handle:
                envelope = json.load(handle)
        except (OSError, json.JSONDecodeError) as exc:
            raise SnapshotError(
                "Snapshot is unreadable or torn; reload from source required",
                {"file_path": str(self.snapshot_path), "cause": str(exc)},
            ) from exc
        mismatch = self._version_mismatch(envelope)
        if mismatch:
            self.invalidate()
            raise SnapshotError(
                "Snapshot versions are incompatible; snapshot invalidated",
                mismatch,
            )
        try:
            score = Score.from_dict(envelope["score"])
        except KeyError as exc:
            self.invalidate()
            raise SnapshotError(
                "Snapshot payload is structurally incomplete; invalidated",
                {"missing_field": str(exc)},
            ) from exc
        score.source_version = envelope.get("source_version")
        score.source_path = envelope.get("source_path")
        return SnapshotBundle(
            score=score,
            source_version=envelope.get("source_version"),
            source_path=envelope.get("source_path"),
            envelope=envelope,
        )

    @staticmethod
    def _version_mismatch(envelope: dict[str, Any]) -> dict[str, Any] | None:
        expected = {
            "snapshot_format_version": SNAPSHOT_FORMAT_VERSION,
            "domain_model_version": DOMAIN_MODEL_VERSION,
            "conversion_rules_version": CONVERSION_RULES_VERSION,
        }
        actual = {key: envelope.get(key) for key in expected}
        if actual != expected:
            return {"expected": expected, "actual": actual}
        return None

    def invalidate(self) -> None:
        """Discard a stale/torn snapshot as a whole (no partial reuse)."""
        with serial_persist_write():
            if self.snapshot_path.exists():
                os.unlink(self.snapshot_path)


def open_or_reload(store: SnapshotStore, reload_callable):
    """Return a bundle from the snapshot, or rebuild via ``reload_callable``."""
    if store.exists():
        try:
            return store.load()
        except SnapshotError:
            pass
    project = reload_callable()
    store.save(project.score)
    return SnapshotBundle(
        score=project.score,
        source_version=project.source_version,
        source_path=project.source_path,
        envelope={},
    )
