from __future__ import annotations

import threading
from dataclasses import dataclass, field
from enum import Enum
from typing import Any

from .errors import WorkbenchError


class SegmentState(str, Enum):
    PENDING_FLATTEN = "PENDING_FLATTEN"
    FLATTENED = "FLATTENED"
    FILLED = "FILLED"
    COMMITTED = "COMMITTED"
    FAILED = "FAILED"


STATE_LABELS = {
    SegmentState.PENDING_FLATTEN: "待扁平化",
    SegmentState.FLATTENED: "已扁平化",
    SegmentState.FILLED: "已填充",
    SegmentState.COMMITTED: "已提交",
    SegmentState.FAILED: "失败",
}

LEGAL_TRANSITIONS = {
    SegmentState.PENDING_FLATTEN: {
        SegmentState.FLATTENED,
        SegmentState.FAILED,
    },
    SegmentState.FLATTENED: {
        SegmentState.FILLED,
        SegmentState.FAILED,
    },
    SegmentState.FILLED: {
        SegmentState.COMMITTED,
        SegmentState.FAILED,
    },
    SegmentState.COMMITTED: set(),
    SegmentState.FAILED: {SegmentState.PENDING_FLATTEN},
}


@dataclass
class SegmentRecord:
    index: int
    state: SegmentState = SegmentState.PENDING_FLATTEN
    error_code: str | None = None
    error_message: str | None = None
    attempts: int = 0
    flat_count: int = 0
    cache_hash: str | None = None

    @property
    def committed(self) -> bool:
        return self.state is SegmentState.COMMITTED

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "state": self.state.value,
            "state_label": STATE_LABELS[self.state],
            "committed": self.committed,
            "error_code": self.error_code,
            "error_message": self.error_message,
            "attempts": self.attempts,
            "flat_count": self.flat_count,
            "cache_hash": self.cache_hash,
        }

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> SegmentRecord:
        return cls(
            index=int(raw["index"]),
            state=SegmentState(raw["state"]),
            error_code=raw.get("error_code"),
            error_message=raw.get("error_message"),
            attempts=int(raw.get("attempts", 0)),
            flat_count=int(raw.get("flat_count", 0)),
            cache_hash=raw.get("cache_hash"),
        )


@dataclass
class StateSnapshot:
    records: list[SegmentRecord]
    commit_sequence: list[int] = field(default_factory=list)

    @property
    def committed_count(self) -> int:
        return sum(record.committed for record in self.records)

    @property
    def total(self) -> int:
        return len(self.records)

    @property
    def progress_text(self) -> str:
        return f"{self.committed_count}/{self.total}"

    def to_dict(self) -> dict[str, Any]:
        return {
            "committed_count": self.committed_count,
            "total": self.total,
            "progress": self.progress_text,
            "commit_sequence": list(self.commit_sequence),
            "segments": [record.to_dict() for record in self.records],
        }


class StateMachine:
    """Thread-safe strict five-state machine for per-contour segments."""

    def __init__(self, total_segments: int) -> None:
        if total_segments <= 0:
            raise WorkbenchError("E_STATE", "state machine needs at least one segment")
        self._lock = threading.RLock()
        self._records = [SegmentRecord(index) for index in range(total_segments)]
        self._commit_sequence: list[int] = []

    def _record(self, index: int) -> SegmentRecord:
        if index < 0 or index >= len(self._records):
            raise WorkbenchError(
                "E_STATE",
                "segment index is outside state machine range",
                segment_index=index,
                total_segments=len(self._records),
            )
        return self._records[index]

    def transition(
        self, index: int, target: SegmentState | str
    ) -> SegmentRecord:
        target_state = SegmentState(target)
        with self._lock:
            record = self._record(index)
            current = record.state
            if target_state not in LEGAL_TRANSITIONS[current]:
                raise WorkbenchError(
                    "E_STATE",
                    f"illegal transition {current.value} -> {target_state.value}",
                    segment_index=index,
                    current_state=current.value,
                    target_state=target_state.value,
                )
            record.state = target_state
            record.error_code = None
            record.error_message = None
            if target_state is SegmentState.COMMITTED:
                if index in self._commit_sequence:
                    raise WorkbenchError(
                        "E_STATE",
                        "segment was already committed",
                        segment_index=index,
                    )
                self._commit_sequence.append(index)
            return record

    def fail(self, index: int, code: str, message: str) -> SegmentRecord:
        with self._lock:
            record = self._record(index)
            if record.state is SegmentState.COMMITTED:
                raise WorkbenchError(
                    "E_STATE",
                    "committed segment cannot be changed to failed",
                    segment_index=index,
                    current_state=record.state.value,
                )
            record.attempts += 1
            if record.state is not SegmentState.FAILED:
                record.state = SegmentState.FAILED
            record.error_code = code
            record.error_message = message
            return record

    def retry(self, index: int) -> SegmentRecord:
        with self._lock:
            record = self._record(index)
            if record.state is not SegmentState.FAILED:
                raise WorkbenchError(
                    "E_STATE",
                    "only failed segments can be retried",
                    segment_index=index,
                    current_state=record.state.value,
                )
            record.state = SegmentState.PENDING_FLATTEN
            record.error_code = None
            record.error_message = None
            return record

    def set_metadata(
        self, index: int, *, flat_count: int | None = None, cache_hash: str | None = None
    ) -> None:
        with self._lock:
            record = self._record(index)
            if flat_count is not None:
                record.flat_count = int(flat_count)
            if cache_hash is not None:
                record.cache_hash = cache_hash

    def restore_committed(self, committed_count: int) -> None:
        with self._lock:
            if not (0 <= committed_count <= len(self._records)):
                raise WorkbenchError(
                    "E_STATE", "invalid committed segment count", value=committed_count
                )
            for index, record in enumerate(self._records):
                if index < committed_count:
                    record.state = SegmentState.COMMITTED
                    record.error_code = None
                    record.error_message = None
                else:
                    record.state = SegmentState.PENDING_FLATTEN
                    record.error_code = None
                    record.error_message = None
            self._commit_sequence = list(range(committed_count))

    def restore_snapshot(self, raw: dict[str, Any]) -> None:
        with self._lock:
            records = [SegmentRecord.from_dict(item) for item in raw.get("segments", [])]
            if len(records) != len(self._records):
                raise WorkbenchError(
                    "E_STATE",
                    "persisted state count does not match segment count",
                    persisted=len(records),
                    expected=len(self._records),
                )
            self._records = records
            self._commit_sequence = [
                int(index) for index in raw.get("commit_sequence", [])
            ]

    def snapshot(self) -> StateSnapshot:
        with self._lock:
            return StateSnapshot(
                records=self.records(),
                commit_sequence=list(self._commit_sequence),
            )

    def to_dict(self) -> dict[str, Any]:
        with self._lock:
            committed = sum(record.committed for record in self._records)
            return {
                "committed_count": committed,
                "total": len(self._records),
                "progress": f"{committed}/{len(self._records)}",
                "commit_sequence": list(self._commit_sequence),
                "segments": [record.to_dict() for record in self._records],
            }

    def records(self) -> list[SegmentRecord]:
        with self._lock:
            return [
                SegmentRecord(
                    index=record.index,
                    state=record.state,
                    error_code=record.error_code,
                    error_message=record.error_message,
                    attempts=record.attempts,
                    flat_count=record.flat_count,
                    cache_hash=record.cache_hash,
                )
                for record in self._records
            ]

    @property
    def committed_count(self) -> int:
        with self._lock:
            return sum(record.committed for record in self._records)

    @property
    def total(self) -> int:
        return len(self._records)

