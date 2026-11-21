from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_PARSE_INVALID = "E_PARSE_INVALID"
E_SHARE_INVALID = "E_SHARE_INVALID"
E_VERSION_LOSS = "E_VERSION_LOSS"
E_SELF_INTERSECT = "E_SELF_INTERSECT"
E_BUSY = "E_BUSY"
E_CANCELLED = "E_CANCELLED"
E_PREVIEW_STALE = "E_PREVIEW_STALE"
E_PROJECT_MISMATCH = "E_PROJECT_MISMATCH"
E_EXPORT_DEGRADED = "E_EXPORT_DEGRADED"
E_BOOLEAN_FAILED = "E_BOOLEAN_FAILED"
E_INTERNAL = "E_INTERNAL"


@dataclass(frozen=True)
class Diagnostic:
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "details": self.details,
        }


class PipelineError(Exception):
    """A stable, serializable failure at a pipeline boundary."""

    def __init__(
        self,
        code: str,
        message: str,
        **details: Any,
    ) -> None:
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message
        self.details = details

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "details": self.details,
        }

    def as_diagnostic(self) -> Diagnostic:
        return Diagnostic(self.code, self.message, dict(self.details))

