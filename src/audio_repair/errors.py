"""Business error codes used by both the core engine and the desktop UI."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class AppError(Exception):
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, f"{self.code}: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "code": self.code,
            "message": self.message,
            "details": self.details,
        }


def business_error(code: str, message: str, **details: Any) -> AppError:
    return AppError(code=code, message=message, details=details)


def param_error(message: str, **details: Any) -> AppError:
    return business_error("E_PARAM", message, **details)


def state_error(message: str, **details: Any) -> AppError:
    return business_error("E_STATE", message, **details)


def decode_error(message: str, **details: Any) -> AppError:
    return business_error("E_DECODE", message, **details)


def io_error(message: str, **details: Any) -> AppError:
    return business_error("E_IO", message, **details)

