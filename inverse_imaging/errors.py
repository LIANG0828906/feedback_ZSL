"""Stable application error codes shared by the CLI, engine and GUI."""

from __future__ import annotations

from dataclasses import dataclass, field as dc_field
from typing import Any


E_SCENE_INVALID = "E_SCENE_INVALID"
E_REDUCE_DIVERGE = "E_REDUCE_DIVERGE"
E_BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_BUSY = "E_BUSY"
E_STALE_HANDLE = "E_STALE_HANDLE"
E_CANCELLED = "E_CANCELLED"
E_IO = "E_IO"


@dataclass
class AppError(Exception):
    code: str
    message: str
    field: str | None = None
    context: dict[str, Any] = dc_field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, f"{self.code}: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.field is not None:
            result["field"] = self.field
        if self.context:
            result["context"] = self.context
        return result

