from __future__ import annotations

from dataclasses import dataclass, field as dataclass_field
from enum import Enum
from typing import Any


class ErrorCode(str, Enum):
    """Stable diagnostic codes used by both the kernel and the workbench."""

    E_SEQ = "E_SEQ"
    E_CHAN = "E_CHAN"
    E_PARA = "E_PARA"
    E_PACK = "E_PACK"
    E_MISS = "E_MISS"
    E_DUP = "E_DUP"
    E_TIMEOUT = "E_TIMEOUT"
    E_LOCAL = "E_LOCAL"
    E_STATE = "E_STATE"
    E_DIVERGE = "E_DIVERGE"
    E_REORDER = "E_REORDER"
    E_HASH = "E_HASH"
    E_IO = "E_IO"


@dataclass(frozen=True)
class Issue:
    """A machine-readable diagnostic.

    `event_id`, `packet_seq`, `channel`, and `field` are deliberately optional:
    not every diagnostic has all of those coordinates.  Callers can still count
    on the relevant coordinates being populated for the required error classes.
    """

    code: ErrorCode
    message: str
    event_id: str | None = None
    packet_seq: int | None = None
    channel: int | None = None
    field: str | None = None
    first_seen_ms: float | None = None
    details: dict[str, Any] = dataclass_field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code.value,
            "message": self.message,
            "event_id": self.event_id,
            "packet_seq": self.packet_seq,
            "channel": self.channel,
            "field": self.field,
            "first_seen_ms": self.first_seen_ms,
            "details": dict(self.details),
        }

    def __str__(self) -> str:
        coords = []
        if self.event_id is not None:
            coords.append(f"event={self.event_id}")
        if self.packet_seq is not None:
            coords.append(f"packet={self.packet_seq}")
        if self.channel is not None:
            coords.append(f"channel={self.channel}")
        if self.field is not None:
            coords.append(f"field={self.field}")
        suffix = f" ({', '.join(coords)})" if coords else ""
        return f"{self.code.value}: {self.message}{suffix}"


class HapticError(Exception):
    """Exception wrapper for a single diagnostic issue."""

    def __init__(self, issue: Issue):
        super().__init__(str(issue))
        self.issue = issue

    @property
    def code(self) -> ErrorCode:
        return self.issue.code


class ValidationException(HapticError):
    def __init__(self, issues: list[Issue]):
        if not issues:
            raise ValueError("ValidationException requires at least one issue")
        self.issues = tuple(issues)
        super().__init__(issues[0])

    def __str__(self) -> str:
        return "; ".join(str(issue) for issue in self.issues)

