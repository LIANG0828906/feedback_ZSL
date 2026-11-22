"""Stable error contract for all public entry points."""

from __future__ import annotations

from typing import Any


class LCFTError(Exception):
    """An error with a stable code and a machine-readable conflict field."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        field: str | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.field = field
        self.details = details or {}

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
        }
        if self.field is not None:
            result["field"] = self.field
        if self.details:
            result["details"] = self.details
        return result

    def __str__(self) -> str:
        if self.field:
            return f"{self.code} [{self.field}]: {self.message}"
        return f"{self.code}: {self.message}"


def timebase_error(field: str, message: str, **details: Any) -> LCFTError:
    return LCFTError("E_TIMEBASE_INVALID", message, field=field, details=details)


def input_error(field: str, message: str, **details: Any) -> LCFTError:
    return LCFTError("E_INPUT_INVALID", message, field=field, details=details)


def param_error(field: str, message: str, **details: Any) -> LCFTError:
    return LCFTError("E_PARAM_INVALID", message, field=field, details=details)

