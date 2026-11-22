"""Stable error codes shared by the CLI, GUI and tests."""

from __future__ import annotations

from dataclasses import dataclass, field as dataclass_field
from typing import Any, Dict, Optional


class ErrorCode:
    PARA = "E_PARA"
    STATE = "E_STATE"
    DIFF = "E_DIFF"
    BAND = "E_BAND"
    DECODE = "E_DECODE"
    IO = "E_IO"
    CANCELLED = "E_CANCELLED"
    INTERNAL = "E_INTERNAL"


@dataclass
class EdgePipeError(Exception):
    code: str
    message: str
    field: Optional[str] = None
    details: Dict[str, Any] = dataclass_field(default_factory=dict)

    def __str__(self) -> str:
        parts = [self.code, self.message]
        if self.field:
            parts.append(f"field={self.field}")
        return " | ".join(parts)

    def to_dict(self) -> Dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "field": self.field,
            "details": self.details,
        }

