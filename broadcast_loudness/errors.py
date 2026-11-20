"""Stable error codes used by the command line and the GUI."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class LoudnessError(Exception):
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        if self.details:
            return f"{self.code}: {self.message} ({self.details})"
        return f"{self.code}: {self.message}"

    def as_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "details": self.details,
        }


class FormatUnsupportedError(LoudnessError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_FORMAT_UNSUPPORTED", message, details)


class BusyError(LoudnessError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_BUSY", message, details)


class LufsDivergenceError(LoudnessError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_LUFS_DIVERGENCE", message, details)


class ReportMismatchError(LoudnessError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_REPORT_MISMATCH", message, details)


class InvalidParameterError(LoudnessError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_INVALID_PARAMETER", message, details)
