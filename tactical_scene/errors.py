"""Stable error codes and structured exceptions used by the public API."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


class ErrorCode:
    SCENE_INVALID = "E_SCENE_INVALID"
    BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
    OCCLUSION_AMBIGUOUS = "E_OCCLUSION_AMBIGUOUS"
    SCENE_STALE = "E_SCENE_STALE"
    STALE_HANDLE = "E_STALE_HANDLE"
    BUSY = "E_BUSY"
    TRACE_INVALID = "E_TRACE_INVALID"
    INVALID_ARGUMENT = "E_INVALID_ARGUMENT"


@dataclass(slots=True)
class TacticalError(Exception):
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"

    def to_dict(self) -> dict[str, Any]:
        return {
            "error": {
                "code": self.code,
                "message": self.message,
                "details": self.details,
            }
        }


def scene_invalid(message: str, *, unit_id: int | None = None, **details: Any) -> TacticalError:
    payload = dict(details)
    if unit_id is not None:
        payload["unit_id"] = unit_id
    return TacticalError(ErrorCode.SCENE_INVALID, message, payload)


def trace_invalid(message: str, **details: Any) -> TacticalError:
    return TacticalError(ErrorCode.TRACE_INVALID, message, details)
