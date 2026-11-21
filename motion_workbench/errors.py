from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_BVH_INVALID = "E_BVH_INVALID"
E_CONSTRAINT_INVALID = "E_CONSTRAINT_INVALID"
E_COMMIT_FAILED = "E_COMMIT_FAILED"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_BUSY = "E_BUSY"
E_CANCELLED = "E_CANCELLED"


@dataclass
class WorkbenchError(Exception):
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, f"{self.code}: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "code": self.code,
            "message": self.message,
            "details": self.details,
        }
