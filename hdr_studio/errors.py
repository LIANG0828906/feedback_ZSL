from __future__ import annotations

from enum import StrEnum
from typing import Any


class ErrorCode(StrEnum):
    HDR_INVALID = "E_HDR_INVALID"
    PARAM_CONFLICT = "E_PARAM_CONFLICT"
    BUSY = "E_BUSY"
    VERSION_MISMATCH = "E_VERSION_MISMATCH"
    RECOVERY_INVALID = "E_RECOVERY_INVALID"
    CANCELLED = "E_CANCELLED"
    IO_ERROR = "E_IO"


class HDRError(Exception):
    """Stable, machine-readable error for all product-facing failures."""

    def __init__(
        self,
        code: ErrorCode,
        message: str,
        *,
        conflict_params: list[str] | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.conflict_params = conflict_params or []
        self.details = details or {}

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code.value,
            "message": self.message,
            "conflict_params": self.conflict_params,
            "details": self.details,
        }

    def __str__(self) -> str:
        if self.conflict_params:
            return f"{self.code.value}: {self.message} ({', '.join(self.conflict_params)})"
        return f"{self.code.value}: {self.message}"

