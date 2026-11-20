from __future__ import annotations

import threading
from dataclasses import dataclass, field

from .errors import E_CANCELLED, TerrainError


@dataclass
class CancellationToken:
    _event: threading.Event = field(default_factory=threading.Event)
    _checkpoint: int = 0

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()

    @property
    def checkpoint(self) -> int:
        return self._checkpoint

    def check(self, location: str) -> None:
        self._checkpoint += 1
        if self._event.is_set():
            raise TerrainError(
                E_CANCELLED,
                "render cancelled at a cooperative checkpoint",
                checkpoint=self._checkpoint,
                location=location,
            )

