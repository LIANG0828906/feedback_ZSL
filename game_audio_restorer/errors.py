"""Business errors shared by the core engine, CLI and GUI."""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any


class ErrorCode(str, Enum):
    WAV_INVALID = "E_WAV_INVALID"
    RECIPE_INVALID = "E_RECIPE_INVALID"
    VERSION_STALE = "E_VERSION_STALE"
    BUSY = "E_BUSY"
    RECOVERY_INVALID = "E_RECOVERY_INVALID"
    PROCESSING = "E_PROCESSING"
    CANCELLED = "E_CANCELLED"


@dataclass
class BusinessError(Exception):
    code: ErrorCode
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(f"{self.code.value}: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "code": self.code.value,
            "message": self.message,
            "details": self.details,
        }
