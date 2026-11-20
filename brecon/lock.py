from __future__ import annotations

import os
from pathlib import Path
from typing import IO

from .errors import BreconError


class RoomBusyLock:
    """One advancing replay task per room.

    POSIX uses ``flock`` and Windows uses ``msvcrt.locking``. Both are released
    by the OS when a process is forcibly terminated, so crash recovery does not
    inherit a stale busy state.
    """

    def __init__(self, room_id: str, lock_path: str | Path) -> None:
        self.room_id = room_id
        self.lock_path = Path(lock_path)
        self._handle: IO[str] | None = None

    def acquire(self) -> "RoomBusyLock":
        self.lock_path.parent.mkdir(parents=True, exist_ok=True)
        handle = self.lock_path.open("a+", encoding="utf-8")
        try:
            if os.name == "nt":
                import msvcrt

                handle.seek(0, os.SEEK_END)
                if handle.tell() == 0:
                    handle.write(" ")
                    handle.flush()
                handle.seek(0)
                try:
                    msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
                except OSError as exc:
                    raise BreconError(
                        "E_BUSY",
                        f"room already has an advancing task: {self.room_id}",
                        details={"room_id": self.room_id},
                    ) from exc
            else:
                import fcntl

                try:
                    fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                except OSError as exc:
                    raise BreconError(
                        "E_BUSY",
                        f"room already has an advancing task: {self.room_id}",
                        details={"room_id": self.room_id},
                    ) from exc
            handle.seek(0)
            handle.truncate()
            handle.write(f"room_id={self.room_id}\npid={os.getpid()}\n")
            handle.flush()
            self._handle = handle
            return self
        except BaseException:
            handle.close()
            raise

    def release(self) -> None:
        handle = self._handle
        if handle is None:
            return
        try:
            if os.name == "nt":
                import msvcrt

                handle.seek(0)
                try:
                    msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
                except OSError:
                    pass
            else:
                import fcntl

                try:
                    fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
                except OSError:
                    pass
        finally:
            handle.close()
            self._handle = None

    def __enter__(self) -> "RoomBusyLock":
        return self.acquire()

    def __exit__(self, exc_type: object, exc: object, traceback: object) -> None:
        self.release()


def room_lock_path(sample_dir: str | Path) -> Path:
    return Path(sample_dir) / ".replay.lock"

