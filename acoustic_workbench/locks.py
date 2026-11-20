from __future__ import annotations

import fcntl
import threading
from pathlib import Path

from .errors import E_BUSY, fail


class PointLocks:
    def __init__(self, lock_root: str | Path):
        self.lock_root = Path(lock_root)
        self.lock_root.mkdir(parents=True, exist_ok=True)
        self._guard = threading.Lock()
        self._thread_locks: dict[str, threading.Lock] = {}

    def _thread_lock(self, point_id: str) -> threading.Lock:
        with self._guard:
            return self._thread_locks.setdefault(point_id, threading.Lock())

    def acquire(self, point_id: str) -> "_PointLease":
        thread_lock = self._thread_lock(point_id)
        if not thread_lock.acquire(blocking=False):
            fail(
                E_BUSY,
                "a rebuild or render task is already active for this listener",
                point_id=point_id,
            )
        path = self.lock_root / f"{point_id}.lock"
        handle = None
        try:
            handle = path.open("a+")
            try:
                fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                fail(
                    E_BUSY,
                    "another process is rebuilding or rendering this listener",
                    point_id=point_id,
                )
            return _PointLease(point_id, thread_lock, handle)
        except BaseException:
            if handle is not None:
                handle.close()
            thread_lock.release()
            raise


class _PointLease:
    def __init__(
        self,
        point_id: str,
        thread_lock: threading.Lock,
        handle: object,
    ):
        self.point_id = point_id
        self._thread_lock = thread_lock
        self._handle = handle
        self._released = False

    def __enter__(self) -> "_PointLease":
        return self

    def __exit__(self, exc_type: object, exc: object, traceback: object) -> None:
        self.release()

    def release(self) -> None:
        if self._released:
            return
        self._released = True
        try:
            fcntl.flock(self._handle.fileno(), fcntl.LOCK_UN)
        finally:
            self._handle.close()
            self._thread_lock.release()

