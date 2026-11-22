"""Fixed sample-boundary segmentation, state machine and ordered stitching."""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from enum import Enum
from typing import Callable, Iterable

import numpy as np

from .errors import param_error, state_error


@dataclass(frozen=True)
class SegmentSpec:
    index: int
    start_sample: int
    end_sample: int

    @property
    def sample_count(self) -> int:
        return self.end_sample - self.start_sample

    def to_dict(self) -> dict[str, int]:
        return {
            "index": self.index,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "sample_count": self.sample_count,
        }


def split_segments(total_samples: int, segment_samples: int) -> list[SegmentSpec]:
    if total_samples <= 0:
        raise param_error("total sample count must be positive")
    if segment_samples <= 0:
        raise param_error("segment_samples must be positive")
    segments = []
    start = 0
    index = 0
    while start < total_samples:
        end = min(total_samples, start + segment_samples)
        segments.append(SegmentSpec(index=index, start_sample=start, end_sample=end))
        start = end
        index += 1
    return segments


def stitch_segment_outputs(
    outputs: dict[int, np.ndarray],
    specs: Iterable[SegmentSpec],
    total_samples: int,
    channels: int,
) -> np.ndarray:
    """Assemble outputs only when every exact boundary is present exactly once."""

    ordered = list(specs)
    expected = {spec.index for spec in ordered}
    actual = set(outputs)
    if actual != expected:
        raise state_error(
            "cannot export an incomplete segment timeline",
            missing=sorted(expected - actual),
            unexpected=sorted(actual - expected),
        )
    result = np.empty((total_samples, channels), dtype=np.float64)
    cursor = 0
    for spec in ordered:
        if spec.start_sample != cursor:
            raise state_error(
                "segment timeline has a gap or overlap",
                segment_index=spec.index,
                expected_start=cursor,
                actual_start=spec.start_sample,
            )
        chunk = np.asarray(outputs[spec.index], dtype=np.float64)
        if chunk.ndim == 1:
            chunk = chunk[:, None]
        if chunk.shape != (spec.sample_count, channels):
            raise state_error(
                "segment output shape does not match its committed boundary",
                segment_index=spec.index,
                expected=[spec.sample_count, channels],
                actual=list(chunk.shape),
            )
        result[spec.start_sample : spec.end_sample] = chunk
        cursor = spec.end_sample
    if cursor != total_samples:
        raise state_error(
            "segment timeline does not end at the source sample count",
            expected_end=total_samples,
            actual_end=cursor,
        )
    return result


class SegmentStatus(str, Enum):
    PENDING = "PENDING"
    PROCESSING = "PROCESSING"
    COMMITTED = "COMMITTED"
    FAILED = "FAILED"


_LEGAL_TRANSITIONS: dict[SegmentStatus, frozenset[SegmentStatus]] = {
    SegmentStatus.PENDING: frozenset({SegmentStatus.PROCESSING}),
    SegmentStatus.PROCESSING: frozenset(
        {SegmentStatus.COMMITTED, SegmentStatus.FAILED}
    ),
    SegmentStatus.COMMITTED: frozenset(),
    SegmentStatus.FAILED: frozenset({SegmentStatus.PROCESSING}),
}


@dataclass
class SegmentRecord:
    spec: SegmentSpec
    status: SegmentStatus = SegmentStatus.PENDING
    attempts: int = 0
    config_fingerprint: str | None = None
    output_sha256: str | None = None
    error: dict[str, object] | None = None

    def to_dict(self) -> dict[str, object]:
        return {
            **self.spec.to_dict(),
            "status": self.status.value,
            "attempts": self.attempts,
            "config_fingerprint": self.config_fingerprint,
            "output_sha256": self.output_sha256,
            "error": self.error,
        }


class SegmentStateMachine:
    """Thread-safe state holder. Illegal moves raise E_STATE and change nothing."""

    def __init__(self, specs: Iterable[SegmentSpec]) -> None:
        spec_list = list(specs)
        self._lock = threading.RLock()
        self._records = {spec.index: SegmentRecord(spec=spec) for spec in spec_list}
        if len(self._records) != len(spec_list):
            raise param_error("segment indexes must be unique")

    def _record_locked(self, index: int) -> SegmentRecord:
        try:
            return self._records[index]
        except KeyError as exc:
            raise state_error("unknown segment index", segment_index=index) from exc

    def transition(self, index: int, target: SegmentStatus) -> SegmentRecord:
        with self._lock:
            record = self._record_locked(index)
            current = record.status
            if target not in _LEGAL_TRANSITIONS[current]:
                raise state_error(
                    "illegal segment state transition",
                    segment_index=index,
                    current=current.value,
                    target=target.value,
                )
            record.status = target
            if target == SegmentStatus.PROCESSING:
                record.attempts += 1
                record.error = None
            return record

    def restore_committed(
        self,
        index: int,
        *,
        config_fingerprint: str,
        output_sha256: str,
    ) -> SegmentRecord:
        """Initialize a record from a validated durable commit.

        This is snapshot restoration, not a live PENDING->COMMITTED transition.
        """

        with self._lock:
            record = self._record_locked(index)
            if record.status != SegmentStatus.PENDING:
                raise state_error(
                    "only pending records can be restored from a snapshot",
                    segment_index=index,
                    current=record.status.value,
                )
            record.status = SegmentStatus.COMMITTED
            record.attempts = 1
            record.config_fingerprint = config_fingerprint
            record.output_sha256 = output_sha256
            record.error = None
            return record

    def mark_committed(
        self,
        index: int,
        *,
        config_fingerprint: str,
        output_sha256: str,
    ) -> SegmentRecord:
        with self._lock:
            record = self.transition(index, SegmentStatus.COMMITTED)
            record.config_fingerprint = config_fingerprint
            record.output_sha256 = output_sha256
            record.error = None
            return record

    def mark_failed(
        self,
        index: int,
        error: dict[str, object],
    ) -> SegmentRecord:
        with self._lock:
            record = self.transition(index, SegmentStatus.FAILED)
            record.error = dict(error)
            return record

    def retry(self, index: int) -> SegmentRecord:
        with self._lock:
            return self.transition(index, SegmentStatus.PROCESSING)

    def get(self, index: int) -> SegmentRecord:
        with self._lock:
            return self._record_locked(index)

    def snapshot(self) -> dict[int, SegmentRecord]:
        with self._lock:
            return {
                index: SegmentRecord(
                    spec=record.spec,
                    status=record.status,
                    attempts=record.attempts,
                    config_fingerprint=record.config_fingerprint,
                    output_sha256=record.output_sha256,
                    error=None if record.error is None else dict(record.error),
                )
                for index, record in self._records.items()
            }

    def indexes_with(self, statuses: set[SegmentStatus]) -> list[int]:
        with self._lock:
            return [
                index
                for index, record in self._records.items()
                if record.status in statuses
            ]

    def committed_count(self) -> int:
        with self._lock:
            return sum(
                record.status == SegmentStatus.COMMITTED
                for record in self._records.values()
            )

    @property
    def total_count(self) -> int:
        return len(self._records)

    def progress(self) -> tuple[int, int]:
        return self.committed_count(), self.total_count


def invoke_state_change(
    callback: Callable[[SegmentRecord], None] | None, record: SegmentRecord
) -> None:
    if callback is not None:
        callback(
            SegmentRecord(
                spec=record.spec,
                status=record.status,
                attempts=record.attempts,
                config_fingerprint=record.config_fingerprint,
                output_sha256=record.output_sha256,
                error=None if record.error is None else dict(record.error),
            )
        )

