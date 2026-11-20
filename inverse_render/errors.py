"""Stable machine-readable errors used by the command line and library API."""

from __future__ import annotations

from enum import StrEnum
from typing import Any


class ErrorCode(StrEnum):
    E_SCENE_INVALID = "E_SCENE_INVALID"
    E_GRAPH_INVALID = "E_GRAPH_INVALID"
    E_REUSE_MISMATCH = "E_REUSE_MISMATCH"
    E_CKPT_INVALID = "E_CKPT_INVALID"
    E_BUSY = "E_BUSY"
    E_SNAPSHOT_STALE = "E_SNAPSHOT_STALE"
    E_CONFIG_INVALID = "E_CONFIG_INVALID"
    E_IO = "E_IO"


class ToolError(RuntimeError):
    """An expected, structured failure.

    ``details`` is always JSON serializable.  ``step`` is populated for
    checkpoint failures so callers can locate the first anomalous commit.
    """

    def __init__(
        self,
        code: ErrorCode | str,
        message: str,
        *,
        details: dict[str, Any] | None = None,
        step: int | None = None,
    ) -> None:
        self.code = ErrorCode(code)
        self.message = message
        self.details = dict(details or {})
        if step is not None:
            self.details.setdefault("step", step)
        super().__init__(f"{self.code}: {message}")

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "ok": False,
            "code": str(self.code),
            "message": self.message,
            "details": self.details,
        }
        return result


def invalid_scene(message: str, **details: Any) -> ToolError:
    return ToolError(ErrorCode.E_SCENE_INVALID, message, details=details)


def invalid_graph(message: str, **details: Any) -> ToolError:
    return ToolError(ErrorCode.E_GRAPH_INVALID, message, details=details)


def invalid_checkpoint(message: str, *, step: int | None = None, **details: Any) -> ToolError:
    return ToolError(
        ErrorCode.E_CKPT_INVALID,
        message,
        details=details,
        step=step,
    )

