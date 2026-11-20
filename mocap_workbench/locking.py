"""Cross-process single-task locking for a BVH input."""

from __future__ import annotations

import hashlib
import os
import tempfile
import threading
from pathlib import Path
from typing import ClassVar

from .errors import E_BUSY, E_IO, BusinessError

try:  # POSIX
    import fcntl  # type: ignore
except ImportError:  # pragma: no cover - Windows
    fcntl = None  # type: ignore[assignment]

try:  # Windows
    import msvcrt  # type: ignore
except ImportError:  # pragma: no cover - POSIX
    msvcrt = None  # type: ignore[assignment]


def _lock_path_for_bvh(bvh_path: str | Path) -> Path:
    resolved = str(Path(bvh_path).resolve())
    digest = hashlib.sha256(resolved.encode("utf-8")).hexdigest()[:32]
    root = Path(tempfile.gettempdir()) / "mocap_workbench_locks"
    root.mkdir(parents=True, exist_ok=True)
    return root / f"{digest}.lock"


class JobLock:
    """Non-blocking OS lock plus an in-process reservation.

    The lock file is intentionally never unlinked.  Unlinking a lock file while
    another process is waiting can create two inodes for the same logical lock.
    """

    _registry_lock: ClassVar[threading.Lock] = threading.Lock()
    _active: ClassVar[set[str]] = set()

    def __init__(self, bvh_path: str | Path):
        self.bvh_path = str(Path(bvh_path).resolve())
        self.path = _lock_path_for_bvh(bvh_path)
        self._handle = None
        self._held = False

    def acquire(self) -> None:
        key = str(self.path)
        with self._registry_lock:
            if key in self._active:
                raise BusinessError(
                    E_BUSY,
                    "a solve task is already running for this BVH",
                    {"bvh": self.bvh_path},
                )
            self._active.add(key)
        try:
            handle = open(self.path, "a+b")
            self._handle = handle
            if fcntl is not None:
                try:
                    fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                except OSError as exc:
                    raise BusinessError(
                        E_BUSY,
                        "a solve task is already running for this BVH",
                        {"bvh": self.bvh_path},
                    ) from exc
            elif msvcrt is not None:  # pragma: no cover - Windows
                handle.seek(0, os.SEEK_END)
                if handle.tell() == 0:
                    handle.write(b"\0")
                    handle.flush()
                handle.seek(0)
                try:
                    msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
                except OSError as exc:
                    raise BusinessError(
                        E_BUSY,
                        "a solve task is already running for this BVH",
                        {"bvh": self.bvh_path},
                    ) from exc
            else:  # pragma: no cover - unsupported Python platform
                raise BusinessError(E_IO, "platform does not provide file locking")
            handle.seek(0)
            handle.truncate()
            handle.write(f"pid={os.getpid()}\n".encode("ascii"))
            handle.flush()
            os.fsync(handle.fileno())
            self._held = True
        except Exception:
            if self._handle is not None:
                try:
                    self._handle.close()
                except OSError:
                    pass
            self._handle = None
            with self._registry_lock:
                self._active.discard(key)
            raise

    def release(self) -> None:
        if self._handle is None:
            with self._registry_lock:
                self._active.discard(str(self.path))
            self._held = False
            return
        try:
            if fcntl is not None:
                fcntl.flock(self._handle.fileno(), fcntl.LOCK_UN)
            elif msvcrt is not None:  # pragma: no cover - Windows
                self._handle.seek(0)
                msvcrt.locking(self._handle.fileno(), msvcrt.LK_UNLCK, 1)
        finally:
            try:
                self._handle.close()
            finally:
                self._handle = None
                self._held = False
                with self._registry_lock:
                    self._active.discard(str(self.path))

    @property
    def held(self) -> bool:
        return self._held

    def __enter__(self) -> JobLock:
        self.acquire()
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.release()

