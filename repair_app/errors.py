from __future__ import annotations

from typing import Any


class AppError(Exception):
    """An expected application failure with a stable business code."""

    def __init__(self, code: str, message: str, **details: Any) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details

    def as_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "ok": False,
            "code": self.code,
            "message": self.message,
        }
        if self.details:
            payload["details"] = self.details
        return payload


def fail(code: str, message: str, **details: Any) -> None:
    raise AppError(code, message, **details)

