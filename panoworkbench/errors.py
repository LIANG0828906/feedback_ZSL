"""Stable error codes shared by the CLI, GUI, and tests."""

from __future__ import annotations

from dataclasses import dataclass, field as dc_field
from enum import Enum
from typing import Any


class ErrorCode(str, Enum):
    DECODE = "E_DECODE"
    META = "E_META"
    PARA = "E_PARA"
    MATCH = "E_MATCH"
    HOMO = "E_HOMO"
    CLIP = "E_CLIP"
    STATE = "E_STATE"
    IO = "E_IO"
    CANCEL = "E_CANCEL"


@dataclass
class WorkbenchError(Exception):
    """A structured error with a stable machine-readable code."""

    code: ErrorCode | str
    message: str
    filename: str | None = None
    field: str | None = None
    details: dict[str, Any] = dc_field(default_factory=dict)

    def __post_init__(self) -> None:
        self.code = ErrorCode(self.code)
        Exception.__init__(self, self.message)

    def __str__(self) -> str:
        location = ""
        if self.filename:
            location += f" file={self.filename}"
        if self.field:
            location += f" field={self.field}"
        return f"{self.code.value}: {self.message}{location}"

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code.value,
            "message": self.message,
        }
        if self.filename is not None:
            payload["filename"] = self.filename
        if self.field is not None:
            payload["field"] = self.field
        if self.details:
            payload["details"] = self.details
        return payload


def parameter_error(field: str, message: str, **details: Any) -> WorkbenchError:
    return WorkbenchError(ErrorCode.PARA, message, field=field, details=details)

