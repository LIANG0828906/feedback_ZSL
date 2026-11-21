from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_SCORE_INVALID = "E_SCORE_INVALID"
E_VERSION_LOSS = "E_VERSION_LOSS"
E_VERSION_STALE = "E_VERSION_STALE"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_CANCELLED = "E_CANCELLED"


@dataclass(frozen=True)
class ErrorLocation:
    note_id: str | None = None
    field: str | None = None
    part_id: str | None = None
    checkpoint: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            key: value
            for key, value in {
                "note_id": self.note_id,
                "field": self.field,
                "part_id": self.part_id,
                "checkpoint": self.checkpoint,
            }.items()
            if value is not None
        }


@dataclass
class EngineError(Exception):
    code: str
    message: str
    location: ErrorLocation | None = None
    details: list[dict[str, Any]] = field(default_factory=list)

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)

    @property
    def note_id(self) -> str | None:
        return self.location.note_id if self.location else None

    @property
    def field(self) -> str | None:
        return self.location.field if self.location else None

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.location:
            result["location"] = self.location.to_dict()
        if self.details:
            result["detail_count"] = len(self.details)
            result["details"] = self.details
        return result


def score_invalid(
    message: str,
    *,
    note_id: str | None = None,
    field: str | None = None,
    part_id: str | None = None,
    details: list[dict[str, Any]] | None = None,
) -> EngineError:
    return EngineError(
        E_SCORE_INVALID,
        message,
        ErrorLocation(note_id=note_id, field=field, part_id=part_id),
        details or [],
    )


def version_loss(message: str, details: list[dict[str, Any]]) -> EngineError:
    location = None
    if details:
        first = details[0]
        location = ErrorLocation(
            note_id=first.get("note_id"),
            field=first.get("field"),
            part_id=first.get("part_id"),
        )
    return EngineError(E_VERSION_LOSS, message, location, details)


def version_stale(message: str, *, expected: str, actual: str) -> EngineError:
    return EngineError(
        E_VERSION_STALE,
        message,
        details=[{"field": "content_hash", "expected": expected, "actual": actual}],
    )


def recovery_invalid(
    message: str,
    *,
    checkpoint: str | None = None,
    note_id: str | None = None,
    field: str | None = None,
    part_id: str | None = None,
    details: list[dict[str, Any]] | None = None,
) -> EngineError:
    return EngineError(
        E_RECOVERY_INVALID,
        message,
        ErrorLocation(
            note_id=note_id,
            field=field,
            part_id=part_id,
            checkpoint=checkpoint,
        ),
        details or [],
    )

