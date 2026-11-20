"""Business error types with stable codes and machine-readable locations."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_FP_INVALID = "E_FP_INVALID"
E_MIGRATE_LOSS = "E_MIGRATE_LOSS"
E_ALIGN_MISMATCH = "E_ALIGN_MISMATCH"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_LIB_MISMATCH = "E_LIB_MISMATCH"
E_BUSY = "E_BUSY"
E_CANCELLED = "E_CANCELLED"
E_AUDIO_INVALID = "E_AUDIO_INVALID"
E_STATE_INVALID = "E_STATE_INVALID"


@dataclass
class BizError(Exception):
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(f"{self.code}: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "code": self.code,
            "message": self.message,
            "details": self.details,
        }
