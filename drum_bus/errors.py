from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass(frozen=True)
class ValidationIssue:
    channel: str
    code: str
    message: str
    path: str | None = None

    def as_dict(self) -> dict[str, Any]:
        value: dict[str, Any] = {
            "channel": self.channel,
            "code": self.code,
            "message": self.message,
        }
        if self.path is not None:
            value["path"] = self.path
        return value


class ValidationFailure(Exception):
    def __init__(self, issues: list[ValidationIssue]):
        self.issues = issues
        super().__init__(f"manifest validation failed with {len(issues)} issue(s)")


class RenderError(Exception):
    pass


@dataclass
class RenderStats:
    completed_blocks: int = 0
    total_blocks: int = 0
    limiter_triggered_samples: int = 0
    extra: dict[str, Any] = field(default_factory=dict)

