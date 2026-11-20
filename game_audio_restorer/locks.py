"""Cross-platform non-blocking per-source task lock."""

from __future__ import annotations

import hashlib
import os
import tempfile
import threading
from pathlib import Path
from typing import IO

from .errors import BusinessError, ErrorCode

_guard = threading.Lock()
_held_in_process: set[str] = set()


def _lock_path(source_path: str | os.PathLike[str]) -> Path:
    canonical = os.path.normcase(os.path.realpath(os.fspath(source_path)))
    digest = hashlib.sha256(canonical.encode("utf-8")).hexdigest()
    directory = Path(tempfile.gettempdir()) / "game_audio_restorer_locks"
    directory.mkdir(parents=True, exist_ok=True)
    return directory / f"{digest}.lock"


class TaskLock:
    def __init__(self, source_path: str | os.PathLike[str]) -> None:
        self.source_path = os.fspath(source_path)
        self.path = _lock_path(source_path)
        self._key = str(self.path.resolve())
        self._handle: IO[bytes] | None = None
        self._owned = False

    def acquire(self) -> "TaskLock":
        with _guard:
            if self._key in _held_in_process:
                raise self._busy()
            _held_in_process.add(self._key)
        try:
            handle = open(self.path, "a+b")
            if os.name == "nt":
                import msvcrt

                handle.seek(0, os.SEEK_END)
                if handle.tell() == 0:
                    handle.write(b"\0")
                    handle.flush()
                handle.seek(0)
                try:
                    msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
                except OSError as exc:
                    handle.close()
                    raise self._busy() from exc
            else:
                import fcntl

                try:
                    fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                except OSError as exc:
                    handle.close()
                    raise self._busy() from exc
            self._handle = handle
            self._owned = True
            return self
        except BusinessError:
            with _guard:
                _held_in_process.discard(self._key)
            raise
        except Exception:
            with _guard:
                _held_in_process.discard(self._key)
            raise

    def _busy(self) -> BusinessError:
        return BusinessError(
            ErrorCode.BUSY,
            "同一 WAV 已有修复任务运行",
            {"source": self.source_path, "lock_file": str(self.path)},
        )

    def release(self) -> None:
        if self._handle is None:
            return
        try:
            if os.name == "nt":
                import msvcrt

                self._handle.seek(0)
                try:
                    msvcrt.locking(self._handle.fileno(), msvcrt.LK_UNLCK, 1)
                except OSError:
                    pass
            else:
                import fcntl

                try:
                    fcntl.flock(self._handle.fileno(), fcntl.LOCK_UN)
                except OSError:
                    pass
        finally:
            self._handle.close()
            self._handle = None
        if self._owned:
            with _guard:
                _held_in_process.discard(self._key)
            self._owned = False

    def __enter__(self) -> "TaskLock":
        return self.acquire()

    def __exit__(self, _exc_type: object, _exc: object, _tb: object) -> None:
        self.release()
