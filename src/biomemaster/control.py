"""Lightweight process interruption state."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass
class StopFlag:
    requested: bool = False
    signal_number: int | None = None

    def request(self, signum: int, _frame: Any) -> None:
        self.requested = True
        self.signal_number = signum
