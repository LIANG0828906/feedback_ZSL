from __future__ import annotations

from dataclasses import dataclass
from enum import StrEnum
from typing import Any


class ErrorCode(StrEnum):
    """Stable machine-readable error codes used by all public entry points."""

    SCHEMA = "E_SCHEMA"
    CYCLE = "E_CYCLE"
    REF = "E_REF"
    PARA = "E_PARA"
    STATE = "E_STATE"
    DIFF = "E_DIFF"


@dataclass(eq=True)
class BalanceError(Exception):
    """A typed validation/runtime error with enough locality for diagnostics."""

    code: ErrorCode | str
    message: str
    object_id: str = ""
    field: str = ""
    detail: dict[str, Any] | None = None

    def __post_init__(self) -> None:
        if not isinstance(self.code, ErrorCode):
            try:
                self.code = ErrorCode(str(self.code))
            except ValueError as exc:
                raise ValueError(f"unknown error code: {self.code}") from exc
        Exception.__init__(self, self.message)

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code.value,
            "message": self.message,
            "object_id": self.object_id,
            "field": self.field,
        }
        if self.detail:
            payload["detail"] = self.detail
        return payload

    def __str__(self) -> str:
        location = f"{self.object_id}.{self.field}".strip(".")
        if location:
            return f"{self.code.value} [{location}] {self.message}"
        return f"{self.code.value} {self.message}"

