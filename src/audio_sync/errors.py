"""Machine-readable errors used across the command line, core and UI."""

from __future__ import annotations

from dataclasses import dataclass

E_BUSY = "E_BUSY"
E_CANCELLED = "E_CANCELLED"
E_VERSION_STALE = "E_VERSION_STALE"
E_MARKER_MISSING = "E_MARKER_MISSING"
E_DRIFT_UNKNOWN = "E_DRIFT_UNKNOWN"
E_COMPENSATION_FAILED = "E_COMPENSATION_FAILED"
E_RESIDUAL_TOLERANCE = "E_RESIDUAL_TOLERANCE"
E_INVALID_WAV = "E_INVALID_WAV"
E_INVALID_MARKERS = "E_INVALID_MARKERS"
E_INVALID_CONFIG = "E_INVALID_CONFIG"
E_NOT_FOUND = "E_NOT_FOUND"
E_TASK_FAILED = "E_TASK_FAILED"


@dataclass(eq=False)
class SyncError(RuntimeError):
    """An expected failure with a stable code suitable for automation."""

    code: str
    message: str
    details: dict | None = None

    def __post_init__(self) -> None:
        super().__init__(f"{self.code}: {self.message}")

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"

    def to_dict(self) -> dict:
        return {"code": self.code, "message": self.message, "details": self.details or {}}


def as_error_dict(exc: BaseException) -> dict:
    if isinstance(exc, SyncError):
        return exc.to_dict()
    return {"code": E_TASK_FAILED, "message": str(exc), "details": {}}

