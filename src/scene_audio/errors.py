from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class StructuredIssue:
    code: str
    message: str
    track: str | None = None
    event_id: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
        }
        if self.track is not None:
            result["track"] = self.track
        if self.event_id is not None:
            result["event_id"] = self.event_id
        if self.details:
            result["details"] = self.details
        return result


class BusinessError(Exception):
    """A deterministic, reportable failure that must abort the render."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        track: str | None = None,
        event_id: str | None = None,
        details: dict[str, Any] | None = None,
        exit_code: int = 2,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.track = track
        self.event_id = event_id
        self.details = details or {}
        self.exit_code = int(exit_code)

    def to_dict(self) -> dict[str, Any]:
        issue = StructuredIssue(
            self.code,
            self.message,
            track=self.track,
            event_id=self.event_id,
            details=self.details,
        ).to_dict()
        return {
            "ok": False,
            "code": self.code,
            "message": self.message,
            "track": self.track,
            "event_id": self.event_id,
            "details": self.details,
            "hard_errors": [issue],
            "quality_warnings": [],
        }


class ValidationFailure(BusinessError):
    def __init__(self, issues: list[StructuredIssue]) -> None:
        if not issues:
            raise ValueError("ValidationFailure requires at least one issue")
        first = issues[0]
        super().__init__(
            first.code,
            first.message,
            track=first.track,
            event_id=first.event_id,
            details={"issue_count": len(issues)},
            exit_code=2,
        )
        self.issues = issues

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "code": self.code,
            "message": self.message,
            "track": self.track,
            "event_id": self.event_id,
            "issues": [issue.to_dict() for issue in self.issues],
            "hard_errors": [issue.to_dict() for issue in self.issues],
            "quality_warnings": [],
        }

