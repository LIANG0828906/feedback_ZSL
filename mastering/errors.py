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


class HardError(Exception):
    def __init__(self, issues: list[Issue], message: str = "project rejected"):
        super().__init__(message)
        self.issues = issues


class QualityWarning(UserWarning):
    pass


class WavFormatError(Exception):
    pass


class DetectionFailed(Exception):
    pass
