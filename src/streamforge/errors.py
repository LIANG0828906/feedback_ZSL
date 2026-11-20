from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


class ErrorCode:
    TIMEBASE_INVALID = "E_TIMEBASE_INVALID"
    BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
    SWITCH_MISALIGN = "E_SWITCH_MISALIGN"
    RECOVERY_INVALID = "E_RECOVERY_INVALID"
    BUSY = "E_BUSY"
    INVALID_CONFIG = "E_INVALID_CONFIG"
    IO = "E_IO"
    CANCELLED = "E_CANCELLED"
    INJECTED_FAILURE = "E_INJECTED_FAILURE"


@dataclass
class BusinessError(Exception):
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        if not self.details:
            return f"{self.code}: {self.message}"
        return f"{self.code}: {self.message} ({self.details})"

    def to_dict(self) -> dict[str, Any]:
        return {"code": self.code, "message": self.message, "details": self.details}


def fail(code: str, message: str, **details: Any) -> BusinessError:
    return BusinessError(code=code, message=message, details=details)
