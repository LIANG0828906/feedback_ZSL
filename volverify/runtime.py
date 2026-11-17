from __future__ import annotations

from dataclasses import dataclass


@dataclass
class StopController:
    signal_number: int | None = None

    def __call__(self) -> bool:
        return self.signal_number is not None

