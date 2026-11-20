from __future__ import annotations

import math
from collections.abc import Callable, Iterable
from dataclasses import dataclass

import numpy as np

from .errors import CompileCancelled
from .model import ProjectConfig, WaveSegment
from .util import (
    GENESIS_HASH,
    canonical_json_bytes,
    complement_ranges,
    expand_ranges,
    range_sample_count,
    sha256_hex,
)
from .versioning import (
    ChunkDigest,
    SegmentRecord,
    VersionedSignal,
    chunk_signal_hash,
    compute_content_hash,
)

CancelCheck = Callable[[], bool]
ProgressCallback = Callable[[str, int, int], None]
_SAMPLE_BLOCK = 2048


@dataclass(frozen=True, eq=False)
class SegmentSignal:
    index: int
    start_sample: int
    end_sample: int
    segment: WaveSegment
    left: np.ndarray
    right: np.ndarray
    sample_hash: str

    def __post_init__(self) -> None:
        self.left.setflags(write=False)
        self.right.setflags(write=False)

    @property
    def sample_count(self) -> int:
        return self.end_sample - self.start_sample

    def to_record(self) -> SegmentRecord:
        return SegmentRecord(
            index=self.index,
            start_sample=self.start_sample,
            end_sample=self.end_sample,
            sample_hash=self.sample_hash,
        )


@dataclass(frozen=True, eq=False)
class BuildState:
    version: VersionedSignal
    segment_signals: tuple[SegmentSignal, ...]

    def segment_signal_map(self) -> dict[int, SegmentSignal]:
        return {signal.index: signal for signal in self.segment_signals}


@dataclass(frozen=True)
class CompileResult:
    state: BuildState
    changed_indices: tuple[int, ...]
    dirty_ranges: tuple[tuple[int, int], ...]
    reused_ranges: tuple[tuple[int, int], ...]
    recomputed_samples: int
    reused_samples: int
    recomputed_chunks: int
    reused_chunks: int
    full_rebuild: bool

    @property
    def version(self) -> VersionedSignal:
        return self.state.version

    @property
    def recomputed_interval_count(self) -> int:
        return len(self.dirty_ranges)

    @property
    def reused_interval_count(self) -> int:
        return len(self.reused_ranges)


def _milliseconds_to_samples(milliseconds: float, sample_rate: int) -> int:
    # Round half upward independently of Python's banker rounding.
    return math.floor(milliseconds * sample_rate / 1000.0 + 0.5)


def segment_sample_interval(
    segment: WaveSegment,
    sample_rate: int,
) -> tuple[int, int]:
    start = _milliseconds_to_samples(segment.start_ms, sample_rate)
    count = _milliseconds_to_samples(segment.duration_ms, sample_rate)
    return start, start + count


def _segment_hash(
    segment: WaveSegment,
    start_sample: int,
    end_sample: int,
    left: np.ndarray,
    right: np.ndarray,
) -> str:
    payload = bytearray(b"haptic-segment-v1\0")
    payload.extend(canonical_json_bytes(segment.to_dict()))
    payload.extend(int(start_sample).to_bytes(8, "little", signed=True))
    payload.extend(int(end_sample).to_bytes(8, "little", signed=True))
    payload.extend(np.asarray(left, dtype="<f8", order="C").tobytes())
    payload.extend(np.asarray(right, dtype="<f8", order="C").tobytes())
    return sha256_hex(bytes(payload))


def segment_signal_hash(
    segment: WaveSegment,
    start_sample: int,
    end_sample: int,
    left: np.ndarray,
    right: np.ndarray,
) -> str:
    return _segment_hash(
        segment,
        start_sample,
        end_sample,
        left,
        right,
    )


def _check_cancel(cancel: CancelCheck | None) -> None:
    if cancel is not None and cancel():
        raise CompileCancelled()


def _envelope(kind: str, time_s: np.ndarray, duration_s: float) -> np.ndarray:
    if kind == "rectangular":
        return np.ones_like(time_s)
    if kind == "linear_decay":
        return 1.0 - time_s / duration_s
    if kind == "sine":
        return 0.5 - 0.5 * np.cos(2.0 * np.pi * time_s / duration_s)
    raise ValueError(f"unhandled envelope {kind!r}")


