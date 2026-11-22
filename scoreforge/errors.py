"""统一错误码体系.

所有领域错误均携带机器可读的 ``code`` 与对象路径 ``path``，
用于界面展示、日志与自动化比对。
"""

from __future__ import annotations


class ScoreError(Exception):
    """所有领域错误的基类."""

    code = "E_UNKNOWN"

    def __init__(self, message: str, path: str | None = None, details: dict | None = None):
        super().__init__(message)
        self.message = message
        self.path = path
        self.details = details or {}

    def __str__(self) -> str:  # pragma: no cover - 展示用途
        loc = f" at {self.path}" if self.path else ""
        return f"[{self.code}]{loc} {self.message}"


class SchemaError(ScoreError):
    """字段缺失或类型非法."""

    code = "E_SCHEMA"


class RefError(ScoreError):
    """引用悬空：声部/小节/锚点音符不存在，或拍位越过所在小节."""

    code = "E_REF"


class DurationError(ScoreError):
    """音符时值非法（非正整数，或累计时值越过小节边界）."""

    code = "E_DUR"


class DanglingEventError(ScoreError):
    """删除/替换音符后残留悬空表达事件."""

    code = "E_DANGLING"

    def __init__(self, message: str, path: str | None = None, event_id: str | None = None):
        super().__init__(message, path=path, details={"event_id": event_id})
        self.event_id = event_id


class BatchError(ScoreError):
    """非法提交批次：编辑已冻结小节、空批次、重复提交或提交边界冲突."""

    code = "E_BATCH"


class CheckpointError(ScoreError):
    """检查点内容哈希或参数版本不匹配，快照整体作废."""

    code = "E_CHECKPOINT"


class DivergenceError(ScoreError):
    """实时编辑 / 断点恢复 / 全量重算三条路径结果不一致."""

    code = "E_DIVERGE"

    def __init__(self, path: str, field: str, left, right):
        super().__init__(
            f"divergence at {path}: field '{field}' left={left!r} right={right!r}",
            path=path,
            details={"field": field, "left": left, "right": right},
        )
        self.field = field
        self.left = left
        self.right = right


class PrecisionError(ScoreError):
    """演奏表达精度超出真值声明容差."""

    code = "E_PREC"

    def __init__(
        self,
        message: str,
        path: str | None = None,
        measure: int | None = None,
        beat_ticks: int | None = None,
        expected=None,
        actual=None,
        tolerance=None,
    ):
        super().__init__(
            message,
            path=path,
            details={
                "measure": measure,
                "beat_ticks": beat_ticks,
                "expected": expected,
                "actual": actual,
                "tolerance": tolerance,
            },
        )
        self.measure = measure
        self.beat_ticks = beat_ticks
