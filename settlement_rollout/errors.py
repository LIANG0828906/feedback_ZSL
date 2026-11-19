from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class RolloutError(Exception):
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)
    exit_code: int = 3

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"

    def as_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "code": self.code,
            "message": self.message,
            "details": self.details,
        }


class ConfigError(RolloutError):
    def __init__(
        self,
        code: str,
        message: str,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(
            code=code,
            message=message,
            details=details or {},
            exit_code=2,
        )


class StateError(RolloutError):
    pass


class InstanceError(RolloutError):
    pass


CFG_FILE_UNREADABLE = "CFG_FILE_UNREADABLE"
CFG_JSON_INVALID = "CFG_JSON_INVALID"
CFG_SCHEMA_INVALID = "CFG_SCHEMA_INVALID"
CFG_UNKNOWN_INSTANCE = "CFG_UNKNOWN_INSTANCE"
CFG_EMPTY_BATCH = "CFG_EMPTY_BATCH"
CFG_DUPLICATE_INSTANCE = "CFG_DUPLICATE_INSTANCE"
CFG_DUPLICATE_BATCH = "CFG_DUPLICATE_BATCH"
CFG_BATCH_ORDER = "CFG_BATCH_ORDER"
CFG_TARGET_RANGE = "CFG_TARGET_RANGE"
CFG_WINDOW_INVALID = "CFG_WINDOW_INVALID"
CFG_SAMPLE_INVALID = "CFG_SAMPLE_INVALID"
CFG_PORT_INVALID = "CFG_PORT_INVALID"

INSTANCE_PRECHECK_FAILED = "INSTANCE_PRECHECK_FAILED"
INSTANCE_SWITCH_FAILED = "INSTANCE_SWITCH_FAILED"
INSTANCE_RESTART_REFUSED = "INSTANCE_RESTART_REFUSED"
INSTANCE_UNAVAILABLE = "INSTANCE_UNAVAILABLE"
INSTANCE_STATS_FAILED = "INSTANCE_STATS_FAILED"
ROLLBACK_VERIFICATION_FAILED = "ROLLBACK_VERIFICATION_FAILED"
STATE_CONFIG_MISMATCH = "STATE_CONFIG_MISMATCH"
STATE_TERMINAL = "STATE_TERMINAL"
