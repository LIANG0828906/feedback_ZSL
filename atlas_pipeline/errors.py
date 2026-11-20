from __future__ import annotations

from enum import Enum
from typing import Any, Mapping


class ErrorCode(str, Enum):
    E_ATLAS_INVALID = "E_ATLAS_INVALID"
    E_PARAM_CONFLICT = "E_PARAM_CONFLICT"
    E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
    E_VERSION_STALE = "E_VERSION_STALE"
    E_BUSY = "E_BUSY"
    E_CANCELLED = "E_CANCELLED"
    E_EXPORT_INVALID = "E_EXPORT_INVALID"
    E_IO = "E_IO"
    E_USAGE = "E_USAGE"


class AtlasError(Exception):
    """A diagnosable pipeline error.

    ``field`` names the exact header/parameter field and ``tile`` identifies a
    block when the failure is block-local.  The error never carries a traceback
    in its serialized form so it remains deterministic.
    """

    def __init__(
        self,
        code: ErrorCode | str,
        message: str,
        *,
        field: str | None = None,
        tile: int | None = None,
        details: Mapping[str, Any] | None = None,
    ) -> None:
        self.code = code if isinstance(code, ErrorCode) else ErrorCode(code)
        self.message = str(message)
        self.field = field
        self.tile = tile
        self.details = dict(details or {})
        super().__init__(self.__str__())

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "code": self.code.value,
            "message": self.message,
        }
        if self.field is not None:
            out["field"] = self.field
        if self.tile is not None:
            out["tile"] = self.tile
        if self.details:
            out["details"] = self.details
        return out

    def __str__(self) -> str:
        where = ""
        if self.tile is not None:
            where += f" tile={self.tile}"
        if self.field is not None:
            where += f" field={self.field}"
        return f"{self.code.value}{where}: {self.message}"
