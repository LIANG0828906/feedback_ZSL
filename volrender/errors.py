"""Business error model shared by loaders, renderers, and CLIs."""

from __future__ import annotations

from dataclasses import dataclass, field as dataclass_field
from enum import StrEnum
from typing import Any


class ErrorCode(StrEnum):
    E_SCENE_INVALID = "E_SCENE_INVALID"
    E_BUDGET_CONFIG_INVALID = "E_BUDGET_CONFIG_INVALID"
    E_BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
    E_SEAM_UNRESOLVED = "E_SEAM_UNRESOLVED"
    E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
    E_BUSY = "E_BUSY"
    E_TILE_INDEX_CORRUPT = "E_TILE_INDEX_CORRUPT"
    E_MEDIUM_MISSING = "E_MEDIUM_MISSING"
    E_CAMERA_MATRIX_NONFINITE = "E_CAMERA_MATRIX_NONFINITE"
    E_DENSITY_TRUNCATED = "E_DENSITY_TRUNCATED"
    E_DISPLAY_UNAVAILABLE = "E_DISPLAY_UNAVAILABLE"
    E_INTERRUPTED = "E_INTERRUPTED"
    E_IO = "E_IO"
    E_USAGE = "E_USAGE"


@dataclass(slots=True)
class AppError(Exception):
    code: ErrorCode
    message: str
    field: str | None = None
    medium_id: str | None = None
    injection_point: str | None = None
    details: dict[str, Any] = dataclass_field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "ok": False,
            "code": self.code.value,
            "message": self.message,
        }
        if self.field is not None:
            result["field"] = self.field
        if self.medium_id is not None:
            result["medium_id"] = self.medium_id
        if self.injection_point is not None:
            result["injection_point"] = self.injection_point
        if self.details:
            result["details"] = self.details
        return result


def scene_error(
    message: str,
    *,
    field_name: str | None = None,
    medium_id: str | None = None,
) -> AppError:
    return AppError(
        ErrorCode.E_SCENE_INVALID,
        message,
        field=field_name,
        medium_id=medium_id,
    )
