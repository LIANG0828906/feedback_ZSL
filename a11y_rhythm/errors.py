"""Stable error contract used by the CLI, tests and GUI bridge."""

from __future__ import annotations

import copy
from dataclasses import dataclass, field
from typing import Any


E_TIMEBASE_INVALID = "E_TIMEBASE_INVALID"
E_DESC_CONFLICT = "E_DESC_CONFLICT"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_STALE_HANDLE = "E_STALE_HANDLE"
E_BUSY = "E_BUSY"
E_CANCELED = "E_CANCELED"
E_INPUT_INVALID = "E_INPUT_INVALID"


@dataclass
class RhythmError(Exception):
    code: str
    message: str
    item_id: str | None = None
    evidence: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "item_id": self.item_id,
            "evidence": self.evidence,
        }

    def __deepcopy__(self, memo: dict[int, Any]) -> "RhythmError":
        return RhythmError(
            self.code,
            self.message,
            self.item_id,
            copy.deepcopy(self.evidence, memo),
        )

