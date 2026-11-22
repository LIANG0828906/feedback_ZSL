"""Business errors shared by the core engine and the desktop UI."""

from __future__ import annotations

from typing import Any


class BusinessError(Exception):
    """An expected, user-actionable error identified by a stable business code."""

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
        payload: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
            "details": self.details,
        }
        if self.field is not None:
            payload["field"] = self.field
        return payload

    def __str__(self) -> str:
        suffix = f" [{self.field}]" if self.field else ""
        return f"{self.code}{suffix}: {self.message}"
