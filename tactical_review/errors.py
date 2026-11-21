"""Business error codes shared by the CLI, engine and GUI."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


class ReviewError(Exception):
    """An expected review failure carrying a stable business code."""

    def __init__(self, code: str, message: str, **details: Any) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details

    def to_dict(self) -> dict[str, Any]:
        return {"code": self.code, "message": self.message, **self.details}


class SimulationError(ReviewError):
    """A fatal simulation error."""


class SnapshotError(ReviewError):
    """A snapshot or recovery error."""


@dataclass
class ErrorSink:
    """Collects non-fatal operational reports without muting them."""

    errors: list[dict[str, Any]] = field(default_factory=list)

    def add(self, code: str, step: int | None = None, **details: Any) -> None:
        item: dict[str, Any] = {"code": code}
        if step is not None:
            item["step"] = step
        item.update(details)
        self.errors.append(item)

    def extend(self, items: list[dict[str, Any]]) -> None:
        self.errors.extend(items)

