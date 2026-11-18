"""Structured error and validation helpers."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class SceneIssue:
    code: str
    location: str
    field: str
    message: str
    value: Any | None = None

    def as_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "code": self.code,
            "location": self.location,
            "field": self.field,
            "message": self.message,
        }
        if self.value is not None:
            out["value"] = self.value
        return out


class ValidationFailure(Exception):
    def __init__(self, issues: list[SceneIssue]):
        self.issues = issues
        super().__init__(f"{len(issues)} scene validation error(s)")

    def as_dict(self) -> dict[str, Any]:
        return {"ok": False, "errors": [issue.as_dict() for issue in self.issues]}


class RenderFailure(Exception):
    def __init__(self, code: str, message: str, details: dict[str, Any] | None = None):
        self.code = code
        self.message = message
        self.details = details or {}
        super().__init__(message)

    def as_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"ok": False, "error": {"code": self.code, "message": self.message}}
        out["error"].update(self.details)
        return out
