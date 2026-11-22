"""Four-state segment lifecycle with guarded transitions."""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from enum import Enum
from typing import Any

from .errors import WorkbenchError


class SegmentState(str, Enum):
    PENDING = "pending"
    PROCESSING = "processing"
    COMMITTED = "committed"
    FAILED = "failed"


LEGAL_TRANSITIONS = {
    SegmentState.PENDING: {SegmentState.PROCESSING},
    SegmentState.PROCESSING: {SegmentState.COMMITTED, SegmentState.FAILED},
    SegmentState.FAILED: {SegmentState.PROCESSING},
    SegmentState.COMMITTED: set(),
}


@dataclass
class SegmentRecord:
    index: int
    state: SegmentState = SegmentState.PENDING
    param_version: str | None = None
    params: dict[str, Any] | None = None
    content_hash: str | None = None
    file: str | None = None
    stats: dict[str, Any] = field(default_factory=dict)
    error: dict[str, Any] | None = None


class SegmentStateMachine:
    def __init__(self, records: dict[int, SegmentRecord]):
        self._records = records
        self._lock = threading.RLock()

    @property
    def records(self) -> dict[int, SegmentRecord]:
        return self._records

    def transition(self, index: int, target: SegmentState) -> SegmentRecord:
        with self._lock:
            record = self._records.get(index)
            if record is None:
                raise WorkbenchError("E_STATE", "Unknown segment", {"segment_index": index})
            source = record.state
            if target not in LEGAL_TRANSITIONS[source]:
                raise WorkbenchError(
                    "E_STATE",
                    "Illegal segment state transition",
                    {
                        "segment_index": index,
                        "from": source.value,
                        "to": target.value,
                        "allowed": sorted(item.value for item in LEGAL_TRANSITIONS[source]),
                    },
                )
            record.state = target
            if target == SegmentState.PROCESSING:
                record.error = None
            return record

    def set_loaded_state(self, index: int, state: SegmentState) -> None:
        with self._lock:
            self._records[index].state = state

    def counts(self) -> dict[str, int]:
        with self._lock:
            result = {state.value: 0 for state in SegmentState}
            for record in self._records.values():
                result[record.state.value] += 1
            return result

    def snapshot(self) -> list[dict[str, Any]]:
        with self._lock:
            return [
                {
                    "index": record.index,
                    "state": record.state.value,
                    "param_version": record.param_version,
                    "content_hash": record.content_hash,
                    "error": record.error,
                    "stats": record.stats,
                }
                for _, record in sorted(self._records.items())
            ]
