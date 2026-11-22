"""Structured domain errors with stable error codes."""

from __future__ import annotations

from dataclasses import dataclass, field as dataclass_field
from typing import Any


ERROR_CODES = {
    "E_META",
    "E_DECODE",
    "E_REG",
    "E_PARA",
    "E_STATE",
    "E_DIVERGE",
    "E_CHECKPOINT",
    "E_BUDGET",
    "E_QUALITY",
}


@dataclass
class SciSeqError(Exception):
    code: str
    message: str
    frame: int | None = None
    field: str | None = None
    details: dict[str, Any] = dataclass_field(default_factory=dict)

    def __post_init__(self) -> None:
        if self.code not in ERROR_CODES:
            raise ValueError(f"unknown error code: {self.code}")
        super().__init__(self.message)

    def __str__(self) -> str:
        parts = [f"[{self.code}] {self.message}"]
        if self.frame is not None:
            parts.append(f"frame={self.frame}")
        if self.field is not None:
            parts.append(f"field={self.field}")
        if self.details:
            detail = ", ".join(f"{key}={value!r}" for key, value in self.details.items())
            parts.append(detail)
        return " | ".join(parts)

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "frame": self.frame,
            "field": self.field,
            "details": self.details,
        }

