from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


BUSINESS_EXIT_CODES: dict[str, int] = {
    "E_MANIFEST_JSON": 2,
    "E_MANIFEST_SCHEMA": 2,
    "E_SEED": 2,
    "E_INSTRUMENT_SET": 2,
    "E_CHANNEL_ID": 2,
    "E_OFFSET": 2,
    "E_GROUP": 2,
    "E_MISSING_FILE": 3,
    "E_AUDIO_READ": 4,
    "E_SAMPLE_RATE": 4,
    "E_BIT_DEPTH": 4,
    "E_CHANNELS": 4,
    "E_DUPLICATE_INSTRUMENT": 5,
    "E_ANCHOR_MISSING": 6,
    "E_ANCHOR_RANGE": 6,
    "E_TARGET_LOUDNESS": 7,
    "E_TRUE_PEAK_LIMIT": 7,
    "E_SILENT_MASTER": 8,
    "E_LOUDNESS_UNACHIEVABLE": 8,
    "E_OUTPUT": 9,
}


@dataclass
class StructuredIssue:
    code: str
    channel: str
    message: str
    instrument: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    @property
    def exit_code(self) -> int:
        return BUSINESS_EXIT_CODES.get(self.code, 2)

    def as_dict(self) -> dict[str, Any]:
        value: dict[str, Any] = {
            "code": self.code,
            "channel": self.channel,
            "message": self.message,
        }
        if self.instrument is not None:
            value["instrument"] = self.instrument
        if self.details:
            value["details"] = self.details
        return value


class MasterError(Exception):
    """A fatal, structured mastering error."""

    def __init__(self, issues: StructuredIssue | list[StructuredIssue]):
        self.issues = [issues] if isinstance(issues, StructuredIssue) else list(issues)
        super().__init__("; ".join(issue.message for issue in self.issues))

    @property
    def exit_code(self) -> int:
        if not self.issues:
            return 2
        return max(issue.exit_code for issue in self.issues)

    @property
    def primary_code(self) -> str:
        return self.issues[0].code if self.issues else "E_MANIFEST_SCHEMA"

    def as_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "event": "error",
            "exit_code": self.exit_code,
            "code": self.primary_code,
            "channel": self.issues[0].channel if self.issues else "MASTER",
            "errors": [issue.as_dict() for issue in self.issues],
        }


class IssueCollector:
    def __init__(self) -> None:
        self.issues: list[StructuredIssue] = []

    def add(
        self,
        code: str,
        channel: str,
        message: str,
        *,
        instrument: str | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        self.issues.append(
            StructuredIssue(
                code=code,
                channel=channel,
                message=message,
                instrument=instrument,
                details=details or {},
            )
        )

    def raise_if_any(self) -> None:
        if self.issues:
            raise MasterError(self.issues)
