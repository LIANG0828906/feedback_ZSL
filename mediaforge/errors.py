from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_META = "E_META"
E_TRACK = "E_TRACK"
E_CHAP = "E_CHAP"
E_VER = "E_VER"
E_LOSSY = "E_LOSSY"
E_PREC = "E_PREC"
E_CONFLICT = "E_CONFLICT"
E_ORDER = "E_ORDER"
E_CORRUPT = "E_CORRUPT"
E_BUFFER = "E_BUFFER"
E_PARAM = "E_PARAM"
E_IO = "E_IO"
E_VALIDATE = "E_VALIDATE"


@dataclass
class WorkbenchError(Exception):
    code: str
    field_path: str
    message: str
    measured: Any = None
    declared: Any = None
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, f"{self.code} at {self.field_path}: {self.message}")

    @property
    def path(self) -> str:
        return self.field_path

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "field_path": self.field_path,
            "message": self.message,
        }
        if self.measured is not None:
            payload["measured"] = self.measured
        if self.declared is not None:
            payload["declared"] = self.declared
        if self.details:
            payload["details"] = self.details
        return payload


@dataclass(frozen=True)
class LossyWarning:
    field_path: str
    message: str
    source_value: Any = None
    target_version: str = ""
    mapping: str = "dropped"

    @property
    def code(self) -> str:
        return E_LOSSY

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": E_LOSSY,
            "field_path": self.field_path,
            "message": self.message,
            "target_version": self.target_version,
            "mapping": self.mapping,
        }
        if self.source_value is not None:
            payload["source_value"] = self.source_value
        return payload


def fail(
    code: str,
    field_path: str,
    message: str,
    *,
    measured: Any = None,
    declared: Any = None,
    details: dict[str, Any] | None = None,
) -> None:
    raise WorkbenchError(
        code=code,
        field_path=field_path,
        message=message,
        measured=measured,
        declared=declared,
        details=details or {},
    )
