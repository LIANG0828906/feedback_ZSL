from __future__ import annotations

import threading
from enum import StrEnum

from .errors import E_STATE, SyncError


class SegmentState(StrEnum):
    PENDING = "PENDING"
    CORRECTED = "CORRECTED"
    COMMITTED = "COMMITTED"
    FAILED = "FAILED"


_LEGAL_TRANSITIONS: dict[SegmentState, frozenset[SegmentState]] = {
    SegmentState.PENDING: frozenset({SegmentState.CORRECTED, SegmentState.FAILED}),
    SegmentState.CORRECTED: frozenset({SegmentState.COMMITTED, SegmentState.FAILED}),
    SegmentState.COMMITTED: frozenset(),
    SegmentState.FAILED: frozenset({SegmentState.PENDING}),
}


class SegmentStateMachine:
    def __init__(self, channel_id: str, segment_index: int, state: SegmentState = SegmentState.PENDING) -> None:
        self.channel_id = channel_id
        self.segment_index = segment_index
        self._state = state
        self._lock = threading.RLock()

    @property
    def state(self) -> SegmentState:
        with self._lock:
            return self._state

    def transition(self, target: SegmentState) -> SegmentState:
        with self._lock:
            if target not in _LEGAL_TRANSITIONS[self._state]:
                raise SyncError(
                    E_STATE,
                    f"illegal state transition {self._state.value} -> {target.value}",
                    channel=self.channel_id,
                    segment=self.segment_index,
                    details={"from": self._state.value, "to": target.value},
                )
            previous = self._state
            self._state = target
            return previous

    def reset_failed_for_retry(self) -> None:
        with self._lock:
            if self._state == SegmentState.FAILED:
                self.transition(SegmentState.PENDING)


def legal_targets(state: SegmentState) -> frozenset[SegmentState]:
    return _LEGAL_TRANSITIONS[state]

