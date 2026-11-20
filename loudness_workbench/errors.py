"""Business error codes used by the workbench.

Errors are intentionally small and serialisable.  The UI and the CLI can
therefore expose a stable code (for example ``E_CHANNEL_UNSUPPORTED``) without
having to parse an exception message.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping


class ErrorCode:
    E_CHANNEL_UNSUPPORTED = "E_CHANNEL_UNSUPPORTED"
    E_AUDIO_FORMAT = "E_AUDIO_FORMAT"
    E_CONFIG_INVALID = "E_CONFIG_INVALID"
    E_BUSY = "E_BUSY"
    E_BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
    E_LUFS_MISMATCH = "E_LUFS_MISMATCH"
    E_REPORT_MISMATCH = "E_REPORT_MISMATCH"
    E_BLOCK_NOT_READY = "E_BLOCK_NOT_READY"
    E_TASK_FAILED = "E_TASK_FAILED"


@dataclass
class MeterError(Exception):
    """A stable, contextual workbench error.

    ``context`` is intentionally JSON-compatible.  It is copied into trace
    events and CLI results, which makes failures reproducible.
    """

    code: str
    message: str
    context: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "code": self.code,
            "message": self.message,
            "context": self.context,
        }

    def __str__(self) -> str:
        if not self.context:
            return f"{self.code}: {self.message}"
        return f"{self.code}: {self.message} {self.context}"


def error(
    code: str,
    message: str,
    context: Mapping[str, Any] | None = None,
) -> MeterError:
    return MeterError(code=code, message=message, context=dict(context or {}))
