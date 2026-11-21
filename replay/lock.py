"""Per-match cooperative lock preventing concurrent advancing tasks."""

from __future__ import annotations

import os
import re

from .errors import E_BUSY, fail


SAFE_MATCH = re.compile(r"[^A-Za-z0-9_.-]+")


class MatchLock:
    def __init__(self, lock_directory: str, match_id: str) -> None:
        safe = SAFE_MATCH.sub("_", match_id)
        self.path = os.path.join(os.path.abspath(lock_directory), f"{safe}.lock")
        self._fd: int | None = None

    def acquire(self) -> "MatchLock":
        os.makedirs(os.path.dirname(self.path), exist_ok=True)
        try:
            fd = os.open(self.path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o644)
        except FileExistsError:
            if self._remove_stale_lock():
                try:
                    fd = os.open(
                        self.path,
                        os.O_CREAT | os.O_EXCL | os.O_WRONLY,
                        0o644,
                    )
                except FileExistsError:
                    self._raise_busy()
            else:
                self._raise_busy()
        self._fd = fd
        os.write(fd, f"pid={os.getpid()}\n".encode("ascii"))
        os.fsync(fd)
        return self

    def _raise_busy(self) -> None:
        fail(
            E_BUSY,
            "another advancing task already owns this match",
            self.path,
            details={"lock_path": self.path},
        )

    def _remove_stale_lock(self) -> bool:
        try:
            with open(self.path, "r", encoding="ascii") as handle:
                content = handle.read()
        except OSError:
            return False
        pid = None
        for token in content.splitlines():
            if token.startswith("pid="):
                try:
                    pid = int(token[4:])
                except ValueError:
                    return False
        if pid is None or pid <= 0:
            return False
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            try:
                os.unlink(self.path)
            except FileNotFoundError:
                pass
            return True
        except PermissionError:
            return False
        return False

    def release(self) -> None:
        if self._fd is None:
            return
        try:
            os.close(self._fd)
        finally:
            self._fd = None
        try:
            os.unlink(self.path)
        except FileNotFoundError:
            pass

    def __enter__(self) -> "MatchLock":
        return self.acquire()

    def __exit__(self, exc_type: object, exc: object, traceback: object) -> None:
        self.release()
