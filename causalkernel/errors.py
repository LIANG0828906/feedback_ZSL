"""Business error types used across the kernel."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class KernelError(Exception):
    code: str
    message: str
    location: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(f"{self.code}: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "location": self.location,
        }


E_SCENARIO_INVALID = "E_SCENARIO_INVALID"
E_TRACE_INVALID = "E_TRACE_INVALID"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_ATTRIBUTION_FAILED = "E_ATTRIBUTION_FAILED"

E_INJECT_CYCLE = "E_INJECT_CYCLE"
E_INJECT_PRECISION_CONFLICT = "E_INJECT_PRECISION_CONFLICT"
E_INJECT_DEPTH_EXCEEDED = "E_INJECT_DEPTH_EXCEEDED"
E_INJECT_NONFINITE_COEFFICIENT = "E_INJECT_NONFINITE_COEFFICIENT"
E_INJECT_TRUNCATED = "E_INJECT_TRUNCATED"

E_EVENT_NOT_TRIGGERED = "E_EVENT_NOT_TRIGGERED"
