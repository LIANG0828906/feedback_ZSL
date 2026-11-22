"""Atomic checkpoint storage with cross-process serialization."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
import threading
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterator

from .errors import ErrorCode, WorkbenchError


@dataclass(frozen=True)
class CheckpointRead:
    payload: dict[str, Any] | None
    digest: str | None
    reason: str | None = None

    @property
    def ok(self) -> bool:
        return self.payload is not None


class CheckpointStore:
    """Stores one complete snapshot in a single JSON envelope."""

    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.lock_path = self.path.with_suffix(self.path.suffix + ".lock")
        self._thread_lock = threading.RLock()

    @contextmanager
    def _file_lock(self) -> Iterator[None]:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        handle = self.lock_path.open("a+b")
        try:
            if os.name == "nt":
                import msvcrt
                import time

                handle.seek(0, os.SEEK_END)
                if handle.tell() == 0:
                    handle.write(b"\0")
                    handle.flush()
                handle.seek(0)
                while True:
                    try:
                        msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
                        break
                    except OSError:
                        time.sleep(0.05)
                try:
                    yield
                finally:
                    handle.seek(0)
                    msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                import fcntl

                fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
                try:
                    yield
                finally:
                    fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
        finally:
            handle.close()

    def save(self, payload: dict[str, Any]) -> str:
        canonical = json.dumps(
            payload,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=True,
        )
        digest = hashlib.sha256(canonical.encode("utf-8")).hexdigest()
        envelope = {"digest": digest, "payload": payload}
        serialized = json.dumps(
            envelope,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=True,
        )
        with self._thread_lock:
            with self._file_lock():
                self.path.parent.mkdir(parents=True, exist_ok=True)
                descriptor, temp_name = tempfile.mkstemp(
                    prefix=f".{self.path.name}.",
                    suffix=".tmp",
                    dir=self.path.parent,
                )
                try:
                    with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
                        handle.write(serialized)
                        handle.flush()
                        os.fsync(handle.fileno())
                    os.replace(temp_name, self.path)
                    if os.name != "nt":
                        directory_fd = os.open(self.path.parent, os.O_RDONLY)
                        try:
                            os.fsync(directory_fd)
                        finally:
                            os.close(directory_fd)
                except BaseException:
                    try:
                        os.unlink(temp_name)
                    except FileNotFoundError:
                        pass
                    raise
        return digest

    def read(self) -> CheckpointRead:
        with self._thread_lock:
            with self._file_lock():
                if not self.path.exists():
                    return CheckpointRead(None, None, "checkpoint does not exist")
                try:
                    with self.path.open("r", encoding="utf-8") as handle:
                        envelope = json.load(handle)
                except (OSError, json.JSONDecodeError) as exc:
                    return CheckpointRead(None, None, f"checkpoint is unreadable: {exc}")
        if not isinstance(envelope, dict) or not isinstance(envelope.get("payload"), dict):
            return CheckpointRead(None, None, "checkpoint envelope is invalid")
        payload = envelope["payload"]
        canonical = json.dumps(
            payload,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=True,
        )
        digest = hashlib.sha256(canonical.encode("utf-8")).hexdigest()
        if digest != envelope.get("digest"):
            return CheckpointRead(None, None, "checkpoint digest mismatch")
        return CheckpointRead(payload, digest)


def require_checkpoint_digest(store: CheckpointStore, expected: str) -> None:
    result = store.read()
    if not result.ok or result.digest != expected:
        raise WorkbenchError(
            ErrorCode.E_IO,
            "checkpoint verification failed",
            field="checkpoint",
            details={"expected": expected, "actual": result.digest, "reason": result.reason},
        )
