"""Per-frame five-state machine with a frozen commit boundary."""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any

from .errors import SciSeqError


class FrameStatus(StrEnum):
    PENDING = "pending"
    REGISTERED = "registered"
    DENOISING = "denoising"
    COMMITTED = "committed"
    FAILED = "failed"


LEGAL_TRANSITIONS: dict[FrameStatus, frozenset[FrameStatus]] = {
    FrameStatus.PENDING: frozenset((FrameStatus.REGISTERED, FrameStatus.FAILED)),
    FrameStatus.REGISTERED: frozenset((FrameStatus.DENOISING, FrameStatus.FAILED)),
    FrameStatus.DENOISING: frozenset((FrameStatus.COMMITTED, FrameStatus.FAILED)),
    FrameStatus.FAILED: frozenset((FrameStatus.PENDING,)),
    FrameStatus.COMMITTED: frozenset(),
}


@dataclass(frozen=True)
class CommittedPayload:
    offset: tuple[float, float]
    denoised_sha256: str
    content_sha256: str
    parameter_version: str
    algorithm_version: str
    extra: dict[str, Any] = field(default_factory=dict)


class FrameStateMachine:
    def __init__(self, frame_index: int, status: FrameStatus = FrameStatus.PENDING) -> None:
        self.frame_index = int(frame_index)
        self._status = status
        self._payload: CommittedPayload | None = None
        self._failure: str | None = None
        self._lock = threading.RLock()

    @property
    def status(self) -> FrameStatus:
        with self._lock:
            return self._status

    @property
    def payload(self) -> CommittedPayload | None:
        with self._lock:
            return self._payload

    @property
    def failure(self) -> str | None:
        with self._lock:
            return self._failure

    def transition(self, target: FrameStatus) -> FrameStatus:
        with self._lock:
            current = self._status
            if target not in LEGAL_TRANSITIONS[current]:
                raise SciSeqError(
                    "E_STATE",
                    "illegal frame state transition",
                    frame=self.frame_index,
                    field="state",
                    details={
                        "current": current.value,
                        "attempted": target.value,
                        "allowed": sorted(item.value for item in LEGAL_TRANSITIONS[current]),
                    },
                )
            self._status = target
            if target == FrameStatus.FAILED:
                self._failure = "frame processing failed"
            elif target == FrameStatus.PENDING:
                self._failure = None
            return self._status

    def fail(self, reason: str) -> FrameStatus:
        with self._lock:
            current = self._status
            if FrameStatus.FAILED not in LEGAL_TRANSITIONS[current]:
                raise SciSeqError(
                    "E_STATE",
                    "frame cannot enter failed state",
                    frame=self.frame_index,
                    field="state",
                    details={"current": current.value, "allowed": []},
                )
            self._status = FrameStatus.FAILED
            self._failure = str(reason)
            return self._status

    def retry(self) -> FrameStatus:
        with self._lock:
            if self._status != FrameStatus.FAILED:
                raise SciSeqError(
                    "E_STATE",
                    "only failed frames can be retried",
                    frame=self.frame_index,
                    field="state",
                    details={"current": self._status.value},
                )
            self._status = FrameStatus.PENDING
            self._failure = None
            return self._status

    def commit(self, payload: CommittedPayload) -> FrameStatus:
        with self._lock:
            current = self._status
            if current != FrameStatus.DENOISING:
                raise SciSeqError(
                    "E_STATE",
                    "commit is only legal from denoising state",
                    frame=self.frame_index,
                    field="state",
                    details={"current": current.value},
                )
            if self._payload is not None:
                raise SciSeqError(
                    "E_STATE",
                    "committed payload is frozen",
                    frame=self.frame_index,
                    field="offset/denoised",
                    details={"current": current.value},
                )
            self._payload = payload
            self._status = FrameStatus.COMMITTED
            return self._status

    def snapshot(self) -> dict[str, Any]:
        with self._lock:
            return {
                "frame": self.frame_index,
                "status": self._status.value,
                "failure": self._failure,
                "payload": None if self._payload is None else self._payload.__dict__,
            }


def progress(states: list[FrameStateMachine]) -> dict[str, int | float]:
    committed = sum(1 for state in states if state.status == FrameStatus.COMMITTED)
    total = len(states)
    return {
        "committed": committed,
        "total": total,
        "fraction": (committed / total) if total else 0.0,
    }

