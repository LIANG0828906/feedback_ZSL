"""节目段状态机。

合法迁移：

``PENDING -> PROCESSING -> COMMITTED``
                 ``-> FAILED``
``FAILED -> PROCESSING``（重试）
``PROCESSING -> PENDING``（依赖未满足时回退，仅限调度器内部使用）

任何非法迁移抛出 E_STATE 且原状态保持不变。
"""
from __future__ import annotations

from enum import Enum

from .errors import StateError


class SegmentState(str, Enum):
    PENDING = "pending"
    PROCESSING = "processing"
    COMMITTED = "committed"
    FAILED = "failed"


_LEGAL_TRANSITIONS: dict[SegmentState, frozenset[SegmentState]] = {
    SegmentState.PENDING: frozenset({SegmentState.PROCESSING}),
    SegmentState.PROCESSING: frozenset({
        SegmentState.COMMITTED, SegmentState.FAILED, SegmentState.PENDING}),
    SegmentState.FAILED: frozenset({SegmentState.PROCESSING}),
    SegmentState.COMMITTED: frozenset(),
}


class SegmentStateMachine:
    def __init__(self, segment_index: int,
                 state: SegmentState = SegmentState.PENDING):
        self.segment_index = segment_index
        self._state = state
        self.fail_reason: str | None = None

    @property
    def state(self) -> SegmentState:
        return self._state

    def can_transition(self, target: SegmentState) -> bool:
        return target in _LEGAL_TRANSITIONS[self._state]

    def transition(self, target: SegmentState,
                   reason: str | None = None) -> SegmentState:
        if not self.can_transition(target):
            raise StateError(
                f"段 {self.segment_index} 非法状态迁移 "
                f"{self._state.value} -> {target.value}",
                field=f"segment[{self.segment_index}].state")
        self._state = target
        if target is SegmentState.FAILED:
            self.fail_reason = reason
        return self._state

    def reset_fail_reason(self) -> None:
        self.fail_reason = None
