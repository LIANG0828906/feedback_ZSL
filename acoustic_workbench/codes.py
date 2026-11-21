"""Business error codes and structured error payloads."""

from __future__ import annotations

from typing import Any, Dict, Optional


E_BUSY = "E_BUSY"
E_CANCELLED = "E_CANCELLED"
E_CHANNEL_LAYOUT_MISMATCH = "E_CHANNEL_LAYOUT_MISMATCH"
E_CONFIG_INVALID = "E_CONFIG_INVALID"
E_DECAY_NEGATIVE = "E_DECAY_NEGATIVE"
E_FIELD_STALE = "E_FIELD_STALE"
E_IO = "E_IO"
E_IR_INVALID = "E_IR_INVALID"
E_POSITION_OUT_OF_BOUNDS = "E_POSITION_OUT_OF_BOUNDS"
E_SYNTHESIS_FAILED = "E_SYNTHESIS_FAILED"


class AcousticError(Exception):
    """A validation or processing failure with a stable business code."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        injection_point: Optional[str] = None,
        details: Optional[Dict[str, Any]] = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.injection_point = injection_point
        self.details = details or {}

    def to_dict(self) -> Dict[str, Any]:
        payload: Dict[str, Any] = {
            "ok": False,
            "code": self.code,
            "message": self.message,
        }
        if self.injection_point:
            payload["injection_point"] = self.injection_point
        if self.details:
            payload["details"] = self.details
        return payload

