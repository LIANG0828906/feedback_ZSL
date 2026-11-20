"""Stable error codes used by the CLI and tests."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


class ErrorCode:
    SCENE_INVALID = "E_SCENE_INVALID"
    SH_INVALID = "E_SH_INVALID"
    INTERP_FAILED = "E_INTERP_FAILED"
    VERSION_STALE = "E_VERSION_STALE"
    RECOVERY_INVALID = "E_RECOVERY_INVALID"
    BUSY = "E_BUSY"
    IO = "E_IO"
    CANCELLED = "E_CANCELLED"


@dataclass
class ProbeError(RuntimeError):
    code: str
    message: str
    probe_id: int | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        RuntimeError.__init__(self, f"{self.code}: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
        }
        if self.probe_id is not None:
            payload["probe_id"] = self.probe_id
        if self.details:
            payload["details"] = self.details
        return payload


class BakeCancelled(ProbeError):
    def __init__(self, message: str = "bake cancelled at a chunk checkpoint") -> None:
        super().__init__(ErrorCode.CANCELLED, message)

