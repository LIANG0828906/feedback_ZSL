"""统一错误码定义。"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


class ErrorCode:
    E_TERRAIN_INVALID = "E_TERRAIN_INVALID"
    E_PATCH_BOUNDS = "E_PATCH_BOUNDS"
    E_PATCH_STALE = "E_PATCH_STALE"
    E_PATCH_INVALID = "E_PATCH_INVALID"
    E_CRACK_UNRESOLVED = "E_CRACK_UNRESOLVED"
    E_COMMIT_FAILED = "E_COMMIT_FAILED"
    E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
    E_BUSY = "E_BUSY"
    E_CANCELLED = "E_CANCELLED"


@dataclass
class TerrainError(Exception):
    """携带稳定错误码的业务异常。"""

    code: str
    message: str = ""
    detail: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:  # pragma: no cover - 调试辅助
        if self.message:
            return f"[{self.code}] {self.message}"
        return f"[{self.code}]"
