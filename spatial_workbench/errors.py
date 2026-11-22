"""Typed error codes and the common error envelope."""

from __future__ import annotations

from enum import Enum
from typing import Any


class ErrorCode(str, Enum):
    E_META = "E_META"
    E_DECODE = "E_DECODE"
    E_PARA = "E_PARA"
    E_STATE = "E_STATE"
    E_DIFF = "E_DIFF"
    E_CHAN = "E_CHAN"
    E_ORDER = "E_ORDER"
    E_CLIP = "E_CLIP"
    E_FRAME = "E_FRAME"
    E_RESOURCE = "E_RESOURCE"
    E_IO = "E_IO"


class WorkbenchError(Exception):
    """An error carrying a stable code and a machine-readable error chain."""

    def __init__(
        self,
        code: ErrorCode | str,
        message: str,
        *,
        stage: str = "unknown",
        frame_index: int | None = None,
        channel_index: int | None = None,
        details: dict[str, Any] | None = None,
        cause: BaseException | None = None,
    ) -> None:
        self.code = ErrorCode(code)
        self.message = message
        self.stage = stage
        self.frame_index = frame_index
        self.channel_index = channel_index
        self.details = dict(details or {})
        self.cause = cause
        super().__init__(f"{self.code.value}: {message}")

    def to_dict(self) -> dict[str, Any]:
        chain = [
            {
                "code": self.code.value,
                "stage": self.stage,
                "message": self.message,
                "frame_index": self.frame_index,
                "channel_index": self.channel_index,
                "details": self.details,
            }
        ]
        cause = self.cause
        seen: set[int] = set()
        while cause is not None and id(cause) not in seen:
            seen.add(id(cause))
            chain.append(
                {
                    "type": type(cause).__name__,
                    "message": str(cause),
                }
            )
            cause = cause.__cause__
        return {
            "ok": False,
            "code": self.code.value,
            "message": self.message,
            "stage": self.stage,
            "frame_index": self.frame_index,
            "channel_index": self.channel_index,
            "details": self.details,
            "error_chain": chain,
        }

    @classmethod
    def wrap(
        cls,
        code: ErrorCode | str,
        message: str,
        exc: BaseException,
        **kwargs: Any,
    ) -> "WorkbenchError":
        return cls(code, message, cause=exc, **kwargs)

