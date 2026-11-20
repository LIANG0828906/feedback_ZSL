"""Business errors with stable codes and field paths."""

from __future__ import annotations

from typing import Any


class DustRenderError(Exception):
    """A user-facing, machine-readable failure."""

    def __init__(
        self,
        code: str,
        message: str,
        field_path: str = "$",
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.field_path = field_path
        self.details = details or {}

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "field_path": self.field_path,
            "details": self.details,
        }


E_SCENE_INVALID = "E_SCENE_INVALID"
E_BUSY = "E_BUSY"
E_BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
E_SEAM_UNRESOLVED = "E_SEAM_UNRESOLVED"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_CANCELLED = "E_CANCELLED"
E_CONFIG_INVALID = "E_CONFIG_INVALID"


def scene_invalid(path: str, message: str, **details: Any) -> DustRenderError:
    return DustRenderError(E_SCENE_INVALID, message, path, details)


def config_invalid(path: str, message: str, **details: Any) -> DustRenderError:
    return DustRenderError(E_CONFIG_INVALID, message, path, details)
