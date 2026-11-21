"""Crash-safe per-channel range commits with an append-only hash chain."""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterable

import numpy as np

from .atomic import atomic_write_bytes, atomic_write_json
from .errors import (
    E_RECOVERY_INVALID,
    E_VERSION_STALE,
    SyncError,
)
from .models import DriftFit, SegmentDelay, SyncReport
from .utils import canonical_json_bytes, sha256_bytes, sha256_json


COMMIT_PATTERN = re.compile(r"^commit_(\d{8})\.json$")
REQUIRED_RECORD_FIELDS = {
    "sequence",
    "parameter_version",
    "report_content_sha256",
    "channel",
    "start_sample",
    "end_sample",
    "chunk_path",
    "chunk_sha256",
    "chunk_dtype",
    "chunk_shape",
    "fits",
    "segment_delays",
    "compensation",
    "prev_hash",
    "record_hash",
}


@dataclass(frozen=True)
class ChannelRange:
    channel: int
    start_sample: int
    end_sample: int

    @property
    def length(self) -> int:
        return self.end_sample - self.start_sample

    def overlaps(self, other: "ChannelRange") -> bool:
        return (
            self.channel == other.channel
            and self.start_sample < other.end_sample
            and other.start_sample < self.end_sample
        )

    def to_dict(self) -> dict[str, int]:
        return {
            "channel": self.channel,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
        }


@dataclass
class RecoveryState:
    parameter_version: str
    report_content_sha256: str
    last_sequence: int | None
    last_hash: str
    committed: dict[int, tuple[ChannelRange, ...]]
    records: tuple[dict[str, Any], ...]
    chunks: dict[tuple[int, int, int], np.ndarray] = field(default_factory=dict)

    @property
    def committed_ranges(self) -> tuple[ChannelRange, ...]:
        return tuple(
            channel_range
            for channel in sorted(self.committed)
            for channel_range in self.committed[channel]
        )


@dataclass
class ResumeResult:
    matrix: np.ndarray
    written_ranges: tuple[ChannelRange, ...]
    skipped_committed_ranges: tuple[ChannelRange, ...]
    state: RecoveryState
    trace: tuple[dict[str, Any], ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "written_ranges": [
                channel_range.to_dict() for channel_range in self.written_ranges
            ],
            "skipped_committed_ranges": [
                channel_range.to_dict()
                for channel_range in self.skipped_committed_ranges
            ],
            "trace": list(self.trace),
            "last_sequence": self.state.last_sequence,
            "last_hash": self.state.last_hash,
        }


def _range_key(channel_range: ChannelRange) -> tuple[int, int, int]:
    return (
        channel_range.channel,
        channel_range.start_sample,
        channel_range.end_sample,
    )


def _subtract_ranges(
    requested: Iterable[ChannelRange],
    committed: Iterable[ChannelRange],
) -> tuple[tuple[ChannelRange, ...], tuple[ChannelRange, ...]]:
    committed_list = list(committed)
    remaining: list[ChannelRange] = []
    skipped: list[ChannelRange] = []
    for item in requested:
        if any(item == existing for existing in committed_list):
            skipped.append(item)
            continue
        if any(item.overlaps(existing) for existing in committed_list):
            raise SyncError(
                E_RECOVERY_INVALID,
                "requested compensation range partially overlaps a committed range",
                channel=item.channel,
                details={
                    "requested": item.to_dict(),
                    "committed": [
                        existing.to_dict()
                        for existing in committed_list
                        if item.overlaps(existing)
                    ],
                },
            )
        remaining.append(item)
    return tuple(remaining), tuple(skipped)


