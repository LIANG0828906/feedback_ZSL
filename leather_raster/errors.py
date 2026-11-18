from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass(frozen=True)
class ValidationIssue:
    code: str
    path: str
    message: str
    layer_index: int | None = None
    cutout_id: int | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "code": self.code,
            "path": self.path,
            "message": self.message,
        }
        if self.layer_index is not None:
            result["layer_index"] = self.layer_index
        if self.cutout_id is not None:
            result["cutout_id"] = self.cutout_id
        if self.details:
            result["details"] = self.details
        return result


class SceneValidationError(ValueError):
    """A machine-readable scene validation failure."""

    exit_code = 2

    def __init__(self, issues: list[ValidationIssue], message: str = "scene validation failed"):
        super().__init__(message)
        self.issues = issues
        self.message = message

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "error": {
                "code": "SCENE_VALIDATION_FAILED",
                "message": self.message,
                "issues": [issue.to_dict() for issue in self.issues],
            },
        }


class OutputMismatchError(RuntimeError):
    exit_code = 4

    def __init__(self, differences: list[dict[str, Any]], message: str = "rendered output differs from baseline"):
        super().__init__(message)
        self.differences = differences
        self.message = message

