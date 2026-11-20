"""Stable, user-facing error codes used by the engine and the GUI."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_MIX_INPUT = "E_MIX_INPUT"
E_SLICE_GAP = "E_SLICE_GAP"
E_BUSY = "E_BUSY"
E_BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_IO = "E_IO"
E_CONFIG = "E_CONFIG"
E_CANCELLED = "E_CANCELLED"


@dataclass
class MixError(Exception):
    """An error with a machine-readable code and optional location.

    ``track_id`` and ``slice_index`` make input and slicing failures directly
    actionable in the workbench.  ``details`` is deliberately JSON-safe so it
    can be emitted in a trace without exception formatting differences.
    """

    code: str
    message: str
    track_id: str | None = None
    slice_index: int | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)

    def to_dict(self) -> dict[str, Any]:
        value: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.track_id is not None:
            value["track_id"] = self.track_id
        if self.slice_index is not None:
            value["slice_index"] = self.slice_index
        if self.details:
            value["details"] = self.details
        return value

    def __str__(self) -> str:
        location = []
        if self.track_id is not None:
            location.append(f"track={self.track_id}")
        if self.slice_index is not None:
            location.append(f"slice={self.slice_index}")
        suffix = f" ({', '.join(location)})" if location else ""
        return f"{self.code}: {self.message}{suffix}"


def input_error(track_id: str, message: str, **details: Any) -> MixError:
    return MixError(E_MIX_INPUT, message, track_id=track_id, details=details)
