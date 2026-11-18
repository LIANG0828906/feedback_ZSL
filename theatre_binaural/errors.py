from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


EXIT_INPUT = 2
EXIT_RESUME_MISMATCH = 3
EXIT_SNAPSHOT_CORRUPT = 4
EXIT_RESUME_DIVERGENCE = 5
EXIT_RENDER = 6


@dataclass
class BusinessError(Exception):
    error_code: str
    channel: str
    message: str
    exit_code: int = EXIT_INPUT
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(self.message)

    def as_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "status": "error",
            "error_code": self.error_code,
            "channel": self.channel,
            "exit_code": self.exit_code,
            "message": self.message,
        }
        if self.details:
            payload["details"] = self.details
        return payload


def input_error(code: str, channel: str, message: str) -> BusinessError:
    return BusinessError(code, channel, message, EXIT_INPUT)

