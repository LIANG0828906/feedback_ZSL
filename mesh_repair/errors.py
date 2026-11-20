from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_MESH_INVALID = "E_MESH_INVALID"
E_BUSY = "E_BUSY"
E_BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
E_BOUNDARY_PENDING = "E_BOUNDARY_PENDING"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_CANCELLED = "E_CANCELLED"
E_CONFIG_INVALID = "E_CONFIG_INVALID"

SUB_INDEX_OUT_OF_RANGE = "E_INDEX_OUT_OF_RANGE"
SUB_DEGENERATE_FACE = "E_DEGENERATE_FACE"
SUB_NONFINITE_COORD = "E_NONFINITE_COORD"
SUB_FACE_COUNT_MISMATCH = "E_FACE_COUNT_MISMATCH"
SUB_MALFORMED_HEADER = "E_MALFORMED_HEADER"
SUB_INVALID_PARAMETER = "E_INVALID_PARAMETER"
SUB_CORRUPT_COMMIT = "E_CORRUPT_COMMIT"
SUB_BLOCK_TABLE_MISMATCH = "E_BLOCK_TABLE_MISMATCH"


@dataclass
class BusinessError(Exception):
    """An expected business failure with a stable code and optional location."""

    code: str
    message: str
    subcode: str | None = None
    location: dict[str, Any] | None = None
    context: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        suffix = f" ({self.subcode})" if self.subcode else ""
        return f"{self.code}{suffix}: {self.message}"

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "ok": False,
            "code": self.code,
            "message": self.message,
        }
        if self.subcode:
            result["subcode"] = self.subcode
        if self.location:
            result["location"] = self.location
        if self.context:
            result["context"] = self.context
        return result
