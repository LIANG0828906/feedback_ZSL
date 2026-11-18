from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


EXIT_CODES = {
    "OK": 0,
    "E_INVALID_PROJECT": 2,
    "E_DETECTION_FAILED": 3,
    "E_RESUME_MISMATCH": 4,
    "E_RESUME_DIFF": 5,
    "E_IO": 6,
}


@dataclass
class BusinessError(Exception):
    code: str
    message: str
    channel: str = "project"
    details: dict[str, Any] = field(default_factory=dict)

    @property
    def exit_code(self) -> int:
        return EXIT_CODES.get(self.code, 1)

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "business_code": self.code,
            "channel": self.channel,
            "message": self.message,
        }
        result.update(self.details)
        return result

    def __str__(self) -> str:
        return f"{self.code} [{self.channel}]: {self.message}"


def invalid_project(channel: str, message: str, **details: Any) -> BusinessError:
    return BusinessError("E_INVALID_PROJECT", message, channel, details)


def detection_failed(channel: str, message: str, **details: Any) -> BusinessError:
    return BusinessError("E_DETECTION_FAILED", message, channel, details)
