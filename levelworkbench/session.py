"""Non-Qt task boundary shared by the workbench UI and tests."""

from __future__ import annotations

import threading

from .errors import E_BUSY, WorkbenchError


class TaskGate:
    """Allows exactly one foreground generation/replay task at a time."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._active = False

    def try_acquire(self) -> bool:
        with self._lock:
            if self._active:
                return False
            self._active = True
            return True

    def acquire_or_raise(self) -> None:
        if not self.try_acquire():
            raise WorkbenchError(
                E_BUSY,
                "a generation or replay task is already running",
            )

    def release(self) -> None:
        with self._lock:
            self._active = False

    @property
    def busy(self) -> bool:
        with self._lock:
            return self._active
