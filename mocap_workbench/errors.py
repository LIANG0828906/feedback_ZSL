"""Business error codes used across the CLI, service and GUI."""

from __future__ import annotations

from typing import Any

E_BVH_INVALID = "E_BVH_INVALID"
E_CONSTRAINT_INVALID = "E_CONSTRAINT_INVALID"
E_CONFIG_INVALID = "E_CONFIG_INVALID"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_BUSY = "E_BUSY"
E_BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
E_CANCELLED = "E_CANCELLED"
E_IO = "E_IO"


class BusinessError(Exception):
    """A failure with a stable machine-readable business code."""

    def __init__(self, code: str, message: str, details: dict[str, Any] | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details or {}

    def to_dict(self) -> dict[str, Any]:
        return {"code": self.code, "message": self.message, "details": self.details}

