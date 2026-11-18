"""Stable business error codes used by the CLI and the engine."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


EXIT_CODES = {
    "E_INPUT": 2,
    "E_STATE": 3,
    "E_MEMORY": 4,
    "E_INTERNAL": 5,
    "E_INTERRUPTED": 130,
}


@dataclass
class EngineError(Exception):
    code: str
    message: str
    subject: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(self.message)

    @property
    def exit_code(self) -> int:
        return EXIT_CODES.get(self.code, 5)

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.subject is not None:
            payload["subject"] = self.subject
        if self.details:
            payload["details"] = self.details
        return payload


def input_error(
    message: str,
    subject: str,
    *,
    row: int | None = None,
    field_name: str | None = None,
    details: dict[str, Any] | None = None,
) -> EngineError:
    merged: dict[str, Any] = dict(details or {})
    if row is not None:
        merged["row"] = row
    if field_name is not None:
        merged["field"] = field_name
    return EngineError("E_INPUT", message, subject=subject, details=merged)


def state_error(message: str, *, subject: str | None = None) -> EngineError:
    return EngineError("E_STATE", message, subject=subject)


def memory_error(message: str, **details: Any) -> EngineError:
    return EngineError("E_MEMORY", message, details=details)
