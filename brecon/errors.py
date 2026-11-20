from __future__ import annotations

from typing import Any


class BreconError(Exception):
    """A machine-readable domain error."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        step: int | None = None,
        field_path: str | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.step = step
        self.field_path = field_path
        self.details = details or {}

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.step is not None:
            result["step"] = self.step
        if self.field_path is not None:
            result["field_path"] = self.field_path
        if self.details:
            result["details"] = self.details
        return result


def error_dict(exc: BreconError) -> dict[str, Any]:
    return {"ok": False, "error": exc.to_dict()}

