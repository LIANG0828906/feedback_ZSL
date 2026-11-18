"""Structured errors used by the command line interface and renderer."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class FogError(Exception):
    """An error with a stable business code and JSON field path."""

    code: str
    path: str
    message: str
    details: dict[str, Any] | None = None

    def __str__(self) -> str:
        return f"{self.code} at {self.path}: {self.message}"

    def as_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "path": self.path,
            "message": self.message,
        }
        if self.details:
            payload.update(self.details)
        return payload


def error_document(error: FogError) -> dict[str, Any]:
    return {"ok": False, "error": error.as_dict()}

