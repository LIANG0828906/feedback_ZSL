"""Stable business error codes.

The codes are part of the command line / API contract.  Callers should branch
on ``EngineError.code`` rather than on human readable messages.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


EXIT_CODES = {
    "IOV_SQL_PARSE_ERROR": 2,
    "IOV_SQL_UNSUPPORTED_JOIN": 2,
    "IOV_SQL_UNSUPPORTED_SUBQUERY": 2,
    "IOV_SQL_UNSUPPORTED_CTE": 2,
    "IOV_SQL_UNSUPPORTED_SET_OPERATION": 2,
    "IOV_SQL_UNSUPPORTED_STRUCTURE": 2,
    "IOV_SQL_UNSUPPORTED_AGGREGATE": 2,
    "IOV_SQL_UNSUPPORTED_WINDOW": 2,
    "IOV_SQL_UNSUPPORTED_GROUP": 2,
    "IOV_SQL_UNSUPPORTED_EXPRESSION": 2,
    "IOV_INPUT_INVALID": 3,
    "IOV_INPUT_EMPTY": 3,
    "IOV_DEPENDENCY_MISSING": 3,
    "IOV_STATE_INCOMPATIBLE": 6,
    "IOV_STATE_CORRUPT": 6,
    "IOV_RESOURCE_MEMORY": 4,
    "IOV_RESOURCE_TEMP": 4,
    "IOV_RESOURCE_CPU": 4,
    "IOV_STAGE_TIMEOUT": 5,
    "IOV_STAGE_TAKEOVER_EXCEEDED": 5,
    "IOV_WORKER_LOST": 5,
    "IOV_INTERNAL": 7,
}


@dataclass
class EngineError(Exception):
    code: str
    message: str
    fragment: str | None = None
    stage: str | None = None
    context: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(self.message)

    @property
    def exit_code(self) -> int:
        return EXIT_CODES.get(self.code, 7)

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "ok": False,
            "error": {
                "code": self.code,
                "message": self.message,
            },
        }
        if self.fragment:
            out["error"]["fragment"] = self.fragment
        if self.stage:
            out["error"]["stage"] = self.stage
        if self.context:
            out["error"]["context"] = self.context
        return out


def require(condition: bool, code: str, message: str, **kwargs: Any) -> None:
    if not condition:
        raise EngineError(code, message, **kwargs)

