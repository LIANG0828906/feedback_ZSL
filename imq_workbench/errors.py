"""Stable error codes shared by the CLI, UI, and tests."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


ALGORITHM_VERSION = "dct-uniform-scalar-ssim-edge-v1"


@dataclass
class WorkbenchError(Exception):
    """A recoverable, machine-readable workbench failure."""

    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        if not self.details:
            return f"{self.code}: {self.message}"
        fields = ", ".join(f"{key}={value!r}" for key, value in sorted(self.details.items()))
        return f"{self.code}: {self.message} ({fields})"

    def to_dict(self) -> dict[str, Any]:
        return {"status": "error", "code": self.code, "message": self.message, "details": self.details}


def require(condition: bool, code: str, message: str, **details: Any) -> None:
    if not condition:
        raise WorkbenchError(code, message, details)


def parameter_error(field: str, message: str, **details: Any) -> WorkbenchError:
    return WorkbenchError("E_PARA", message, {"field": field, **details})
