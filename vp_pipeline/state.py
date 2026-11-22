"""Four-state frame-window state machine."""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any

from .errors import E_STATE, PipelineError
from .frame import FrameResult


class WindowState(str, Enum):
    PENDING = "pending"
    COMPUTING = "computing"
    COMMITTED = "committed"
    FAILED = "failed"


STATE_LABELS_ZH = {
    WindowState.PENDING: "待合成",
    WindowState.COMPUTING: "合成中",
    WindowState.COMMITTED: "已提交",
    WindowState.FAILED: "失败",
}

LEGAL_TRANSITIONS = {
    (WindowState.PENDING, WindowState.COMPUTING),
    (WindowState.COMPUTING, WindowState.COMMITTED),
    (WindowState.COMPUTING, WindowState.FAILED),
    (WindowState.FAILED, WindowState.COMPUTING),
}


@dataclass
class WindowSlot:
    index: int
    start: int
    end_exclusive: int
    state: WindowState = WindowState.PENDING
    snapshot: dict[str, Any] | None = None
    frames: tuple[FrameResult, ...] = ()
    last_error: dict[str, Any] | None = None
    invalidation_reason: str | None = None
    generation: int = 0
    field_path_override: str | None = field(default=None, repr=False)

    @property
    def frame_indices(self) -> tuple[int, ...]:
        return tuple(range(self.start, self.end_exclusive))


def transition(
    slot: WindowSlot,
    target: WindowState,
    *,
    allow_invalidation: bool = False,
) -> bool:
    if slot.state == target:
        return False
    legal = (slot.state, target) in LEGAL_TRANSITIONS
    if (
        slot.state == WindowState.COMMITTED
        and target == WindowState.PENDING
        and allow_invalidation
    ):
        legal = True
    if (
        slot.state == WindowState.FAILED
        and target == WindowState.PENDING
        and allow_invalidation
    ):
        legal = True
    if not legal:
        raise PipelineError(
            E_STATE,
            f"Illegal window transition {slot.state.value} -> {target.value}",
            field_path=f"windows[{slot.index}].state",
            window_index=slot.index,
            current=slot.state.value,
            target=target.value,
        )
    slot.state = target
    if target == WindowState.COMPUTING:
        slot.generation += 1
        slot.last_error = None
    if target == WindowState.COMMITTED:
        slot.last_error = None
        slot.invalidation_reason = None
    if target == WindowState.PENDING:
        slot.last_error = None
    return True
