"""Structured error types used by the engine, CLI, and UI."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

ALGORITHM_VERSION = "1.0.0"


@dataclass(frozen=True)
class Issue:
    code: str
    field: str
    message: str
    event_seq: int | None = None
    context: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "field": self.field,
            "event_seq": self.event_seq,
            "message": self.message,
            "context": dict(self.context),
        }


class StudioError(Exception):
    """Base exception carrying a stable machine-readable code."""

    default_code = "E_STUDIO"

    def __init__(
        self,
        message: str,
        *,
        field: str | None = None,
        event_seq: int | None = None,
        code: str | None = None,
        context: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code or self.default_code
        self.field = field or ""
        self.event_seq = event_seq
        self.context = dict(context or {})
        self.message = message

    def to_issue(self) -> Issue:
        return Issue(
            code=self.code,
            field=self.field,
            event_seq=self.event_seq,
            message=self.message,
            context=self.context,
        )

    def to_dict(self) -> dict[str, Any]:
        return self.to_issue().to_dict()


class SchemaError(StudioError):
    default_code = "E_SCHEMA"


class AxisError(StudioError):
    default_code = "E_AXIS"


class OrderError(StudioError):
    default_code = "E_ORDER"


class ParameterError(StudioError):
    default_code = "E_PARA"


class MappingError(StudioError):
    default_code = "E_MAP"


class DivergenceError(StudioError):
    default_code = "E_DIVERGE"


class SnapshotError(StudioError):
    default_code = "E_SNAPSHOT"


class SampleValidationError(StudioError):
    """Aggregates validation issues; the first issue determines the code."""

    def __init__(self, issues: list[Issue]) -> None:
        if not issues:
            raise ValueError("SampleValidationError requires at least one issue")
        first = issues[0]
        super().__init__(
            first.message,
            field=first.field,
            event_seq=first.event_seq,
            code=first.code,
            context={"issues": [issue.to_dict() for issue in issues]},
        )
        self.issues = list(issues)

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "field": self.field,
            "event_seq": self.event_seq,
            "message": self.message,
            "issues": [issue.to_dict() for issue in self.issues],
        }


__all__ = [
    "ALGORITHM_VERSION",
    "AxisError",
    "DivergenceError",
    "Issue",
    "MappingError",
    "OrderError",
    "ParameterError",
    "SampleValidationError",
    "SchemaError",
    "SnapshotError",
    "StudioError",
]

