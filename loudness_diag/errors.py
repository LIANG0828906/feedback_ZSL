"""Business error codes used by the CLI and library."""

from __future__ import annotations

from typing import Any

E_HEADER_INVALID = "E_HEADER_INVALID"
E_BUSY = "E_BUSY"
E_CONFIG_INVALID = "E_CONFIG_INVALID"
E_VERSION_MISMATCH = "E_VERSION_MISMATCH"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_SNAPSHOT_RELEASED = "E_SNAPSHOT_RELEASED"


class DiagnosticError(Exception):
    """A deterministic, machine-readable business failure."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        first_broken_block: int | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.first_broken_block = first_broken_block
        self.details = details or {}

    def as_dict(self) -> dict[str, Any]:
        error: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.first_broken_block is not None:
            error["first_broken_block"] = self.first_broken_block
        if self.details:
            error["details"] = self.details
        return {"ok": False, "error": error}

    def __str__(self) -> str:
        if self.first_broken_block is None:
            return f"{self.code}: {self.message}"
        return f"{self.code}: {self.message} (first broken block: {self.first_broken_block})"


def recovery_error(
    message: str,
    *,
    first_broken_block: int | None,
    details: dict[str, Any] | None = None,
) -> DiagnosticError:
    return DiagnosticError(
        E_RECOVERY_INVALID,
        message,
        first_broken_block=first_broken_block,
        details=details,
    )

