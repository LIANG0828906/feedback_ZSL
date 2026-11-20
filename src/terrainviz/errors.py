from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_HEIGHT_INVALID = "E_HEIGHT_INVALID"
E_CRACK_UNRESOLVED = "E_CRACK_UNRESOLVED"
E_SNAPSHOT_INVALID = "E_SNAPSHOT_INVALID"
E_BUSY = "E_BUSY"
E_CANCELLED = "E_CANCELLED"
E_INVALID_ARGUMENT = "E_INVALID_ARGUMENT"


@dataclass(init=False)
class TerrainError(RuntimeError):
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __init__(
        self,
        code: str,
        message: str,
        details: dict[str, Any] | None = None,
        **extra_details: Any,
    ) -> None:
        RuntimeError.__init__(self, message)
        self.code = code
        self.message = message
        merged = dict(details or {})
        merged.update(extra_details)
        self.details = merged

    def __str__(self) -> str:
        if not self.details:
            return f"{self.code}: {self.message}"
        return f"{self.code}: {self.message} ({self.details})"

    def to_dict(self) -> dict[str, Any]:
        return {"code": self.code, "message": self.message, "details": self.details}


def require(condition: bool, code: str, message: str, **details: Any) -> None:
    if not condition:
        raise TerrainError(code, message, details)