class RecoveryJournal:
    def __init__(
        self,
        directory: str | Path,
        report: SyncReport,
    ) -> None:
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.chunks_directory = self.directory / "chunks"
        self.chunks_directory.mkdir(parents=True, exist_ok=True)
        self.report = report
        parameter_version = report.parameter_version
        if report.compensation is None:
            raise SyncError(
                E_VERSION_STALE,
                "recovery journal requires compensation parameters",
                fields=["compensation"],
            )
        if (
            not parameter_version
            or report.compensation.parameter_version != parameter_version
        ):
            raise SyncError(
                E_VERSION_STALE,
                "report and compensation parameter versions differ",
                fields=["parameter_version", "compensation.parameter_version"],
            )
        self.parameter_version = parameter_version
        self.report_content_sha256 = report.content_hash()

    def _record_paths(self) -> list[Path]:
        output: list[Path] = []
        for path in self.directory.iterdir():
            if path.is_file() and COMMIT_PATTERN.match(path.name):
                output.append(path)
        return sorted(output, key=lambda path: path.name)

    def _validate_record_shape(
        self,
        record: dict[str, Any],
        *,
        expected_sequence: int,
        expected_previous_hash: str,
    ) -> None:
        missing = sorted(REQUIRED_RECORD_FIELDS - set(record))
        if missing:
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery commit is missing required fields",
                fields=missing,
                details={"sequence": record.get("sequence")},
            )
        if int(record["sequence"]) != expected_sequence:
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery commit sequence is not contiguous",
                details={
                    "expected": expected_sequence,
                    "actual": record["sequence"],
                },
            )
        if str(record["prev_hash"]) != expected_previous_hash:
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery hash chain is broken",
                details={
                    "sequence": expected_sequence,
                    "expected_previous_hash": expected_previous_hash,
                    "actual_previous_hash": record["prev_hash"],
                },
            )
        payload = dict(record)
        actual_hash = payload.pop("record_hash")
        expected_hash = sha256_json(payload)
        if str(actual_hash) != expected_hash:
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery commit record hash does not match content",
                details={
                    "sequence": expected_sequence,
                    "expected_hash": expected_hash,
                    "actual_hash": actual_hash,
                },
            )
        if str(record["parameter_version"]) != self.parameter_version:
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery commit parameter version does not match the report",
                fields=["parameter_version"],
                details={
                    "expected": self.parameter_version,
                    "actual": record["parameter_version"],
                },
            )
        if str(record["report_content_sha256"]) != self.report_content_sha256:
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery commit report hash does not match",
                fields=["report_content_sha256"],
            )
        if not isinstance(record["fits"], list) or not isinstance(
            record["segment_delays"], list
        ):
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery commit fit or segment payload is malformed",
            )
        if not isinstance(record["compensation"], dict):
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery commit compensation payload is malformed",
            )
        start = int(record["start_sample"])
        end = int(record["end_sample"])
        if start < 0 or end <= start:
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery commit range is invalid",
                channel=int(record["channel"]),
                details={"start_sample": start, "end_sample": end},
            )
        channel_range = ChannelRange(
            channel=int(record["channel"]),
            start_sample=start,
            end_sample=end,
        )
        shape = tuple(int(value) for value in record["chunk_shape"])
        if len(shape) != 1 or shape[0] != channel_range.length:
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery chunk shape does not match committed range",
                channel=channel_range.channel,
                details={
                    "chunk_shape": list(shape),
                    "range_length": channel_range.length,
                },
            )

    def _load_chunk(self, record: dict[str, Any]) -> np.ndarray:
        relative = Path(str(record["chunk_path"]))
        if relative.is_absolute() or ".." in relative.parts:
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery chunk path escapes the journal directory",
                details={"chunk_path": str(relative)},
            )
        path = self.directory / relative
        if not path.is_file():
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery chunk file is missing",
                channel=int(record["channel"]),
                details={"chunk_path": str(relative)},
            )
        data = path.read_bytes()
        digest = sha256_bytes(data)
        if digest != str(record["chunk_sha256"]):
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery chunk hash does not match",
                channel=int(record["channel"]),
                details={
                    "chunk_path": str(relative),
                    "expected": record["chunk_sha256"],
                    "actual": digest,
                },
            )
        dtype = np.dtype(str(record["chunk_dtype"]))
        values = np.frombuffer(data, dtype=dtype).astype(np.float64)
        shape = tuple(int(value) for value in record["chunk_shape"])
        if values.size != int(np.prod(shape)):
            raise SyncError(
                E_RECOVERY_INVALID,
                "recovery chunk byte count does not match its shape",
                channel=int(record["channel"]),
            )
        return values.reshape(shape)

    def load_state(self) -> RecoveryState:
        paths = self._record_paths()
        previous_hash = "0" * 64
        committed: dict[int, list[ChannelRange]] = {}
        chunks: dict[tuple[int, int, int], np.ndarray] = {}
        records: list[dict[str, Any]] = []
        for expected_sequence, path in enumerate(paths):
            try:
                record = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, json.JSONDecodeError) as exc:
                raise SyncError(
                    E_RECOVERY_INVALID,
                    f"cannot read recovery commit: {path.name}",
                    details={"error": str(exc)},
                ) from exc
            if not isinstance(record, dict):
                raise SyncError(
                    E_RECOVERY_INVALID,
                    "recovery commit root must be an object",
                    details={"path": path.name},
                )
            self._validate_record_shape(
                record,
                expected_sequence=expected_sequence,
                expected_previous_hash=previous_hash,
            )
            channel_range = ChannelRange(
                channel=int(record["channel"]),
                start_sample=int(record["start_sample"]),
                end_sample=int(record["end_sample"]),
            )
            for existing in committed.get(channel_range.channel, []):
                if channel_range.overlaps(existing):
                    raise SyncError(
                        E_RECOVERY_INVALID,
                        "recovery commits contain overlapping channel ranges",
                        channel=channel_range.channel,
                        details={
                            "existing": existing.to_dict(),
                            "new": channel_range.to_dict(),
                        },
                    )
            committed.setdefault(channel_range.channel, []).append(channel_range)
            chunks[_range_key(channel_range)] = self._load_chunk(record)
            previous_hash = str(record["record_hash"])
            records.append(record)
        last_sequence = int(records[-1]["sequence"]) if records else None
        return RecoveryState(
            parameter_version=self.parameter_version,
            report_content_sha256=self.report_content_sha256,
            last_sequence=last_sequence,
            last_hash=previous_hash,
            committed={
                channel: tuple(
                    sorted(values, key=lambda item: item.start_sample)
                )
                for channel, values in committed.items()
            },
            records=tuple(records),
            chunks=chunks,
        )

    def _next_sequence_and_hash(self, state: RecoveryState) -> tuple[int, str]:
        if state.last_sequence is None:
            return 0, "0" * 64
        return int(state.last_sequence) + 1, state.last_hash

    def commit_range(
        self,
        channel_range: ChannelRange,
        chunk: np.ndarray,
        *,
        fits: Iterable[DriftFit] | None = None,
        segment_delays: Iterable[SegmentDelay] | None = None,
    ) -> dict[str, Any]:
        state = self.load_state()
        if any(
            channel_range.overlaps(existing)
            for existing in state.committed_ranges
        ):
            raise SyncError(
                E_RECOVERY_INVALID,
                "refusing to rewrite an already committed channel range",
                channel=channel_range.channel,
                details={"range": channel_range.to_dict()},
            )
        values = np.ascontiguousarray(chunk, dtype="<f8").reshape(-1)
        if values.size != channel_range.length:
            raise SyncError(
                E_RECOVERY_INVALID,
                "chunk length does not match the committed range",
                channel=channel_range.channel,
                details={
                    "chunk_length": int(values.size),
                    "range_length": channel_range.length,
                },
            )
        chunk_relative = (
            Path("chunks")
            / (
                f"ch{channel_range.channel:02d}_"
                f"{channel_range.start_sample:010d}_"
                f"{channel_range.end_sample:010d}.f64le"
            )
        )
        chunk_data = values.tobytes(order="C")
        chunk_hash = sha256_bytes(chunk_data)
        atomic_write_bytes(self.directory / chunk_relative, chunk_data)
        fit_payload = [
            fit.to_dict()
            for fit in (
                fits
                if fits is not None
                else [
                    fit
                    for fit in self.report.drift_fits
                    if fit.channel == channel_range.channel
                ]
            )
        ]
        delay_payload = [
            segment.to_dict()
            for segment in (
                segment_delays
                if segment_delays is not None
                else [
                    segment
                    for segment in self.report.segment_delays
                    if segment.channel == channel_range.channel
                    and segment.start_sample >= channel_range.start_sample
                    and segment.end_sample <= channel_range.end_sample
                ]
            )
        ]
        sequence, previous_hash = self._next_sequence_and_hash(state)
        record: dict[str, Any] = {
            "sequence": sequence,
            "parameter_version": self.parameter_version,
            "report_content_sha256": self.report_content_sha256,
            "channel": channel_range.channel,
            "start_sample": channel_range.start_sample,
            "end_sample": channel_range.end_sample,
            "chunk_path": chunk_relative.as_posix(),
            "chunk_sha256": chunk_hash,
            "chunk_dtype": "<f8",
            "chunk_shape": [channel_range.length],
            "fits": fit_payload,
            "segment_delays": delay_payload,
            "compensation": self.report.compensation.to_dict(),
            "prev_hash": previous_hash,
        }
        record["record_hash"] = sha256_json(record)
        path = self.directory / f"commit_{sequence:08d}.json"
        atomic_write_json(path, record)
        return record

    def plan(
        self,
        requested_ranges: Iterable[ChannelRange],
    ) -> tuple[tuple[ChannelRange, ...], tuple[ChannelRange, ...], RecoveryState]:
        state = self.load_state()
        remaining, skipped = _subtract_ranges(
            requested_ranges, state.committed_ranges
        )
        return remaining, skipped, state

    def resume(
        self,
        requested_ranges: Iterable[ChannelRange],
        *,
        channels: Iterable[int],
        output_length: int,
        compensate_range: Callable[[ChannelRange], np.ndarray],
        trace_path: str | Path | None = None,
    ) -> ResumeResult:
        remaining, skipped, state = self.plan(requested_ranges)
        trace: list[dict[str, Any]] = []
        for channel_range in remaining:
            values = np.asarray(
                compensate_range(channel_range), dtype=np.float64
            )
            record = self.commit_range(channel_range, values)
            trace.append(
                {
                    "event": "commit",
                    "sequence": record["sequence"],
                    "channel": channel_range.channel,
                    "start_sample": channel_range.start_sample,
                    "end_sample": channel_range.end_sample,
                    "chunk_sha256": record["chunk_sha256"],
                    "record_hash": record["record_hash"],
                }
            )
        state = self.load_state()
        channel_list = tuple(sorted(set(int(channel) for channel in channels)))
        matrix = np.zeros((len(channel_list), max(0, int(output_length))))
        for channel_range in state.committed_ranges:
            if channel_range.channel not in channel_list:
                continue
            row = channel_list.index(channel_range.channel)
            values = state.chunks[_range_key(channel_range)]
            start = max(0, channel_range.start_sample)
            end = min(matrix.shape[1], channel_range.end_sample)
            width = max(0, end - start)
            if width:
                matrix[row, start:end] = values[:width]
        for channel_range in skipped:
            trace.append(
                {
                    "event": "reuse_committed",
                    "channel": channel_range.channel,
                    "start_sample": channel_range.start_sample,
                    "end_sample": channel_range.end_sample,
                }
            )
        trace.append(
            {
                "event": "resume_complete",
                "last_sequence": state.last_sequence,
                "last_hash": state.last_hash,
                "written_ranges": len(remaining),
                "skipped_ranges": len(skipped),
            }
        )
        if trace_path is not None:
            atomic_write_json(trace_path, {"trace": trace})
        return ResumeResult(
            matrix=matrix,
            written_ranges=remaining,
            skipped_committed_ranges=skipped,
            state=state,
            trace=tuple(trace),
        )


def contiguous_ranges(
    channels: Iterable[int],
    *,
    output_length: int,
    chunk_length: int,
) -> tuple[ChannelRange, ...]:
    output: list[ChannelRange] = []
    for channel in sorted(set(int(value) for value in channels)):
        start = 0
        while start < output_length:
            end = min(output_length, start + max(1, int(chunk_length)))
            output.append(
                ChannelRange(
                    channel=channel,
                    start_sample=start,
                    end_sample=end,
                )
            )
            start = end
    return tuple(output)

