from __future__ import annotations

import threading

from .errors import E_CANCELLED, fail


class CancellationToken:
    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()

    def checkpoint(self, location: str) -> None:
        if self._event.is_set():
            fail(E_CANCELLED, "incremental reconstruction was cancelled", checkpoint=location)

