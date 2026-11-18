from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class MasteringIssue:
    code: str
    track: str
    message: str

    def as_dict(self) -> dict[str, str]:
        return {
            "code": self.code,
            "track": self.track,
            "message": self.message,
        }


class MasteringError(Exception):
    """A deterministic, user-facing mastering failure."""

    def __init__(self, issues: list[MasteringIssue], exit_code: int = 2):
        self.issues = issues
        self.exit_code = exit_code
        super().__init__("; ".join(f"{i.code} [{i.track}]: {i.message}" for i in issues))

    def as_dict(self) -> dict[str, Any]:
        return {
            "status": "error",
            "errors": [issue.as_dict() for issue in self.issues],
        }


def issue(code: str, track: str, message: str) -> MasteringIssue:
    return MasteringIssue(code=code, track=track, message=message)


def fail(code: str, track: str, message: str, exit_code: int = 2) -> None:
    raise MasteringError([issue(code, track, message)], exit_code=exit_code)