def build_segment_signal(
    segment: WaveSegment,
    config: ProjectConfig,
    *,
    cancel: CancelCheck | None = None,
) -> SegmentSignal:
    start, end = segment_sample_interval(segment, config.sample_rate)
    count = end - start
    left = np.empty(count, dtype=np.float64)
    right = np.empty(count, dtype=np.float64)
    duration_s = segment.duration_ms / 1000.0
    sample_rate = float(config.sample_rate)

    for offset in range(0, count, _SAMPLE_BLOCK):
        _check_cancel(cancel)
        stop = min(count, offset + _SAMPLE_BLOCK)
        time_s = np.arange(offset, stop, dtype=np.float64) / sample_rate
        env = _envelope(segment.envelope, time_s, duration_s)
        carrier = np.sin(2.0 * np.pi * segment.frequency_hz * time_s)
        base = carrier * env * segment.amplitude
        left[offset:stop] = base * segment.left_ratio * float(segment.left_polarity)
        right[offset:stop] = base * segment.right_ratio * float(segment.right_polarity)

    sample_hash = _segment_hash(segment, start, end, left, right)
    return SegmentSignal(
        index=segment.index,
        start_sample=start,
        end_sample=end,
        segment=segment,
        left=left,
        right=right,
        sample_hash=sample_hash,
    )


