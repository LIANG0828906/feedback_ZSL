from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class ValidationIssue:
    field: str
    message: str
    layer_index: int | None = None
    mesh_id: str | None = None

    def as_dict(self) -> dict[str, Any]:
        return {
            "field": self.field,
            "layer_index": self.layer_index,
            "mesh_id": self.mesh_id,
            "message": self.message,
        }


class SceneValidationError(Exception):
    exit_code = 2

    def __init__(self, issues: list[ValidationIssue]):
        self.issues = issues
        super().__init__(f"{len(issues)} scene validation error(s)")

    def payload(self) -> dict[str, Any]:
        return {
            "ok": False,
            "code": "SCENE_VALIDATION_ERROR",
            "errors": [issue.as_dict() for issue in self.issues],
        }


class SceneParseError(SceneValidationError):
    pass


class RenderError(Exception):
    exit_code = 3

    def __init__(self, code: str, message: str, **details: Any):
        self.code = code
        self.message = message
        self.details = details
        super().__init__(message)

    def payload(self) -> dict[str, Any]:
        data = {"ok": False, "code": self.code, "message": self.message}
        data.update(self.details)
        return data


class OutputMismatchError(RenderError):
    exit_code = 4

    def __init__(self, message: str, differences: list[dict[str, Any]]):
        super().__init__(
            "RESUME_OUTPUT_MISMATCH",
            message,
            differences=differences,
        )
