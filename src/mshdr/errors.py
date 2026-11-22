"""Stable error codes used by the workbench and its CLI."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


EXIT_CODES = {
    "E_META": 2,
    "E_DECODE": 3,
    "E_REG": 4,
    "E_PARA": 5,
    "E_GAMUT": 6,
    "E_STATE": 7,
    "E_VER": 8,
    "E_ORDER": 9,
    "E_SEG": 10,
    "E_DIVERGE": 11,
    "E_IO": 12,
}


@dataclass(init=False)
class WorkbenchError(Exception):
    """An expected, machine-readable workbench failure."""

    code: str
    message: str
    field: str | None = None
    details: dict[str, Any]

    def __init__(
        self,
        code: str,
        message: str,
        field: str | None = None,
        details: dict[str, Any] | None = None,
        **extra_details: Any,
    ) -> None:
        self.code = code
        self.message = message
        self.field = field
        self.details = {**(details or {}), **extra_details}
        super().__init__(self.message)

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "ok": False,
            "code": self.code,
            "message": self.message,
        }
        if self.field is not None:
            payload["field"] = self.field
        if self.details:
            payload["details"] = self.details
        return payload

    @property
    def exit_code(self) -> int:
        return EXIT_CODES.get(self.code, 1)


def require(condition: bool, code: str, message: str, field: str | None = None, **details: Any) -> None:
    if not condition:
        raise WorkbenchError(code=code, message=message, field=field, details=details)

