"""统一异常类型与错误码。

错误码约定（与验收用例对齐）：

* ``E_PARA``   参数非法（采样率不支持、帧参数/压缩限幅参数越界等）
* ``E_STATE``  节目段状态机非法迁移
* ``E_DECODE`` 样本损坏或无法解码
* ``E_DIFF``   并行 / 串行 / 恢复路径结果不一致
* ``E_CANCEL`` 用户取消
"""
from __future__ import annotations


class BenchError(Exception):
    """所有工作台业务异常的基类，携带稳定错误码与出错字段。"""

    code = "E_UNKNOWN"

    def __init__(self, message: str, *, field: str | None = None,
                 detail: dict | None = None):
        super().__init__(message)
        self.field = field
        self.detail = detail or {}

    def __str__(self) -> str:  # pragma: no cover - 仅调试展示
        prefix = f"[{self.code}]"
        if self.field:
            prefix += f" field={self.field}"
        return f"{prefix} {super().__str__()}"


class ParameterError(BenchError):
    code = "E_PARA"


class StateError(BenchError):
    code = "E_STATE"


class DecodeError(BenchError):
    code = "E_DECODE"


class DiffError(BenchError):
    code = "E_DIFF"

    def __init__(self, message: str, *, segment_index: int,
                 sample_index: int, expected: float, actual: float):
        super().__init__(message, detail={
            "segment_index": segment_index,
            "sample_index": sample_index,
            "expected": expected,
            "actual": actual,
        })
        self.segment_index = segment_index
        self.sample_index = sample_index
        self.expected = expected
        self.actual = actual


class CancelledError(BenchError):
    code = "E_CANCEL"
