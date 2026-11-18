from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class ValidationIssue:
    code: str
    message: str
    path: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.path is not None:
            result["path"] = self.path
        if self.details:
            result["details"] = self.details
        return result


class MasterError(Exception):
    """A business error that can be serialized for machine consumers."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        path: str | None = None,
        details: dict[str, Any] | None = None,
        issues: list[ValidationIssue] | None = None,
        exit_code: int = 2,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.path = path
        self.details = details or {}
        self.issues = issues or []
        self.exit_code = exit_code

    def as_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "status": "error",
            "code": self.code,
            "message": self.message,
        }
        if self.path is not None:
            payload["path"] = self.path
        if self.details:
            payload["details"] = self.details
        if self.issues:
            payload["issues"] = [issue.as_dict() for issue in self.issues]
        return payload


class ValidationFailure(MasterError):
    def __init__(self, issues: list[ValidationIssue], manifest_path: str) -> None:
        super().__init__(
            "E_VALIDATION_FAILED",
            "Project manifest or audio validation failed; rendering was not started.",
            path=manifest_path,
            issues=issues,
            exit_code=2,
        )

