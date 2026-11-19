from __future__ import annotations

from typing import Any


class ApiError(Exception):
    def __init__(
        self,
        code: str,
        status: int,
        message: str,
        details: dict[str, Any] | None = None,
    ):
        super().__init__(message)
        self.code = code
        self.status = status
        self.message = message
        self.details = details or {}

    def as_dict(self) -> dict[str, Any]:
        return {
            "error": {
                "code": self.code,
                "details": self.details,
                "message": self.message,
            }
        }
