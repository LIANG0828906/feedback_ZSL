"""Business error codes used across the workbench."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

E_FILTER_INVALID = "E_FILTER_INVALID"
E_VERSION_STALE = "E_VERSION_STALE"
E_BUSY = "E_BUSY"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_CANCELLED = "E_CANCELLED"
E_WAV_INVALID = "E_WAV_INVALID"
E_PARAMS_INVALID = "E_PARAMS_INVALID"


@dataclass
class CalibrationError(Exception):
    """A failure with a stable business code and structured evidence."""

    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, f"{self.code}: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        return {"code": self.code, "message": self.message, "details": self.details}


class OperationCancelled(CalibrationError):
    def __init__(self, message: str = "operation cancelled") -> None:
        super().__init__(E_CANCELLED, message)
