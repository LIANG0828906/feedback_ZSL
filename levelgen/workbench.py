"""Thread-safe workbench task coordination independent of Qt."""

from __future__ import annotations

import threading

from .errors import E_BUSY


class SingleTaskGate:
    """Allow exactly one active generation or replay task."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._active = False

    @property
    def active(self) -> bool:
        with self._lock:
            return self._active

    def try_start(self) -> str | None:
        with self._lock:
            if self._active:
                return E_BUSY
            self._active = True
            return None

    def finish(self) -> None:
        with self._lock:
            self._active = False
