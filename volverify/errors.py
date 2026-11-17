from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass(frozen=True)
class Issue:
    code: str
    scope: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "scope": self.scope,
            "message": self.message,
        }
        if self.details:
            payload["details"] = self.details
        return payload


class FrameRejected(Exception):
    def __init__(self, frame_id: str, issues: list[Issue]):
        self.frame_id = frame_id
        self.issues = issues
        super().__init__(
            f"frame {frame_id!r} rejected with "
            + ", ".join(issue.code for issue in issues)
        )


class RenderInterrupted(Exception):
    def __init__(self, signal_number: int):
        self.signal_number = signal_number
        super().__init__(f"render interrupted by signal {signal_number}")


class ConvergenceFailed(Exception):
    def __init__(self, frame_id: str, report: dict[str, Any]):
        self.frame_id = frame_id
        self.report = report
        super().__init__(f"frame {frame_id!r} failed convergence validation")

