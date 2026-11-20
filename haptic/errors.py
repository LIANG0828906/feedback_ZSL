from __future__ import annotations

from typing import Any

E_WAVE_INVALID = "E_WAVE_INVALID"
E_VERSION_STALE = "E_VERSION_STALE"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_BUSY = "E_BUSY"
E_CANCELLED = "E_CANCELLED"
E_IO = "E_IO"


class HapticError(Exception):
    """Structured application error with a stable machine-readable code."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        segment_index: int | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.segment_index = segment_index
        self.details = details or {}

    def __str__(self) -> str:
        location = (
            f" segment={self.segment_index}" if self.segment_index is not None else ""
        )
        return f"{self.code}{location}: {self.message}"

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
        }
        if self.segment_index is not None:
            payload["segment_index"] = self.segment_index
        if self.details:
            payload["details"] = self.details
        return payload


class CompileCancelled(HapticError):
    def __init__(self, message: str = "recompute cancelled at a checkpoint") -> None:
        super().__init__(E_CANCELLED, message)
