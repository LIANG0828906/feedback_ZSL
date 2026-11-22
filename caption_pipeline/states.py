"""台词级状态机与提交边界。"""
from __future__ import annotations

import enum

from .errors import E_STATE, PipelineError


class LineState(str, enum.Enum):
    PENDING = "PENDING"          # 待排布
    LAYING_OUT = "LAYING_OUT"    # 排布中
    COMMITTED = "COMMITTED"      # 已提交
    FAILED = "FAILED"            # 失败


_ALLOWED = {
    LineState.PENDING: {LineState.LAYING_OUT},
    LineState.LAYING_OUT: {LineState.COMMITTED, LineState.FAILED},
    LineState.COMMITTED: set(),
    LineState.FAILED: {LineState.PENDING},  # 允许重试
}


class LineStateMachine:
    """单句台词的状态机；非法迁移抛 E_STATE 且保持原状态。"""

    def __init__(self):
        self.state = LineState.PENDING
        self.deps_met = False  # 前置依赖（包络检测与窗口推导）是否完成

    def transition(self, target: LineState):
        if target not in _ALLOWED[self.state]:
            raise PipelineError(
                E_STATE, f"非法状态迁移 {self.state.value} -> {target.value}",
                details={"state": self.state.value, "target": target.value})
        if target == LineState.LAYING_OUT and not self.deps_met:
            raise PipelineError(
                E_STATE, "前置依赖未完成，不得进入排布中",
                details={"state": self.state.value, "target": target.value})
        self.state = target
