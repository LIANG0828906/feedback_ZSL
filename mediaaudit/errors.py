"""统一错误码与异常类型。"""

from __future__ import annotations

from enum import Enum
from typing import Any


class ErrorCode(str, Enum):
    """业务错误码（字符串值即对外稳定标识）。"""

    E_SOURCE_INVALID = "E_SOURCE_INVALID"
    E_SPAN_MISMATCH = "E_SPAN_MISMATCH"
    E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
    E_BUSY = "E_BUSY"
    E_STATE_INVALID = "E_STATE_INVALID"
    E_NOT_FOUND = "E_NOT_FOUND"


class AuditError(Exception):
    """携带错误码与结构化定位信息的业务异常。

    ``location`` 用于定位首个非法字段，例如 ``("frames[2]", "time_s")``；
    ``details`` 携带哈希、偏移量等附加证据，保证问题可追溯。
    """

    def __init__(
        self,
        code: ErrorCode,
        message: str,
        location: tuple[str, ...] | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.location = location
        self.details = details or {}

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code.value,
            "message": self.message,
            "location": list(self.location) if self.location else [],
            "details": self.details,
        }

    def __str__(self) -> str:  # pragma: no cover - 调试辅助
        loc = ".".join(self.location) if self.location else "-"
        return f"[{self.code.value}] {self.message} (at {loc})"
