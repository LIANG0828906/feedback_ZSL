"""Per-frame state machine with frozen committed records."""

from __future__ import annotations

import threading
from enum import Enum
from typing import Any

from .codes import E_STATE
from .errors import error
from .models import FrameEstimate


class FrameState(str, Enum):
    PENDING = "pending"
    ESTIMATED = "estimated"
    COMMITTED = "committed"
    FAILED = "failed"

    @property
    def label(self) -> str:
        return {
            FrameState.PENDING: "待估计",
            FrameState.ESTIMATED: "已估计",
            FrameState.COMMITTED: "已提交",
            FrameState.FAILED: "失败",
        }[self]


LEGAL_TRANSITIONS: dict[FrameState, frozenset[FrameState]] = {
    FrameState.PENDING: frozenset({FrameState.ESTIMATED, FrameState.FAILED}),
    FrameState.ESTIMATED: frozenset(
        {FrameState.COMMITTED, FrameState.FAILED, FrameState.PENDING}
    ),
    FrameState.COMMITTED: frozenset(),
    FrameState.FAILED: frozenset({FrameState.PENDING, FrameState.ESTIMATED}),
}


class FrameSlot:
    def __init__(self, index: int) -> None:
        self.index = index
        self.state = FrameState.PENDING
        self.estimate: FrameEstimate | None = None
        self.failure: dict[str, Any] | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame_index": self.index,
            "state": self.state.value,
            "estimate": self.estimate.to_dict() if self.estimate else None,
            "failure": self.failure,
        }


class FrameStateMachine:
    def __init__(self, total_frames: int) -> None:
        if total_frames < 0:
            raise error(
                "E_PARA",
                "total_frames cannot be negative",
                stage="state.init",
                field="total_frames",
            )
        self.total_frames = int(total_frames)
        self._slots = [FrameSlot(index) for index in range(self.total_frames)]
        self._lock = threading.RLock()

    def _slot(self, frame_index: int) -> FrameSlot:
        if frame_index < 0 or frame_index >= self.total_frames:
            raise error(
                E_STATE,
                "frame index is outside the state machine",
                stage="state.lookup",
                frame_index=frame_index,
                context={"total_frames": self.total_frames},
            )
        return self._slots[frame_index]

    def state(self, frame_index: int) -> FrameState:
        with self._lock:
            return self._slot(frame_index).state

    def estimate(self, frame_index: int) -> FrameEstimate | None:
        with self._lock:
            return self._slot(frame_index).estimate

    def transition(
        self,
        frame_index: int,
        requested: FrameState,
        *,
        estimate: FrameEstimate | None = None,
        failure: dict[str, Any] | None = None,
    ) -> FrameState:
        with self._lock:
            slot = self._slot(frame_index)
            current = slot.state
            if requested not in LEGAL_TRANSITIONS[current]:
                raise error(
                    E_STATE,
                    f"illegal frame transition {current.value} -> {requested.value}",
                    stage="state.transition",
                    frame_index=frame_index,
                    context={
                        "current": current.value,
                        "requested": requested.value,
                        "legal": sorted(item.value for item in LEGAL_TRANSITIONS[current]),
                    },
                )
            if current is FrameState.COMMITTED:
                raise error(
                    E_STATE,
                    "committed frame parameters are frozen",
                    stage="state.freeze",
                    frame_index=frame_index,
                )
            if requested is FrameState.ESTIMATED:
                if estimate is None:
                    raise error(
                        E_STATE,
                        "estimated transition requires an estimate payload",
                        stage="state.transition",
                        frame_index=frame_index,
                    )
                slot.estimate = estimate
                slot.failure = None
            elif requested is FrameState.FAILED:
                if current is FrameState.COMMITTED:
                    raise error(
                        E_STATE,
                        "committed frame cannot be marked failed",
                        stage="state.transition",
                        frame_index=frame_index,
                    )
                slot.failure = failure or {}
            elif requested is FrameState.PENDING:
                slot.estimate = None
                slot.failure = None
            slot.state = requested
            return slot.state

    def mark_estimated(self, frame_index: int, estimate: FrameEstimate) -> FrameState:
        return self.transition(frame_index, FrameState.ESTIMATED, estimate=estimate)

    def mark_committed(self, frame_index: int) -> FrameState:
        return self.transition(frame_index, FrameState.COMMITTED)

    def mark_failed(self, frame_index: int, failure: dict[str, Any]) -> FrameState:
        return self.transition(frame_index, FrameState.FAILED, failure=failure)

    def retry(self, frame_index: int) -> FrameState:
        return self.transition(frame_index, FrameState.PENDING)

    def revise_uncommitted(self, frame_index: int) -> FrameState:
        with self._lock:
            slot = self._slot(frame_index)
            if slot.state is FrameState.COMMITTED:
                raise error(
                    E_STATE,
                    "committed frame parameters cannot be revised",
                    stage="state.revise",
                    frame_index=frame_index,
                )
            if slot.state is FrameState.ESTIMATED:
                return self.transition(frame_index, FrameState.PENDING)
            return slot.state

    def progress(self) -> dict[str, Any]:
        with self._lock:
            counts = {state: 0 for state in FrameState}
            for slot in self._slots:
                counts[slot.state] += 1
            committed = counts[FrameState.COMMITTED]
            return {
                "committed_frames": committed,
                "total_frames": self.total_frames,
                "ratio": 0.0 if self.total_frames == 0 else committed / self.total_frames,
                "states": {state.value: count for state, count in counts.items()},
            }

    def committed_indices(self) -> list[int]:
        with self._lock:
            return [
                slot.index
                for slot in self._slots
                if slot.state is FrameState.COMMITTED
            ]

    def frozen_estimates(self) -> list[FrameEstimate]:
        with self._lock:
            return [
                slot.estimate
                for slot in self._slots
                if slot.state is FrameState.COMMITTED and slot.estimate is not None
            ]

    def snapshot(self) -> list[dict[str, Any]]:
        with self._lock:
            return [slot.to_dict() for slot in self._slots]
