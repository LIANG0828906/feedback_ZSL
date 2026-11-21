"""Business errors shared by the volume rendering components."""

from __future__ import annotations

from enum import Enum
from typing import Any, Mapping


class ErrorCode(str, Enum):
    E_HEADER_INVALID = "E_HEADER_INVALID"
    E_TF_INVALID = "E_TF_INVALID"
    E_PARAM_STALE = "E_PARAM_STALE"
    E_PREVIEW_MISMATCH = "E_PREVIEW_MISMATCH"
    E_SNAPSHOT_INVALID = "E_SNAPSHOT_INVALID"
    E_BUSY = "E_BUSY"
    E_MASK_INVALID = "E_MASK_INVALID"


class VolumeRenderError(Exception):
    """An error that can be shown directly to an operator or logged as JSON."""

    def __init__(
        self,
        code: ErrorCode | str,
        message: str,
        *,
        field: str | None = None,
        details: Mapping[str, Any] | None = None,
    ) -> None:
        self.code = ErrorCode(code)
        self.message = message
        self.field = field
        self.details = dict(details or {})
        super().__init__(self._render_message())

    def _render_message(self) -> str:
        location = f" [{self.field}]" if self.field else ""
        return f"{self.code.value}{location}: {self.message}"

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code.value,
            "message": self.message,
        }
        if self.field is not None:
            payload["field"] = self.field
        if self.details:
            payload["details"] = self.details
        return payload


def require(condition: bool, code: ErrorCode, message: str, field: str | None = None) -> None:
    if not condition:
        raise VolumeRenderError(code, message, field=field)
