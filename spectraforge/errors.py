"""Stable error codes used by the CLI, core pipeline, and UI."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


ERROR_EXIT_CODES = {
    "E_META": 10,
    "E_DIM": 11,
    "E_DECODE": 12,
    "E_PARA": 20,
    "E_GAMUT": 21,
    "E_STATE": 30,
    "E_DIVERGE": 40,
    "E_INCOMPLETE": 41,
    "E_LOCK": 42,
    "E_IO": 50,
    "E_CANCEL": 60,
    "E_INTERNAL": 70,
}


@dataclass
class WorkbenchError(Exception):
    """A user-visible failure with a machine-readable code."""

    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(f"{self.code}: {self.message}")

    @property
    def exit_code(self) -> int:
        return ERROR_EXIT_CODES.get(self.code, 1)

    def to_dict(self) -> dict[str, Any]:
        return {"ok": False, "code": self.code, "message": self.message, "details": self.details}

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"


def require(condition: bool, code: str, message: str, **details: Any) -> None:
    if not condition:
        raise WorkbenchError(code, message, details)
