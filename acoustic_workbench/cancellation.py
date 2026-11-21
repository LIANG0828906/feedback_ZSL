"""Cooperative cancellation primitives."""

from __future__ import annotations

import threading
from typing import Optional

from .codes import AcousticError, E_CANCELLED


class CancelToken:
    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()

    def checkpoint(self, location: str) -> None:
        if self._event.is_set():
            raise AcousticError(
                E_CANCELLED,
                "Task was cancelled at a cooperative checkpoint.",
                injection_point=f"checkpoint:{location}",
            )


def checkpoint(token: Optional[CancelToken], location: str) -> None:
    if token is not None:
        token.checkpoint(location)

