"""Structured replay errors used by both the engine and the CLI."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

E_REPLAY_INVALID = "E_REPLAY_INVALID"
E_CRACK_UNSTABLE = "E_CRACK_UNSTABLE"
E_BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
E_CREDIT_CONFLICT = "E_CREDIT_CONFLICT"
E_REVOKE_CLOSED = "E_REVOKE_CLOSED"
E_REVOKE_NOT_FOUND = "E_REVOKE_NOT_FOUND"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_BASELINE_VERSION = "E_BASELINE_VERSION"
E_BASELINE_MISMATCH = "E_BASELINE_MISMATCH"
E_CANCELLED = "E_CANCELLED"
E_IO = "E_IO"
E_NO_DISPLAY = "E_NO_DISPLAY"


@dataclass
class ReplayError(Exception):
    code: str
    message: str
    record_index: int | None = None
    field_path: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        location = ""
        if self.record_index is not None:
            location += f" record={self.record_index}"
        if self.field_path:
            location += f" field={self.field_path}"
        return f"{self.code}{location}: {self.message}"

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "record_index": self.record_index,
            "field_path": self.field_path,
            "details": self.details,
        }

