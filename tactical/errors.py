"""Business error codes exposed by the service."""

from __future__ import annotations


class ServiceError(Exception):
    """An error carrying a stable business code."""

    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"


E_VERSION_MISMATCH = "E_VERSION_MISMATCH"
E_BATCH_FAILED = "E_BATCH_FAILED"
E_BUSY = "E_BUSY"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
