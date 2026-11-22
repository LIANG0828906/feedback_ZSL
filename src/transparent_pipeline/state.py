"""Thread-safe block state machine and stable progress accounting."""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from enum import Enum
from typing import Any

from .errors import PipelineError, state_error


class BlockStatus(str, Enum):
    PENDING_INTERSECT = "pending_intersect"
    TRANSMISSION_ACCUMULATING = "transmission_accumulating"
    SCATTERING_ACCUMULATING = "scattering_accumulating"
    COMMITTED = "committed"
    FAILED = "failed"


STATUS_LABELS = {
    BlockStatus.PENDING_INTERSECT.value: "待求交",
    BlockStatus.TRANSMISSION_ACCUMULATING.value: "透射率累积中",
    BlockStatus.SCATTERING_ACCUMULATING.value: "散射积分中",
    BlockStatus.COMMITTED.value: "已提交",
    BlockStatus.FAILED.value: "失败",
}


LEGAL_TRANSITIONS: dict[BlockStatus, set[BlockStatus]] = {
    BlockStatus.PENDING_INTERSECT: {BlockStatus.TRANSMISSION_ACCUMULATING, BlockStatus.FAILED},
    BlockStatus.TRANSMISSION_ACCUMULATING: {BlockStatus.SCATTERING_ACCUMULATING, BlockStatus.FAILED},
    BlockStatus.SCATTERING_ACCUMULATING: {BlockStatus.COMMITTED, BlockStatus.FAILED},
    BlockStatus.COMMITTED: {BlockStatus.PENDING_INTERSECT},
    BlockStatus.FAILED: {BlockStatus.PENDING_INTERSECT},
}


@dataclass
class BlockState:
    block_id: int
    status: BlockStatus = BlockStatus.PENDING_INTERSECT
    media_versions: dict[str, int] = field(default_factory=dict)
    light_versions: dict[str, int] = field(default_factory=dict)
    buffer_checksum: str | None = None
    failure: str | None = None
    invalidation_count: int = 0
    revision: int = 0
    history: list[dict[str, Any]] = field(default_factory=list)


class BlockStateMachine:
    def __init__(self, block_ids: list[int]) -> None:
        self._lock = threading.RLock()
        self._states = {block_id: BlockState(block_id=block_id) for block_id in sorted(block_ids)}

    def _require(self, block_id: int) -> BlockState:
        try:
            return self._states[block_id]
        except KeyError as exc:
            raise PipelineError("E_STATE", f"blocks[{block_id}]", "unknown block id") from exc

    def status(self, block_id: int) -> str:
        with self._lock:
            return self._require(block_id).status.value

    def state(self, block_id: int) -> BlockState:
        with self._lock:
            state = self._require(block_id)
            return BlockState(
                block_id=state.block_id,
                status=state.status,
                media_versions=dict(state.media_versions),
                light_versions=dict(state.light_versions),
                buffer_checksum=state.buffer_checksum,
                failure=state.failure,
                invalidation_count=state.invalidation_count,
                revision=state.revision,
                history=list(state.history),
            )

    def transition(
        self,
        block_id: int,
        target: BlockStatus | str,
        *,
        reason: str | None = None,
        media_versions: dict[str, int] | None = None,
        light_versions: dict[str, int] | None = None,
        buffer_checksum: str | None = None,
        failure: str | None = None,
    ) -> None:
        if isinstance(target, str):
            try:
                target_status = BlockStatus(target)
            except ValueError as exc:
                raise PipelineError("E_STATE", f"blocks[{block_id}].status", f"unknown target status {target!r}") from exc
        else:
            target_status = target
        with self._lock:
            state = self._require(block_id)
            current = state.status
            if target_status not in LEGAL_TRANSITIONS[current]:
                raise state_error(block_id, current.value, target_status.value, reason)
            if current is BlockStatus.COMMITTED and target_status is BlockStatus.PENDING_INTERSECT:
                if reason != "invalidate":
                    raise state_error(block_id, current.value, target_status.value, reason)
            state.status = target_status
            state.revision += 1
            if target_status is BlockStatus.COMMITTED:
                state.media_versions = dict(media_versions or {})
                state.light_versions = dict(light_versions or {})
                state.buffer_checksum = buffer_checksum
                state.failure = None
            elif target_status is BlockStatus.FAILED:
                state.failure = failure or "unspecified block failure"
            elif target_status is BlockStatus.PENDING_INTERSECT:
                state.buffer_checksum = None
                state.failure = None
                if current is BlockStatus.COMMITTED:
                    state.invalidation_count += 1
            elif target_status is BlockStatus.TRANSMISSION_ACCUMULATING:
                state.failure = None
            state.history.append(
                {
                    "from": current.value,
                    "to": target_status.value,
                    "reason": reason,
                    "revision": state.revision,
                }
            )

    def retry(self, block_id: int) -> None:
        with self._lock:
            state = self._require(block_id)
            if state.status is not BlockStatus.FAILED:
                raise state_error(block_id, state.status.value, BlockStatus.PENDING_INTERSECT.value, "retry")
            self.transition(block_id, BlockStatus.PENDING_INTERSECT, reason="retry")

    def invalidate(self, block_id: int, reason: str = "invalidate") -> None:
        with self._lock:
            state = self._require(block_id)
            if state.status is BlockStatus.PENDING_INTERSECT:
                return
            if state.status is BlockStatus.COMMITTED:
                self.transition(block_id, BlockStatus.PENDING_INTERSECT, reason="invalidate")
                return
            if state.status is BlockStatus.FAILED:
                self.transition(block_id, BlockStatus.PENDING_INTERSECT, reason="retry")
                return
            raise state_error(block_id, state.status.value, BlockStatus.PENDING_INTERSECT.value, reason)

    def progress(self) -> dict[str, int]:
        with self._lock:
            committed = sum(1 for state in self._states.values() if state.status is BlockStatus.COMMITTED)
            failed = sum(1 for state in self._states.values() if state.status is BlockStatus.FAILED)
            return {"committed": committed, "total": len(self._states), "failed": failed}

    def statuses(self) -> dict[int, str]:
        with self._lock:
            return {block_id: state.status.value for block_id, state in self._states.items()}

    def labels(self) -> dict[int, str]:
        return {block_id: STATUS_LABELS[status] for block_id, status in self.statuses().items()}

    def committed_ids(self) -> list[int]:
        with self._lock:
            return [
                block_id
                for block_id, state in sorted(self._states.items())
                if state.status is BlockStatus.COMMITTED
            ]

    def failed_ids(self) -> list[int]:
        with self._lock:
            return [
                block_id
                for block_id, state in sorted(self._states.items())
                if state.status is BlockStatus.FAILED
            ]

    def committed_locks(self) -> dict[int, dict[str, Any]]:
        with self._lock:
            return {
                block_id: {
                    "media_versions": dict(state.media_versions),
                    "light_versions": dict(state.light_versions),
                    "buffer_checksum": state.buffer_checksum,
                }
                for block_id, state in sorted(self._states.items())
                if state.status is BlockStatus.COMMITTED
            }


__all__ = [
    "BlockState",
    "BlockStateMachine",
    "BlockStatus",
    "LEGAL_TRANSITIONS",
    "STATUS_LABELS",
]
