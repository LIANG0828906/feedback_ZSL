"""Business error codes shared by the CLI, tests and the library API."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping


BUSINESS_EXIT_CODES: dict[str, int] = {
    "OK": 0,
    "E_INTERNAL": 1,
    "E_USAGE": 2,
    "E_SCENE_INVALID": 10,
    "E_RAY_INVALID": 11,
    "E_VERSION_STALE": 12,
    "E_STALE_HANDLE": 13,
    "E_REGION_INVALID": 14,
    "E_TRACE_INVALID": 15,
    "E_SCREEN_OUT_OF_RANGE": 16,
    "E_SNAPSHOT_CORRUPT": 17,
    "E_INJECTION_NOT_TRIGGERED": 18,
    "E_CHECK_FAILED": 20,
}


@dataclass
class ToolError(Exception):
    """A stable business failure, including the first conflicting input entry."""

    code: str
    message: str
    injection_point: str
    details: Mapping[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        if self.code not in BUSINESS_EXIT_CODES or self.code == "OK":
            raise ValueError(f"unknown business error code: {self.code}")
        Exception.__init__(self, self.message)

    @property
    def exit_code(self) -> int:
        return BUSINESS_EXIT_CODES[self.code]

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "business_code": self.code,
            "exit_code": self.exit_code,
            "message": self.message,
            "injection_point": self.injection_point,
            "details": dict(self.details),
        }


def fail(
    code: str,
    message: str,
    injection_point: str,
    **details: Any,
) -> None:
    raise ToolError(code, message, injection_point, details)


def result_document(ok: bool, business_code: str = "OK", **payload: Any) -> dict[str, Any]:
    return {
        "ok": ok,
        "business_code": business_code,
        "exit_code": BUSINESS_EXIT_CODES[business_code],
        **payload,
    }