def apply_limiter(
    raw: np.ndarray,
    config: ProjectConfig,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Return limited values, modified-sample mask, out-of-range mask."""
    low = config.device_min
    high = config.device_max
    out_of_range = (raw < low) | (raw > high)
    if config.limiter == "hard":
        limited = np.clip(raw, low, high)
        return limited, out_of_range.copy(), out_of_range

    ratio = config.soft_knee_ratio
    limited = raw.copy()
    limited_mask = np.zeros(raw.shape, dtype=np.bool_)

    if high > 0.0:
        knee_high = high * ratio
        span_high = high - knee_high
        positive = raw > knee_high
        if span_high > 0.0:
            limited[positive] = knee_high + span_high * np.tanh(
                (raw[positive] - knee_high) / span_high
            )
            limited_mask |= positive

    if low < 0.0:
        knee_low = low * ratio
        span_low = knee_low - low
        negative = raw < knee_low
        if span_low > 0.0:
            limited[negative] = knee_low - span_low * np.tanh(
                (knee_low - raw[negative]) / span_low
            )
            limited_mask |= negative

    return limited, limited_mask, out_of_range


def _total_samples(
    segments: Iterable[WaveSegment],
    sample_rate: int,
) -> int:
    total = 0
    for segment in segments:
        _, end = segment_sample_interval(segment, sample_rate)
        total = max(total, end)
    return total


def _overlay_range(
    *,
    start: int,
    end: int,
    ordered_signals: list[SegmentSignal],
    left: np.ndarray,
    right: np.ndarray,
    limited_mask: np.ndarray,
    out_of_range_mask: np.ndarray,
    config: ProjectConfig,
    cancel: CancelCheck | None,
) -> None:
    for block_start in range(start, end, _SAMPLE_BLOCK):
        _check_cancel(cancel)
        block_end = min(end, block_start + _SAMPLE_BLOCK)
        raw_left = np.zeros(block_end - block_start, dtype=np.float64)
        raw_right = np.zeros(block_end - block_start, dtype=np.float64)
        for signal in ordered_signals:
            overlap_start = max(block_start, signal.start_sample)
            overlap_end = min(block_end, signal.end_sample)
            if overlap_start >= overlap_end:
                continue
            source_start = overlap_start - signal.start_sample
            source_end = source_start + (overlap_end - overlap_start)
            target_start = overlap_start - block_start
            target_end = target_start + (overlap_end - overlap_start)
            raw_left[target_start:target_end] += signal.left[source_start:source_end]
            raw_right[target_start:target_end] += signal.right[source_start:source_end]

        limited_left, limited_l, out_l = apply_limiter(raw_left, config)
        limited_right, limited_r, out_r = apply_limiter(raw_right, config)
        left[block_start:block_end] = limited_left
        right[block_start:block_end] = limited_right
        limited_mask[block_start:block_end] = limited_l | limited_r
        out_of_range_mask[block_start:block_end] = out_l | out_r


def _make_chunk(
    start: int,
    end: int,
    left: np.ndarray,
    right: np.ndarray,
    limited_mask: np.ndarray,
    out_of_range_mask: np.ndarray,
) -> ChunkDigest:
    return ChunkDigest(
        start_sample=start,
        end_sample=end,
        signal_hash=chunk_signal_hash(
            start,
            end,
            left[start:end],
            right[start:end],
        ),
        limited_samples=int(limited_mask[start:end].sum()),
        out_of_range_samples=int(out_of_range_mask[start:end].sum()),
    )


def _chunks_for_all(
    *,
    total: int,
    chunk_size: int,
    left: np.ndarray,
    right: np.ndarray,
    limited_mask: np.ndarray,
    out_of_range_mask: np.ndarray,
    cancel: CancelCheck | None,
) -> tuple[ChunkDigest, ...]:
    chunks: list[ChunkDigest] = []
    for start in range(0, total, chunk_size):
        _check_cancel(cancel)
        end = min(total, start + chunk_size)
        chunks.append(
            _make_chunk(
                start,
                end,
                left,
                right,
                limited_mask,
                out_of_range_mask,
            )
        )
    return tuple(chunks)


def _build_version(
    *,
    previous: VersionedSignal | None,
    config: ProjectConfig,
    segments: tuple[WaveSegment, ...],
    signals: tuple[SegmentSignal, ...],
    chunks: tuple[ChunkDigest, ...],
    left: np.ndarray,
    right: np.ndarray,
    limited_mask: np.ndarray,
    out_of_range_mask: np.ndarray,
    modified_indices: tuple[int, ...],
) -> VersionedSignal:
    version_number = 1 if previous is None else previous.version + 1
    parent_hash = GENESIS_HASH if previous is None else previous.content_hash
    segment_records = tuple(
        signal.to_record() for signal in sorted(signals, key=lambda s: s.index)
    )
    limited_samples = sum(chunk.limited_samples for chunk in chunks)
    out_of_range_samples = sum(chunk.out_of_range_samples for chunk in chunks)
    content_hash = compute_content_hash(
        version=version_number,
        parent_hash=parent_hash,
        config=config,
        segments=segments,
        segment_records=segment_records,
        chunks=chunks,
        limited_samples=limited_samples,
        out_of_range_samples=out_of_range_samples,
    )
    return VersionedSignal(
        version=version_number,
        parent_hash=parent_hash,
        content_hash=content_hash,
        sample_rate=config.sample_rate,
        left=left,
        right=right,
        limited_mask=limited_mask,
        out_of_range_mask=out_of_range_mask,
        segments=segments,
        config=config,
        segment_records=segment_records,
        chunks=chunks,
        limited_samples=limited_samples,
        out_of_range_samples=out_of_range_samples,
        modified_indices=modified_indices,
    )


def compile_signals(
    segments: Iterable[WaveSegment],
    config: ProjectConfig,
    *,
    previous: BuildState | None = None,
    modified_indices: Iterable[int] | None = None,
    force_full: bool = False,
    cancel: CancelCheck | None = None,
    progress: ProgressCallback | None = None,
) -> CompileResult:
    """Build or incrementally update an immutable signal version."""
    segment_tuple = tuple(segments)
    total = _total_samples(segment_tuple, config.sample_rate)
    requested_modified = {int(index) for index in (modified_indices or ())}

    is_full = previous is None or force_full
    changed: set[int] = set()
    if previous is not None and not is_full:
        previous_segments = previous.version.segments
        if (
            previous.version.config != config
            or len(previous_segments) != len(segment_tuple)
            or previous.version.sample_count != total
        ):
            is_full = True
        else:
            for index, (old, new) in enumerate(zip(previous_segments, segment_tuple)):
                if old.index != new.index or old.impact_key() != new.impact_key():
                    changed.add(index)
            invalid_requested = {
                index
                for index in requested_modified
                if index < 0 or index >= len(segment_tuple)
            }
            if invalid_requested:
                is_full = True
            else:
                changed |= requested_modified

    if is_full:
        changed = set(range(len(segment_tuple)))
        new_signals: dict[int, SegmentSignal] = {}
        for position, segment in enumerate(segment_tuple):
            _check_cancel(cancel)
            signal = build_segment_signal(segment, config, cancel=cancel)
            new_signals[position] = signal
            if progress is not None:
                progress("segment", position + 1, len(segment_tuple))
        dirty_ranges = ((0, total),) if total > 0 else ()
        signal_map = new_signals
    elif previous is not None:
        previous_map = previous.segment_signal_map()
        new_signals = dict(previous_map)
        for index in sorted(changed):
            _check_cancel(cancel)
            new_signals[index] = build_segment_signal(
                segment_tuple[index],
                config,
                cancel=cancel,
            )
            if progress is not None:
                progress("segment", index + 1, len(segment_tuple))
        support_ranges: list[tuple[int, int]] = []
        for index in changed:
            old_signal = previous_map.get(index)
            if old_signal is not None and old_signal.sample_count > 0:
                support_ranges.append((old_signal.start_sample, old_signal.end_sample))
            new_signal = new_signals.get(index)
            if new_signal is not None and new_signal.sample_count > 0:
                support_ranges.append((new_signal.start_sample, new_signal.end_sample))
        dirty_ranges = expand_ranges(
            support_ranges,
            padding=1,
            total_samples=total,
        )
        signal_map = new_signals
    else:
        raise AssertionError("unreachable compile mode")

    ordered_signals = sorted(
        signal_map.values(),
        key=lambda signal: (signal.start_sample, signal.index),
    )

    if is_full:
        left = np.zeros(total, dtype=np.float64)
        right = np.zeros(total, dtype=np.float64)
        limited_mask = np.zeros(total, dtype=np.bool_)
        out_of_range_mask = np.zeros(total, dtype=np.bool_)
    else:
        assert previous is not None
        left = previous.version.left.copy()
        right = previous.version.right.copy()
        limited_mask = previous.version.limited_mask.copy()
        out_of_range_mask = previous.version.out_of_range_mask.copy()

    for dirty_start, dirty_end in dirty_ranges:
        _overlay_range(
            start=dirty_start,
            end=dirty_end,
            ordered_signals=ordered_signals,
            left=left,
            right=right,
            limited_mask=limited_mask,
            out_of_range_mask=out_of_range_mask,
            config=config,
            cancel=cancel,
        )
        if progress is not None:
            progress("overlay", dirty_end, total)

    if is_full or previous is None:
        chunks = _chunks_for_all(
            total=total,
            chunk_size=config.chunk_size,
            left=left,
            right=right,
            limited_mask=limited_mask,
            out_of_range_mask=out_of_range_mask,
            cancel=cancel,
        )
        recomputed_chunks = len(chunks)
        reused_chunks = 0
    else:
        previous_chunks = {
            chunk.start_sample: chunk for chunk in previous.version.chunks
        }
        affected_chunk_starts: set[int] = set()
        for dirty_start, dirty_end in dirty_ranges:
            first = (dirty_start // config.chunk_size) * config.chunk_size
            for start in range(first, dirty_end, config.chunk_size):
                if start < total:
                    affected_chunk_starts.add(start)
        chunks_list: list[ChunkDigest] = []
        for start in range(0, total, config.chunk_size):
            _check_cancel(cancel)
            end = min(total, start + config.chunk_size)
            if start in affected_chunk_starts:
                chunks_list.append(
                    _make_chunk(
                        start,
                        end,
                        left,
                        right,
                        limited_mask,
                        out_of_range_mask,
                    )
                )
            else:
                previous_chunk = previous_chunks.get(start)
                if previous_chunk is None or previous_chunk.end_sample != end:
                    chunks_list.append(
                        _make_chunk(
                            start,
                            end,
                            left,
                            right,
                            limited_mask,
                            out_of_range_mask,
                        )
                    )
                else:
                    chunks_list.append(previous_chunk)
        chunks = tuple(chunks_list)
        recomputed_chunks = len(affected_chunk_starts)
        reused_chunks = len(chunks) - recomputed_chunks

    _check_cancel(cancel)
    signal_tuple = tuple(sorted(signal_map.values(), key=lambda signal: signal.index))
    version = _build_version(
        previous=None if previous is None else previous.version,
        config=config,
        segments=segment_tuple,
        signals=signal_tuple,
        chunks=chunks,
        left=left,
        right=right,
        limited_mask=limited_mask,
        out_of_range_mask=out_of_range_mask,
        modified_indices=tuple(sorted(changed)),
    )
    reused_ranges = complement_ranges(dirty_ranges, total)
    recomputed_samples = range_sample_count(dirty_ranges)
    result = CompileResult(
        state=BuildState(
            version=version,
            segment_signals=signal_tuple,
        ),
        changed_indices=tuple(sorted(changed)),
        dirty_ranges=dirty_ranges,
        reused_ranges=reused_ranges,
        recomputed_samples=recomputed_samples,
        reused_samples=total - recomputed_samples,
        recomputed_chunks=recomputed_chunks,
        reused_chunks=reused_chunks,
        full_rebuild=is_full,
    )
    return result
