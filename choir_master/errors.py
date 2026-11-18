"""Structured business errors shared by the CLI and library API."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


EXIT_OK = 0
EXIT_VALIDATION = 2
EXIT_RESUME_MISMATCH = 3
EXIT_PROCESSING = 4


@dataclass(frozen=True)
class BusinessIssue:
    code: str
    mic: str
    message: str
    voice: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        # Stable key order is intentional: reports and errors are diffable.
        result: dict[str, Any] = {
            "code": self.code,
            "mic": self.mic,
            "message": self.message,
        }
        if self.voice is not None:
            result["voice"] = self.voice
        for key in sorted(self.details):
            result[key] = self.details[key]
        return result


class BusinessError(Exception):
    def __init__(
        self,
        issues: list[BusinessIssue],
        exit_code: int = EXIT_VALIDATION,
    ) -> None:
        super().__init__(issues[0].message if issues else "business error")
        self.issues = issues
        self.exit_code = exit_code

    def payload(self) -> dict[str, Any]:
        return {
            "status": "error",
            "errors": [issue.as_dict() for issue in self.issues],
        }


def issue(
    code: str,
    mic: str,
    message: str,
    *,
    voice: str | None = None,
    **details: Any,
) -> BusinessIssue:
    return BusinessIssue(
        code=code,
        mic=mic,
        message=message,
        voice=voice,
        details=details,
    )


def fail(
    code: str,
    mic: str,
    message: str,
    *,
    voice: str | None = None,
    exit_code: int = EXIT_VALIDATION,
    **details: Any,
) -> None:
    raise BusinessError(
        [issue(code, mic, message, voice=voice, **details)],
        exit_code=exit_code,
    )

