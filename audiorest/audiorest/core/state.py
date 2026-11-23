"""窗口状态机：待处理/处理中/已提交/失败四类状态与合法迁移。"""

from __future__ import annotations

from enum import Enum

from ..errors import raise_state


class WindowState(str, Enum):
    PENDING = "pending"
    PROCESSING = "processing"
    COMMITTED = "committed"
    FAILED = "failed"


# 只允许沿这些边迁移；任何其他组合都是非法迁移（E_STATE）
ALLOWED_TRANSITIONS: dict[WindowState, frozenset[WindowState]] = {
    WindowState.PENDING: frozenset({WindowState.PROCESSING, WindowState.FAILED}),
    WindowState.PROCESSING: frozenset({WindowState.COMMITTED, WindowState.FAILED, WindowState.PENDING}),
    WindowState.COMMITTED: frozenset(),  # 提交后冻结，不可再迁移
    WindowState.FAILED: frozenset({WindowState.PENDING}),  # 允许重试回到待处理
}


class WindowStateMachine:
    def __init__(self, window_count: int):
        self._states = [WindowState.PENDING] * window_count

    @property
    def states(self) -> list[WindowState]:
        return list(self._states)

    def state(self, index: int) -> WindowState:
        return self._states[index]

    def transition(self, index: int, target: WindowState) -> None:
        current = self._states[index]
        if target not in ALLOWED_TRANSITIONS[current]:
            raise_state(
                f"窗口 {index} 非法状态迁移: {current.value} -> {target.value}",
                f"windows[{index}].state",
                current=current.value,
                target=target.value,
            )
        self._states[index] = target

    @property
    def committed_count(self) -> int:
        return sum(1 for state in self._states if state is WindowState.COMMITTED)

    @property
    def progress(self) -> tuple[int, int]:
        return self.committed_count, len(self._states)
