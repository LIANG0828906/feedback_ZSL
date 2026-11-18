"""Structured scene validation errors."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass(frozen=True)
class ValidationIssue:
    code: str
    layer_index: int | None
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "code": self.code,
            "layer_index": self.layer_index,
            "message": self.message,
        }
        if self.details:
            result["details"] = self.details
        return result


class SceneValidationError(ValueError):
    """Raised when a scene cannot be accepted as a whole."""

    def __init__(self, issues: list[ValidationIssue]):
        self.issues = sorted(
            issues,
            key=lambda item: (
                -1 if item.layer_index is None else item.layer_index,
                item.code,
                item.message,
            ),
        )
        summary = ", ".join(
            f"{item.code}@{item.layer_index}: {item.message}" for item in self.issues
        )
        super().__init__(summary or "invalid scene")

    def as_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "code": "SCENE_REJECTED",
            "errors": [issue.as_dict() for issue in self.issues],
        }
