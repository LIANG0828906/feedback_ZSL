"""Stable business error codes and structured exceptions."""

from __future__ import annotations

from dataclasses import dataclass, field as dc_field
from typing import Any


class ErrorCode:
    SCENE_INVALID = "E_SCENE_INVALID"
    CHUNK_INDEX = "E_CHUNK_INDEX"
    MEDIA_MISSING = "E_MEDIA_MISSING"
    CAMERA_NONFINITE = "E_CAMERA_NONFINITE"
    DENSITY_TRUNCATED = "E_DENSITY_TRUNCATED"
    CONFIG_INVALID = "E_CONFIG_INVALID"
    CONFIG_BUDGET = "E_CONFIG_BUDGET"
    CONFIG_PARALLELISM = "E_CONFIG_PARALLELISM"
    BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
    SEAM_UNRESOLVED = "E_SEAM_UNRESOLVED"
    CANCELLED = "E_CANCELLED"
    INTERNAL = "E_INTERNAL"


@dataclass
class BusinessError(Exception):
    """A deterministic, user-facing failure.

    ``field`` and ``offset`` identify the failing input location.  For binary
    inputs the offset is an absolute byte offset; for JSON configuration it is
    the character offset of the relevant key when available.
    """

    code: str
    message: str
    field: str = ""
    offset: int = -1
    details: dict[str, Any] = dc_field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, f"{self.code}: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "ok": False,
            "code": self.code,
            "message": self.message,
            "field": self.field,
            "offset": self.offset,
        }
        if self.details:
            result["details"] = self.details
        return result


class SceneError(BusinessError):
    def __init__(
        self,
        message: str,
        field: str,
        offset: int = -1,
        code: str = ErrorCode.SCENE_INVALID,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(code, message, field, offset, details or {})


class ConfigError(BusinessError):
    def __init__(
        self,
        message: str,
        field: str,
        offset: int = -1,
        code: str = ErrorCode.CONFIG_INVALID,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(code, message, field, offset, details or {})


class BudgetError(BusinessError):
    def __init__(self, message: str, details: dict[str, Any]) -> None:
        super().__init__(
            ErrorCode.BUDGET_EXCEEDED,
            message,
            field="scheduler.sample_budget",
            offset=-1,
            details=details,
        )


class SeamError(BusinessError):
    def __init__(self, message: str, details: dict[str, Any]) -> None:
        super().__init__(
            ErrorCode.SEAM_UNRESOLVED,
            message,
            field="render.seam",
            offset=-1,
            details=details,
        )


class CancelledError(BusinessError):
    def __init__(self, message: str = "render cancelled cooperatively") -> None:
        super().__init__(ErrorCode.CANCELLED, message, field="render.cancel", offset=-1)
