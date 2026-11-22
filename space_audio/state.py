from __future__ import annotations

import threading
from dataclasses import dataclass
from enum import Enum
from typing import Any, Sequence

from .analysis import FrameResult
from .errors import ErrorCode, WorkbenchError


class FrameState(str, Enum):
    PENDING = "pending"
    ESTIMATED = "estimated"
    COMMITTED = "committed"
    FAILED = "failed"


LEGAL_TRANSITIONS: dict[FrameState, frozenset[FrameState]] = {
    FrameState.PENDING: frozenset({FrameState.ESTIMATED, FrameState.FAILED}),
    FrameState.ESTIMATED: frozenset(
        {FrameState.COMMITTED, FrameState.PENDING, FrameState.FAILED}
    ),
    FrameState.COMMITTED: frozenset(),
    FrameState.FAILED: frozenset({FrameState.PENDING, FrameState.ESTIMATED}),
}


@dataclass(frozen=True)
class StateTransition:
    frame_index: int
    previous: FrameState
    current: FrameState


class FrameStateMachine:
    """Thread-safe four-state frame machine with immutable committed results."""

    def __init__(
        self,
        frame_count: int,
        committed_results: Sequence[FrameResult] | None = None,
    ) -> None:
        if frame_count < 0:
            raise ValueError("frame_count must be non-negative")
        self._lock = threading.RLock()
        self._frame_count = int(frame_count)
        self._states = [FrameState.PENDING for _ in range(frame_count)]
        self._committed: list[FrameResult | None] = [None for _ in range(frame_count)]
        self._pending: dict[int, FrameResult] = {}
        self._errors: dict[int, WorkbenchError] = {}
        self._transitions: list[StateTransition] = []
        if committed_results:
            if len(committed_results) > frame_count:
                raise WorkbenchError(
                    ErrorCode.STATE,
                    "checkpoint has more committed frames than the current input",
                )
            expected = 0
            for result in committed_results:
                if result.frame_index != expected:
                    raise WorkbenchError(
                        ErrorCode.STATE,
                        "committed frame sequence is not contiguous",
                        frame_index=result.frame_index,
                        details={"expected": expected},
                    )
                self._states[expected] = FrameState.COMMITTED
                self._committed[expected] = result
                expected += 1
            self._transitions.extend(
                StateTransition(index, FrameState.PENDING, FrameState.COMMITTED)
                for index in range(expected)
            )

    @property
    def frame_count(self) -> int:
        with self._lock:
            return self._frame_count

    @property
    def committed_count(self) -> int:
        with self._lock:
            return sum(state == FrameState.COMMITTED for state in self._states)

    @property
    def progress(self) -> tuple[int, int]:
        with self._lock:
            return self.committed_count, self._frame_count

    @property
    def committed_results(self) -> tuple[FrameResult, ...]:
        with self._lock:
            rows = [
                result for result in self._committed if result is not None
            ]
            return tuple(rows)

    @property
    def transition_log(self) -> tuple[StateTransition, ...]:
        with self._lock:
            return tuple(self._transitions)

    def state(self, frame_index: int) -> FrameState:
        with self._lock:
            self._validate_index(frame_index)
            return self._states[frame_index]

    def states(self) -> tuple[FrameState, ...]:
        with self._lock:
            return tuple(self._states)

    def state_counts(self) -> dict[str, int]:
        with self._lock:
            counts = {state.value: 0 for state in FrameState}
            for state in self._states:
                counts[state.value] += 1
            return counts

    def pending_result(self, frame_index: int) -> FrameResult | None:
        with self._lock:
            self._validate_index(frame_index)
            return self._pending.get(frame_index)

    def error(self, frame_index: int) -> WorkbenchError | None:
        with self._lock:
            self._validate_index(frame_index)
            return self._errors.get(frame_index)

    def pending_x(self) -> dict[int, FrameResult]:
        with self._lock:
            return dict(self._pending)

    def transition(self, frame_index: int, target: FrameState) -> None:
        with self._lock:
            self._validate_index(frame_index)
            current = self._states[frame_index]
            if target not in LEGAL_TRANSITIONS[current]:
                raise WorkbenchError(
                    ErrorCode.STATE,
                    "illegal frame state transition",
                    frame_index=frame_index,
                    details={
                        "from": current.value,
                        "to": target.value,
                        "allowed": sorted(
                            state.value for state in LEGAL_TRANSITIONS[current]
                        ),
                    },
                )
            self._states[frame_index] = target
            self._transitions.append(StateTransition(frame_index, current, target))
            if target == FrameState.PENDING:
                self._pending.pop(frame_index, None)
                self._errors.pop(frame_index, None)

    def begin_estimate(self, frame_index: int) -> FrameResult | None:
        with self._lock:
            state = self.state(frame_index)
            if state == FrameState.ESTIMATED:
                return self._pending.get(frame_index)
            if state == FrameState.FAILED:
                self.transition(frame_index, FrameState.PENDING)
                state = FrameState.PENDING
            if state != FrameState.PENDING:
                raise WorkbenchError(
                    ErrorCode.STATE,
                    "frame is not available for estimation",
                    frame_index=frame_index,
                    details={"state": state.value},
                )
            return None

    def mark_estimated(self, frame_index: int, result: FrameResult) -> None:
        with self._lock:
            if result.frame_index != frame_index:
                raise WorkbenchError(
                    ErrorCode.STATE,
                    "estimated result belongs to a different frame",
                    frame_index=frame_index,
                    details={"result_frame": result.frame_index},
                )
            self.transition(frame_index, FrameState.ESTIMATED)
            self._pending[frame_index] = result
            self._errors.pop(frame_index, None)

    def mark_failed(self, frame_index: int, error: WorkbenchError) -> None:
        with self._lock:
            state = self.state(frame_index)
            if state == FrameState.COMMITTED:
                raise WorkbenchError(
                    ErrorCode.STATE,
                    "committed frame cannot be marked failed",
                    frame_index=frame_index,
                )
            if state != FrameState.FAILED:
                self.transition(frame_index, FrameState.FAILED)
            self._errors[frame_index] = error
            self._pending.pop(frame_index, None)

    def commit(self, frame_index: int) -> FrameResult:
        with self._lock:
            state = self.state(frame_index)
            if state != FrameState.ESTIMATED:
                raise WorkbenchError(
                    ErrorCode.STATE,
                    "frame can only be committed from the estimated state",
                    frame_index=frame_index,
                    details={"state": state.value},
                )
            result = self._pending.get(frame_index)
            if result is None:
                raise WorkbenchError(
                    ErrorCode.STATE,
                    "estimated frame has no frozen result",
                    frame_index=frame_index,
                )
            self.transition(frame_index, FrameState.COMMITTED)
            self._committed[frame_index] = result
            self._pending.pop(frame_index, None)
            return result

    def retry(self, frame_index: int) -> None:
        with self._lock:
            state = self.state(frame_index)
            if state != FrameState.FAILED:
                raise WorkbenchError(
                    ErrorCode.STATE,
                    "only failed frames can be retried",
                    frame_index=frame_index,
                    details={"state": state.value},
                )
            self.transition(frame_index, FrameState.PENDING)

    def invalidate_estimate(self, frame_index: int) -> None:
        with self._lock:
            state = self.state(frame_index)
            if state == FrameState.ESTIMATED:
                self.transition(frame_index, FrameState.PENDING)
            elif state == FrameState.FAILED:
                self.transition(frame_index, FrameState.PENDING)

    def reset_uncommitted(self) -> None:
        with self._lock:
            for index, state in enumerate(self._states):
                if state in (FrameState.ESTIMATED, FrameState.FAILED):
                    self.transition(index, FrameState.PENDING)

    def rebuild_for_count(
        self,
        frame_count: int,
        committed_results: Sequence[FrameResult],
    ) -> "FrameStateMachine":
        with self._lock:
            return FrameStateMachine(frame_count, committed_results)

    def _validate_index(self, frame_index: int) -> None:
        if not isinstance(frame_index, int) or not 0 <= frame_index < self._frame_count:
            raise WorkbenchError(
                ErrorCode.STATE,
                "frame index is outside the state machine",
                frame_index=frame_index,
                details={"frame_count": self._frame_count},
            )
