"""Thread-safe four-state frame lifecycle."""

from __future__ import annotations

import threading
from dataclasses import dataclass
from enum import Enum
from typing import Callable, Iterable

from .errors import ErrorCode, WorkbenchError
from .models import FrameParameters, FrameRenderStats


class FrameState(str, Enum):
    PENDING = "pending"
    ESTIMATED = "estimated"
    COMMITTED = "committed"
    FAILED = "failed"


LEGAL_TRANSITIONS: dict[FrameState, frozenset[FrameState]] = {
    FrameState.PENDING: frozenset({FrameState.ESTIMATED, FrameState.FAILED}),
    FrameState.ESTIMATED: frozenset(
        {FrameState.PENDING, FrameState.COMMITTED, FrameState.FAILED}
    ),
    FrameState.FAILED: frozenset({FrameState.PENDING}),
    FrameState.COMMITTED: frozenset(),
}


@dataclass
class FrameSlot:
    params: FrameParameters | None = None
    state: FrameState = FrameState.PENDING
    error: WorkbenchError | None = None
    render_stats: FrameRenderStats | None = None


class FrameStateMachine:
    def __init__(self, frame_count: int) -> None:
        if frame_count <= 0:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "frame_count must be positive",
                stage="state",
            )
        self._lock = threading.RLock()
        self._slots = [FrameSlot() for _ in range(frame_count)]

    def __len__(self) -> int:
        return len(self._slots)

    @property
    def lock(self) -> threading.RLock:
        return self._lock

    def _check_index(self, frame_index: int) -> None:
        if not 0 <= frame_index < len(self._slots):
            raise WorkbenchError(
                ErrorCode.E_STATE,
                "frame index is outside the state machine",
                stage="state",
                frame_index=frame_index,
                details={"frame_count": len(self._slots)},
            )

    def state(self, frame_index: int) -> FrameState:
        with self._lock:
            self._check_index(frame_index)
            return self._slots[frame_index].state

    def params(self, frame_index: int) -> FrameParameters:
        with self._lock:
            self._check_index(frame_index)
            slot = self._slots[frame_index]
            if slot.params is None:
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "frame has no reconstruction parameters",
                    stage="state",
                    frame_index=frame_index,
                )
            return slot.params.clone()

    def render_stats(self, frame_index: int) -> FrameRenderStats | None:
        with self._lock:
            self._check_index(frame_index)
            return self._slots[frame_index].render_stats

    def error(self, frame_index: int) -> WorkbenchError | None:
        with self._lock:
            self._check_index(frame_index)
            return self._slots[frame_index].error

    def states(self) -> tuple[FrameState, ...]:
        with self._lock:
            return tuple(slot.state for slot in self._slots)

    def transition(self, frame_index: int, target: FrameState) -> None:
        with self._lock:
            self._check_index(frame_index)
            slot = self._slots[frame_index]
            target = FrameState(target)
            if target not in LEGAL_TRANSITIONS[slot.state]:
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    f"illegal frame transition {slot.state.value} -> {target.value}",
                    stage="state",
                    frame_index=frame_index,
                    details={
                        "from": slot.state.value,
                        "to": target.value,
                        "allowed": sorted(
                            item.value for item in LEGAL_TRANSITIONS[slot.state]
                        ),
                    },
                )
            slot.state = target
            if target == FrameState.PENDING:
                slot.params = None
                slot.render_stats = None
                slot.error = None

    def estimate(
        self,
        frame_index: int,
        evaluator: Callable[[], FrameParameters],
    ) -> FrameParameters:
        with self._lock:
            self._check_index(frame_index)
            slot = self._slots[frame_index]
            if slot.state == FrameState.COMMITTED:
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "committed frame parameters are frozen",
                    stage="state",
                    frame_index=frame_index,
                    details={"state": slot.state.value},
                )
            if slot.state in (FrameState.ESTIMATED, FrameState.FAILED):
                self.transition(frame_index, FrameState.PENDING)
        try:
            params = evaluator()
        except WorkbenchError as exc:
            try:
                self.fail(frame_index, exc)
            except WorkbenchError:
                pass
            raise
        except Exception as exc:
            wrapped = WorkbenchError.wrap(
                ErrorCode.E_FRAME,
                "frame estimator failed unexpectedly",
                exc,
                stage="analysis",
                frame_index=frame_index,
            )
            try:
                self.fail(frame_index, wrapped)
            except WorkbenchError:
                pass
            raise wrapped from exc
        with self._lock:
            slot = self._slots[frame_index]
            if slot.state != FrameState.PENDING:
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "frame is not pending after estimation",
                    stage="state",
                    frame_index=frame_index,
                    details={"state": slot.state.value},
                )
            slot.params = params.clone()
            slot.error = None
            slot.state = FrameState.ESTIMATED
        return params.clone()

    def fail(self, frame_index: int, error: WorkbenchError) -> None:
        with self._lock:
            self._check_index(frame_index)
            slot = self._slots[frame_index]
            if slot.state not in (FrameState.PENDING, FrameState.ESTIMATED):
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    f"cannot fail a frame in state {slot.state.value}",
                    stage="state",
                    frame_index=frame_index,
                )
            slot.state = FrameState.FAILED
            slot.error = error

    def retry(self, frame_index: int) -> None:
        with self._lock:
            self._check_index(frame_index)
            slot = self._slots[frame_index]
            if slot.state != FrameState.FAILED:
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "only failed frames can be retried",
                    stage="state",
                    frame_index=frame_index,
                    details={"state": slot.state.value},
                )
            self.transition(frame_index, FrameState.PENDING)

    def commit(
        self,
        frame_index: int,
        persist: Callable[[FrameParameters, FrameRenderStats | None], None],
    ) -> None:
        with self._lock:
            self._check_index(frame_index)
            slot = self._slots[frame_index]
            if slot.state != FrameState.ESTIMATED or slot.params is None:
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "only estimated frames can be committed",
                    stage="state",
                    frame_index=frame_index,
                    details={"state": slot.state.value},
                )
            persist(slot.params.clone(), slot.render_stats)
            slot.state = FrameState.COMMITTED

    def attach_render_stats(
        self,
        frame_index: int,
        stats: FrameRenderStats,
    ) -> None:
        with self._lock:
            self._check_index(frame_index)
            slot = self._slots[frame_index]
            if slot.state not in (FrameState.ESTIMATED, FrameState.COMMITTED):
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "render statistics require an estimated or committed frame",
                    stage="state",
                    frame_index=frame_index,
                    details={"state": slot.state.value},
                )
            slot.render_stats = stats

    def load_committed(
        self,
        frame_index: int,
        params: FrameParameters,
        stats: FrameRenderStats | None = None,
    ) -> None:
        with self._lock:
            self._check_index(frame_index)
            slot = self._slots[frame_index]
            if slot.state == FrameState.COMMITTED:
                if slot.params is not None and slot.params.fingerprint() != params.fingerprint():
                    raise WorkbenchError(
                        ErrorCode.E_DIFF,
                        "checkpoint attempts to rewrite a committed frame",
                        stage="checkpoint-load",
                        frame_index=frame_index,
                    )
                return
            if slot.state != FrameState.PENDING:
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "checkpoint restoration requires pending frames",
                    stage="checkpoint-load",
                    frame_index=frame_index,
                    details={"state": slot.state.value},
                )
            slot.params = params.clone()
            slot.render_stats = stats
            slot.error = None
            slot.state = FrameState.COMMITTED

    def frozen_parameters(self) -> tuple[FrameParameters, ...]:
        with self._lock:
            result: list[FrameParameters] = []
            for index, slot in enumerate(self._slots):
                if slot.state != FrameState.COMMITTED or slot.params is None:
                    raise WorkbenchError(
                        ErrorCode.E_STATE,
                        "not all frames are committed",
                        stage="state",
                        frame_index=index,
                        details={"state": slot.state.value},
                    )
                result.append(slot.params.clone())
            return tuple(result)

    def counts(self) -> dict[str, int]:
        with self._lock:
            counts = {state.value: 0 for state in FrameState}
            for slot in self._slots:
                counts[slot.state.value] += 1
            return counts

