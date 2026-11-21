from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any


class ErrorCode(str, Enum):
    PATH_INVALID = "E_PATH_INVALID"
    NON_FINITE_COORDINATE = "E_NON_FINITE_COORDINATE"
    DEGENERATE_SEGMENT = "E_DEGENERATE_SEGMENT"
    ZERO_AREA_CONTOUR = "E_ZERO_AREA_CONTOUR"
    COINCIDENT_EDGE = "E_COINCIDENT_EDGE"
    HOLE_REVERSED = "E_HOLE_REVERSED"
    INTERSECT_AMBIGUOUS = "E_INTERSECT_AMBIGUOUS"
    SELF_INTERSECT = "E_SELF_INTERSECT"
    RESULT_DEGRADED = "E_RESULT_DEGRADED"
    BUSY = "E_BUSY"
    CANCELLED = "E_CANCELLED"
    PROJECT_MISMATCH = "E_PROJECT_MISMATCH"
    IO = "E_IO"
    BOOLEAN_FAILED = "E_BOOLEAN_FAILED"


@dataclass(frozen=True)
class ErrorLocation:
    path: str | None = None
    subpath: int | None = None
    segment: int | None = None
    parameter: float | None = None
    point: tuple[float, float] | None = None
    injection: str | None = None

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {}
        for key in ("path", "subpath", "segment", "parameter", "injection"):
            value = getattr(self, key)
            if value is not None:
                result[key] = value
        if self.point is not None:
            result["point"] = [float(self.point[0]), float(self.point[1])]
        return result


@dataclass
class BusinessError(Exception):
    code: ErrorCode | str
    message: str
    location: ErrorLocation = field(default_factory=ErrorLocation)
    details: dict[str, Any] = field(default_factory=dict)
    parent_code: str | None = None

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)

    def to_dict(self) -> dict[str, Any]:
        code = self.code.value if isinstance(self.code, ErrorCode) else str(self.code)
        payload: dict[str, Any] = {
            "ok": False,
            "code": code,
            "message": self.message,
            "location": self.location.to_dict(),
        }
        if self.parent_code:
            payload["parent_code"] = self.parent_code
        if self.details:
            payload["details"] = self.details
        return payload


def fail(
    code: ErrorCode | str,
    message: str,
    *,
    path: str | None = None,
    subpath: int | None = None,
    segment: int | None = None,
    parameter: float | None = None,
    point: tuple[float, float] | None = None,
    injection: str | None = None,
    parent_code: str | None = None,
    **details: Any,
) -> None:
    raise BusinessError(
        code=code,
        message=message,
        location=ErrorLocation(
            path=path,
            subpath=subpath,
            segment=segment,
            parameter=parameter,
            point=point,
            injection=injection,
        ),
        details=details,
        parent_code=parent_code,
    )

