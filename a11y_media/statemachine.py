"""分段状态机与提交边界。

每段状态: PENDING(待处理) -> PROCESSING(处理中) -> COMMITTED(已提交) / FAILED(失败)
合法迁移:
  PENDING    -> PROCESSING
  PROCESSING -> COMMITTED | FAILED
  FAILED     -> PROCESSING   (重试)
其余迁移一律抛 E_STATE 且原状态保持不变。
已提交分段的结果被冻结, 参数调整只作用于未提交分段。
"""

from __future__ import annotations

import threading

from .errors import state_error

PENDING = "PENDING"
PROCESSING = "PROCESSING"
COMMITTED = "COMMITTED"
FAILED = "FAILED"

_LEGAL = {
    PENDING: {PROCESSING},
    PROCESSING: {COMMITTED, FAILED},
    COMMITTED: set(),
    FAILED: {PROCESSING},
}


class SegmentTracker:
    """线程安全的分段状态跟踪器。"""

    def __init__(self, num_chunks: int):
        self._states = [PENDING] * num_chunks
        self._lock = threading.Lock()

    def transition(self, chunk: int, to: str) -> None:
        with self._lock:
            cur = self._states[chunk]
            if to not in _LEGAL[cur]:
                raise state_error(
                    "非法状态迁移", chunk=chunk, current=cur, target=to
                )
            self._states[chunk] = to

    def state(self, chunk: int) -> str:
        with self._lock:
            return self._states[chunk]

    def states(self) -> list:
        with self._lock:
            return list(self._states)

    def progress(self) -> tuple:
        """(已提交分段数, 总分段数)。"""
        with self._lock:
            return sum(1 for s in self._states if s == COMMITTED), len(self._states)

    def mark_committed_from_snapshot(self, chunk: int) -> None:
        """从持久化快照恢复已提交状态(仅加载期使用)。"""
        with self._lock:
            self._states[chunk] = COMMITTED
