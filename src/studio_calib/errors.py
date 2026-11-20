from __future__ import annotations

from dataclasses import dataclass


@dataclass
class CalibrationError(Exception):
    code: str
    message: str
    parameter: str | None = None

    def __str__(self) -> str:
        if self.parameter:
            return f"{self.code}: {self.message} (parameter={self.parameter})"
        return f"{self.code}: {self.message}"

    def to_dict(self) -> dict[str, str | None]:
        return {
            "code": self.code,
            "message": self.message,
            "parameter": self.parameter,
        }

