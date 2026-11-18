from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass(frozen=True)
class Issue:
    code: str
    message: str
    track: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.track is not None:
            result["track"] = self.track
        if self.details:
            result["details"] = self.details
        return result


class MasteringError(Exception):
    """A failure that must be reported without emitting a partial master."""

    def __init__(
        self,
        code: str,
        message: str,
        issues: list[Issue] | None = None,
        *,
        exit_code: int = 2,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.issues = issues or []
        self.exit_code = exit_code

    def as_dict(self) -> dict[str, Any]:
        return {
            "success": False,
            "error": {
                "code": self.code,
                "message": self.message,
                "issues": [issue.as_dict() for issue in self.issues],
            },
        }


class ProjectRejected(MasteringError):
    def __init__(self, issues: list[Issue], message: str = "project validation failed") -> None:
        super().__init__("PROJECT_REJECTED", message, issues)


class LoudnessOutOfTolerance(MasteringError):
    def __init__(self, message: str, details: dict[str, Any]) -> None:
        issue = Issue("LOUDNESS_OUT_OF_TOLERANCE", message, details=details)
        super().__init__(
            "LOUDNESS_OUT_OF_TOLERANCE",
            message,
            [issue],
            exit_code=3,
        )

