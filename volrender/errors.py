"""Business error codes shared by the rendering component and the UI."""

from __future__ import annotations

from typing import Any


E_IO = "E_IO"
E_FORMAT = "E_FORMAT"
E_INPUT_INVALID = "E_INPUT_INVALID"
E_TF_INVALID = "E_TF_INVALID"
E_MODE_INVALID = "E_MODE_INVALID"
E_LIGHT_DRIFT = "E_LIGHT_DRIFT"
E_SNAPSHOT_INVALID = "E_SNAPSHOT_INVALID"
E_SNAPSHOT_STALE = "E_SNAPSHOT_STALE"
E_BUSY = "E_BUSY"


class VolRenderError(Exception):
    """A stable, user-facing error with a machine-readable business code."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details or {}

    def as_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "details": self.details,
        }

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"
