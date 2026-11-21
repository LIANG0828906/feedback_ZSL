from __future__ import annotations

from dataclasses import dataclass, field as dataclass_field
from enum import Enum
from typing import Any


class ErrorCode(str, Enum):
    SURF_INVALID = "E_SURF_INVALID"
    DEP_GRAPH_INCOMPLETE = "E_DEP_GRAPH_INCOMPLETE"
    INCR_MISMATCH = "E_INCR_MISMATCH"
    RECOVERY_INVALID = "E_RECOVERY_INVALID"
    STALE_HANDLE = "E_STALE_HANDLE"
    BUSY = "E_BUSY"
    RESOURCE_LIMIT = "E_RESOURCE_LIMIT"
    NOT_FOUND = "E_NOT_FOUND"
    INTERNAL = "E_INTERNAL"


@dataclass
class SurfaceViewerError(Exception):
    """A stable, machine-readable error.

    ``field`` is a location such as ``patches[2].precision_level``.  It is kept
    separate from ``message`` so callers (and tests) do not need to parse text.
    """

    code: ErrorCode
    message: str
    field: str | None = None
    details: dict[str, Any] = dataclass_field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"code": self.code.value, "message": self.message}
        if self.field is not None:
            result["field"] = self.field
        if self.details:
            result["details"] = self.details
        return result


def invalid(field: str, message: str, **details: Any) -> SurfaceViewerError:
    return SurfaceViewerError(
        ErrorCode.SURF_INVALID, message=message, field=field, details=details
    )
