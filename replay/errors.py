"""业务错误码与带定位的异常类型。

所有解析/解码/恢复/比对失败都抛出 :class:`ReplayError`，携带稳定业务码、
记录序号与字段路径，禁止静默降级为部分解析。
"""
from __future__ import annotations

from enum import Enum


class ErrorCode(str, Enum):
    # 格式与解析
    E_FORMAT_INVALID = "E_FORMAT_INVALID"
    E_FORMAT_ORDER = "E_FORMAT_ORDER"
    E_FORMAT_VERSION_UNKNOWN = "E_FORMAT_VERSION_UNKNOWN"
    # 操作缓冲
    E_OP_LATE = "E_OP_LATE"
    E_OP_DUPLICATE = "E_OP_DUPLICATE"
    E_OP_OUT_OF_BOUNDS = "E_OP_OUT_OF_BOUNDS"
    # 差分解码
    E_DIFF_INVALID = "E_DIFF_INVALID"
    E_DIFF_TRUNCATED = "E_DIFF_TRUNCATED"
    # 快照恢复
    E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
    # 基线
    E_BASELINE_VERSION = "E_BASELINE_VERSION"
    E_BASELINE_MISSING = "E_BASELINE_MISSING"
    # 任务并发
    E_BUSY = "E_BUSY"


class ReplayError(Exception):
    """带业务码、记录序号、字段路径与注入点的确定性异常。"""

    def __init__(
        self,
        code: ErrorCode,
        message: str,
        *,
        record_index: int | None = None,
        field_path: str | None = None,
        injection_point: str | None = None,
        detail: dict | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.record_index = record_index
        self.field_path = field_path
        self.injection_point = injection_point
        self.detail = detail or {}

    def to_dict(self) -> dict:
        payload = {
            "code": self.code.value,
            "message": self.message,
            "record_index": self.record_index,
            "field_path": self.field_path,
            "injection_point": self.injection_point,
        }
        payload.update(self.detail)
        return payload

    def __str__(self) -> str:  # pragma: no cover - 调试便利
        locs = []
        if self.record_index is not None:
            locs.append(f"record#{self.record_index}")
        if self.field_path:
            locs.append(f"path={self.field_path}")
        suffix = f" ({', '.join(locs)})" if locs else ""
        return f"[{self.code.value}] {self.message}{suffix}"
