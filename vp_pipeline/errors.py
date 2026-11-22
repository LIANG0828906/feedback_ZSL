"""Structured pipeline errors and stable error codes."""

from __future__ import annotations

from typing import Any

E_VER = "E_VER"
E_SCHEMA = "E_SCHEMA"
E_PARA = "E_PARA"
E_STATE = "E_STATE"
E_LOSSY = "E_LOSSY"
E_DIFF = "E_DIFF"
E_DECODE = "E_DECODE"
E_IO = "E_IO"
E_INTERNAL = "E_INTERNAL"


class PipelineError(RuntimeError):
    def __init__(
        self,
        code: str,
        message: str,
        field_path: str | None = None,
        details: dict[str, Any] | None = None,
        **context: Any,
    ) -> None:
        RuntimeError.__init__(self, message)
        self.code = code
        self.message = message
        self.field_path = field_path
        self.details = {**(details or {}), **context}

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.field_path is not None:
            payload["field_path"] = self.field_path
        if self.details:
            payload["details"] = self.details
        return payload

    def __str__(self) -> str:
        location = f" at {self.field_path}" if self.field_path else ""
        return f"{self.code}{location}: {self.message}"


def raise_error(
    code: str,
    message: str,
    field_path: str | None = None,
    **details: Any,
) -> None:
    raise PipelineError(code=code, message=message, field_path=field_path, details=details)
