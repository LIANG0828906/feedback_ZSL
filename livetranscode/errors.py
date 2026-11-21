from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


class ErrorCode:
    TIMEBASE_INVALID = "E_TIMEBASE_INVALID"
    BUSY = "E_BUSY"
    BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
    SWITCH_INVALID = "E_SWITCH_INVALID"
    SNAPSHOT_MISMATCH = "E_SNAPSHOT_MISMATCH"
    CONFIG_INVALID = "E_CONFIG_INVALID"
    IO_ERROR = "E_IO"
    CANCELLED = "E_CANCELLED"
    INTERNAL = "E_INTERNAL"


@dataclass
class BusinessError(Exception):
    code: str
    message: str
    context: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(f"{self.code}: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        return {"ok": False, "code": self.code, "message": self.message, "context": self.context}


def build_error(code: str, message: str, **context: Any) -> BusinessError:
    return BusinessError(code=code, message=message, context=context)

