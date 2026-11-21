"""Business error types and stable error codes."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_INPUT_INVALID = "E_INPUT_INVALID"
E_MODE_INVALID = "E_MODE_INVALID"
E_ATTR_UNRESOLVED = "E_ATTR_UNRESOLVED"
E_VERSION_LOSS = "E_VERSION_LOSS"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_BASELINE_VERSION = "E_BASELINE_VERSION"
E_BASELINE_CORRUPT = "E_BASELINE_CORRUPT"
E_DUP_CLIENT_SEQ = "E_DUP_CLIENT_SEQ"
E_COORD_RANGE = "E_COORD_RANGE"
E_NONFINITE = "E_NONFINITE"
E_LATE_OP = "E_LATE_OP"
E_BUSY = "E_BUSY"


@dataclass
class ReplayError(Exception):
    """A rejected replay request with a machine-readable location.

    ``path`` points at the first offending protocol field.  ``step`` is set for
    errors discovered while advancing.  ``injection`` describes the entry-point
    mutation that made the error reachable, when one was requested.
    """

    code: str
    message: str
    path: str | None = None
    step: int | None = None
    details: dict[str, Any] = field(default_factory=dict)
    injection: dict[str, Any] | None = None

    def __post_init__(self) -> None:
        super().__init__(self.message)

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "ok": False,
            "code": self.code,
            "message": self.message,
        }
        if self.path is not None:
            result["path"] = self.path
        if self.step is not None:
            result["step"] = self.step
        if self.details:
            result["details"] = self.details
        if self.injection is not None:
            result["injection"] = self.injection
        return result

    def with_injection(self, injection: dict[str, Any] | None) -> "ReplayError":
        if injection is not None and self.injection is None:
            self.injection = injection
        return self


def fail(
    code: str,
    message: str,
    path: str | None = None,
    *,
    step: int | None = None,
    details: dict[str, Any] | None = None,
    injection: dict[str, Any] | None = None,
) -> None:
    raise ReplayError(
        code=code,
        message=message,
        path=path,
        step=step,
        details=details or {},
        injection=injection,
    )
