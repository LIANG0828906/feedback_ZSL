"""Structured business errors and integer process exit codes."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


ERROR_EXIT_CODES = {
    "E_SCHEMA": 2,
    "E_PARA": 3,
    "E_GRAD": 4,
    "E_STATE": 5,
    "E_DECODE": 6,
    "E_DIFF": 7,
    "E_CHECKPOINT": 8,
    "E_BLOCK": 9,
    "E_IO": 10,
    "E_INTERNAL": 11,
}


@dataclass
class AppError(Exception):
    """A business failure carrying a stable code and actionable context."""

    code: str
    message: str
    field_path: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        if not self.code.startswith("E_"):
            raise ValueError(f"business error code must start with E_: {self.code}")
        Exception.__init__(self, self.message)

    @property
    def exit_code(self) -> int:
        return ERROR_EXIT_CODES.get(self.code, ERROR_EXIT_CODES["E_INTERNAL"])

    def to_dict(self) -> dict[str, Any]:
        body: dict[str, Any] = {
            "ok": False,
            "code": self.code,
            "message": self.message,
            "exit_code": self.exit_code,
        }
        if self.field_path is not None:
            body["field_path"] = self.field_path
        if self.details:
            body["details"] = self.details
        return body


def fail(
    code: str,
    message: str,
    field_path: str | None = None,
    **details: Any,
) -> "NoReturn":
    raise AppError(code=code, message=message, field_path=field_path, details=details)


from typing import NoReturn  # noqa: E402  (kept after the public error type)

