"""Structured validation and runtime errors."""

from __future__ import annotations

from dataclasses import asdict, dataclass
from typing import Any


@dataclass(frozen=True)
class ValidationIssue:
    code: str
    message: str
    layer: int | None = None
    layer_id: str | None = None
    path: str | None = None

    def as_dict(self) -> dict[str, Any]:
        return {key: value for key, value in asdict(self).items() if value is not None}


class SceneValidationError(ValueError):
    """A scene failed schema, geometry, material, or camera validation."""

    def __init__(self, issues: list[ValidationIssue]):
        self.issues = issues
        super().__init__(f"scene validation failed with {len(issues)} issue(s)")

    def as_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "error": "scene_validation_failed",
            "issues": [issue.as_dict() for issue in self.issues],
        }


class RenderConsistencyError(RuntimeError):
    """A resumed result differed from a supplied reference."""

    def __init__(self, differences: dict[str, Any]):
        self.differences = differences
        super().__init__(
            f"render consistency check failed with {differences.get('difference_count', 0)} difference(s)"
        )

