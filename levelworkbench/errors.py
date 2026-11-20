"""Business error types and stable error codes."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_CONFIG_INVALID = "E_CONFIG_INVALID"
E_CONSTRAINT_INVALID = "E_CONSTRAINT_INVALID"
E_CONSTRAINT_CONFLICT = "E_CONSTRAINT_CONFLICT"
E_GENERATION_FAILED = "E_GENERATION_FAILED"
E_PHASE_FAILED = "E_PHASE_FAILED"
E_TRACE_INVALID = "E_TRACE_INVALID"
E_REPLAY_MISMATCH = "E_REPLAY_MISMATCH"
E_CANCELLED = "E_CANCELLED"
E_BUSY = "E_BUSY"
E_IO = "E_IO"

E_INJECT_ROOM_BOUNDS = "E_INJECT_ROOM_BOUNDS"
E_INJECT_DANGLING_DOOR = "E_INJECT_DANGLING_DOOR"
E_INJECT_KEY_CYCLE = "E_INJECT_KEY_CYCLE"
E_INJECT_CONSTRAINT_LIMIT = "E_INJECT_CONSTRAINT_LIMIT"
E_INJECT_TRUNCATED_INPUT = "E_INJECT_TRUNCATED_INPUT"


@dataclass
class WorkbenchError(Exception):
    """An error safe to expose through CLI/UI JSON."""

    code: str
    message: str
    stage: str | None = None
    constraint_id: str | None = None
    injection_point: dict[str, Any] | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(self.message)

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "ok": False,
            "code": self.code,
            "message": self.message,
        }
        if self.stage is not None:
            payload["stage"] = self.stage
        if self.constraint_id is not None:
            payload["constraint_id"] = self.constraint_id
        if self.injection_point is not None:
            payload["injection_point"] = self.injection_point
        if self.details:
            payload["details"] = self.details
        return payload
