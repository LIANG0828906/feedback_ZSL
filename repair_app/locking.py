from __future__ import annotations

import os
import threading
from pathlib import Path

from .errors import AppError

try:
    import fcntl
except ImportError:  # pragma: no cover - Windows
    fcntl = None

try:
    import msvcrt
except ImportError:  # pragma: no cover - POSIX
    msvcrt = None


_process_locks: dict[str, threading.Lock] = {}
_process_locks_guard = threading.Lock()


def _process_lock_for(path: str) -> threading.Lock:
    with _process_locks_guard:
        return _process_locks.setdefault(path, threading.Lock())


class SessionLock:
    """Cross-process, non-blocking session lock.

    POSIX uses flock; Windows uses msvcrt byte-range locking. The operating
    system releases the lock automatically after forced termination.
    """

    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self._handle = None
        self._thread_lock: threading.Lock | None = None
        self._acquired = False

    def __enter__(self) -> "SessionLock":
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._thread_lock = _process_lock_for(str(self.path.resolve()))
        if not self._thread_lock.acquire(blocking=False):
            raise AppError("E_BUSY", "同一会话已有处理任务在运行")
        try:
            self._handle = open(self.path, "a+b")
            if self._handle.tell() == 0:
                self._handle.write(b"\0")
                self._handle.flush()
            self._handle.seek(0)
            if fcntl is not None:
                try:
                    fcntl.flock(self._handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError as exc:
                    raise AppError("E_BUSY", "同一会话已有处理任务在运行") from exc
            elif msvcrt is not None:  # pragma: no cover - Windows
                try:
                    msvcrt.locking(self._handle.fileno(), msvcrt.LK_NBLCK, 1)
                except OSError as exc:
                    raise AppError("E_BUSY", "同一会话已有处理任务在运行") from exc
            else:  # pragma: no cover
                raise AppError("E_INTERNAL", "当前平台不支持会话文件锁")
            self._acquired = True
            return self
        except BaseException:
            if self._handle is not None:
                self._handle.close()
                self._handle = None
            if self._thread_lock is not None:
                self._thread_lock.release()
                self._thread_lock = None
            raise

    def __exit__(self, exc_type, exc, traceback) -> None:
        try:
            if self._handle is not None:
                if self._acquired:
                    try:
                        if fcntl is not None:
                            fcntl.flock(self._handle.fileno(), fcntl.LOCK_UN)
                        elif msvcrt is not None:  # pragma: no cover - Windows
                            self._handle.seek(0)
                            msvcrt.locking(self._handle.fileno(), msvcrt.LK_UNLCK, 1)
                    except OSError:
                        pass
                self._handle.close()
        finally:
            self._handle = None
            self._acquired = False
            if self._thread_lock is not None:
                self._thread_lock.release()
                self._thread_lock = None

