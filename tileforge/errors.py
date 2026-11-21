"""Business error codes used by every TileForge entry point."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_HEADER_INVALID = "E_HEADER_INVALID"
E_GRID_INVALID = "E_GRID_INVALID"
E_BIT_DEPTH_INVALID = "E_BIT_DEPTH_INVALID"
E_TRANSFER_CODE = "E_TRANSFER_CODE"
E_DATA_LENGTH = "E_DATA_LENGTH"
E_BLOCK_HASH = "E_BLOCK_HASH"
E_IO = "E_IO"
E_BUSY = "E_BUSY"
E_BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
E_VERSION_STALE = "E_VERSION_STALE"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_PREVIEW_MISMATCH = "E_PREVIEW_MISMATCH"
E_CONFIG_INVALID = "E_CONFIG_INVALID"
E_CONVERSION_MISMATCH = "E_CONVERSION_MISMATCH"
E_CANCELLED = "E_CANCELLED"


@dataclass
class TileForgeError(Exception):
    """A stable machine-readable error with optional field-level context."""

    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        if not self.details:
            return f"{self.code}: {self.message}"
        return f"{self.code}: {self.message} ({self.details})"

    def to_dict(self) -> dict[str, Any]:
        return {"ok": False, "code": self.code, "message": self.message, "details": self.details}


def error(code: str, message: str, **details: Any) -> TileForgeError:
    return TileForgeError(code=code, message=message, details=details)
