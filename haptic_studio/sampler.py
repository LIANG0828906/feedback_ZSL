from __future__ import annotations

import base64
import math
import threading
from collections.abc import Iterable, Sequence
from dataclasses import dataclass

import numpy as np

from .canonical import bytes_hash, content_hash
from .errors import ErrorCode, HapticError, wave_error
from .models import DeviceConfig, Envelope, WaveSegment, WaveSequence

SAMPLE_CHUNK = 2048


def ms_to_sample(milliseconds: float, sample_rate: int) -> int:
    """Deterministic non-negative half-up conversion of milliseconds to frames."""
    return math.floor(milliseconds * sample_rate / 1000.0 + 0.5)


def total_frames(sequence: WaveSequence, sample_rate: int) -> int:
    return max(0, ms_to_sample(sequence.total_duration_ms, sample_rate))


def segment_bounds(
    segment: WaveSegment, sample_rate: int, frame_count: int
) -> tuple[int, int]:
    start = min(frame_count, ms_to_sample(segment.start_ms, sample_rate))
    end = min(frame_count, ms_to_sample(segment.end_ms, sample_rate))
    return start, max(start, end)


def round_half_away(values: np.ndarray) -> np.ndarray:
    return np.sign(values) * np.floor(np.abs(values) + 0.5)


class CancelToken:
    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()

    def check(self) -> None:
        if self._event.is_set():
            raise HapticError(ErrorCode.CANCELLED, "recompute cancelled at checkpoint")


@dataclass(frozen=True, slots=True)
class SegmentSignal:
    index: int
    start_sample: int
    end_sample: int
    left: np.ndarray
    right: np.ndarray
    content_hash: str
    factor_hash: str

    @property
    def sample_count(self) -> int:
        return self.end_sample - self.start_sample

    def to_metadata(self) -> dict[str, object]:
        return {
            "index": self.index,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "content_hash": self.content_hash,
            "factor_hash": self.factor_hash,
        }

    def to_payload(self, *, include_samples: bool = True) -> dict[str, object]:
        payload = self.to_metadata()
        if include_samples:
            payload["left_b64"] = base64.b64encode(int_array_bytes(self.left)).decode(
                "ascii"
            )
            payload["right_b64"] = base64.b64encode(int_array_bytes(self.right)).decode(
                "ascii"
            )
        return payload

    @classmethod
    def from_payload(cls, payload: dict[str, object]) -> SegmentSignal:
        try:
            left = int_array_from_bytes(base64.b64decode(str(payload["left_b64"])))
            right = int_array_from_bytes(base64.b64decode(str(payload["right_b64"])))
            signal = cls(
                index=int(payload["index"]),
                start_sample=int(payload["start_sample"]),
                end_sample=int(payload["end_sample"]),
                left=left,
                right=right,
                content_hash=str(payload["content_hash"]),
                factor_hash=str(payload["factor_hash"]),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "invalid segment signal payload",
                details={"error": str(exc)},
            ) from exc
        if signal.sample_count != left.size or left.size != right.size:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "segment signal sample count mismatch",
                details={"index": signal.index},
            )
        if segment_signal_hash(signal) != signal.content_hash:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "segment signal content hash mismatch",
                details={"index": signal.index},
            )
        return signal


@dataclass(frozen=True, slots=True)
class LimitStats:
    limited_sample_count: int
    first_limited_sample: int | None
    left_limited_count: int
    right_limited_count: int
    limiter: str

    def to_dict(self) -> dict[str, object]:
        return {
            "limited_sample_count": self.limited_sample_count,
            "first_limited_sample": self.first_limited_sample,
            "left_limited_count": self.left_limited_count,
            "right_limited_count": self.right_limited_count,
            "limiter": self.limiter,
        }


@dataclass(frozen=True, slots=True)
class SamplingResult:
    total_frames: int
    segment_signals: tuple[SegmentSignal, ...]
    left_bytes: bytes
    right_bytes: bytes
    limited_bytes: bytes
    signal_hash: str
    limit_stats: LimitStats

    @property
    def segment_by_index(self) -> dict[int, SegmentSignal]:
        return {signal.index: signal for signal in self.segment_signals}

    @property
    def left_array(self) -> np.ndarray:
        array = np.frombuffer(self.left_bytes, dtype="<i4")
        array.setflags(write=False)
        return array

    @property
    def right_array(self) -> np.ndarray:
        array = np.frombuffer(self.right_bytes, dtype="<i4")
        array.setflags(write=False)
        return array

    @property
    def limited_mask(self) -> np.ndarray:
        array = np.frombuffer(self.limited_bytes, dtype=np.uint8)
        array.setflags(write=False)
        return array


def int_array_bytes(array: np.ndarray) -> bytes:
    return np.ascontiguousarray(array, dtype="<i4").tobytes(order="C")


def int_array_from_bytes(raw: bytes) -> np.ndarray:
    if len(raw) % 4:
        raise ValueError("integer array byte length is not a multiple of four")
    array = np.frombuffer(raw, dtype="<i4").astype(np.int32, copy=True)
    array.setflags(write=False)
    return array


def segment_factor_hash(segment: WaveSegment, config: DeviceConfig) -> str:
    return content_hash(
        {
            "segment": segment.to_dict(),
            "sample_rate": config.sample_rate,
            "bit_depth": config.bit_depth,
            "left_ratio": config.left_ratio,
            "right_ratio": config.right_ratio,
            "left_polarity": config.left_polarity,
            "right_polarity": config.right_polarity,
            "limiter": config.limiter,
        }
    )


def segment_signal_hash(signal: SegmentSignal) -> str:
    return content_hash(
        {
            "index": signal.index,
            "start_sample": signal.start_sample,
            "end_sample": signal.end_sample,
            "left_sha256": bytes_hash(int_array_bytes(signal.left)),
            "right_sha256": bytes_hash(int_array_bytes(signal.right)),
        }
    )


def validate_for_sampling(sequence: WaveSequence, config: DeviceConfig) -> None:
    sequence.validate()
    nyquist = config.sample_rate / 2.0
    for segment in sequence.segments:
        if segment.frequency_hz > nyquist + 1e-9:
            raise wave_error(
                f"frequency_hz {segment.frequency_hz} exceeds Nyquist limit {nyquist}",
                segment_index=segment.index,
                field_name="frequency_hz",
            )


def compute_segment_signal(
    segment: WaveSegment,
    sequence: WaveSequence,
    config: DeviceConfig,
    *,
    frame_count: int | None = None,
    cancellation: CancelToken | None = None,
) -> SegmentSignal:
    frame_count = (
        total_frames(sequence, config.sample_rate)
        if frame_count is None
        else frame_count
    )
    start, end = segment_bounds(segment, config.sample_rate, frame_count)
    count = end - start
    if count <= 0:
        empty = np.empty(0, dtype=np.int32)
        empty.setflags(write=False)
        signal = SegmentSignal(
            index=segment.index,
            start_sample=start,
            end_sample=end,
            left=empty,
            right=empty,
            content_hash="",
            factor_hash=segment_factor_hash(segment, config),
        )
        return SegmentSignal(
            index=signal.index,
            start_sample=signal.start_sample,
            end_sample=signal.end_sample,
            left=signal.left,
            right=signal.right,
            content_hash=segment_signal_hash(signal),
            factor_hash=signal.factor_hash,
        )

    left_parts: list[np.ndarray] = []
    right_parts: list[np.ndarray] = []
    max_code = float(config.max_code)
    duration_s = segment.duration_ms / 1000.0
    for chunk_start in range(start, end, SAMPLE_CHUNK):
        if cancellation is not None:
            cancellation.check()
        chunk_end = min(end, chunk_start + SAMPLE_CHUNK)
        sample_index = np.arange(chunk_start, chunk_end, dtype=np.float64)
        offset_s = (sample_index - start) / config.sample_rate
        normalized = (
            offset_s / duration_s if duration_s > 0 else np.zeros_like(offset_s)
        )
        normalized = np.clip(normalized, 0.0, 1.0)
        if segment.envelope is Envelope.RECTANGULAR:
            envelope = np.ones_like(normalized)
        elif segment.envelope is Envelope.LINEAR_DECAY:
            envelope = 1.0 - normalized
        elif segment.envelope is Envelope.SINE:
            envelope = np.sin(np.pi * normalized)
        else:
            raise wave_error(
                f"unsupported envelope {segment.envelope}",
                segment_index=segment.index,
                field_name="envelope",
            )
        carrier = np.sin(2.0 * np.pi * segment.frequency_hz * offset_s)
        base = segment.amplitude * envelope * carrier * max_code
        left = round_half_away(base * config.left_ratio * config.left_polarity).astype(
            np.int32
        )
        right = round_half_away(
            base * config.right_ratio * config.right_polarity
        ).astype(np.int32)
        left_parts.append(left)
        right_parts.append(right)

    left_array = np.concatenate(left_parts) if left_parts else np.empty(0, np.int32)
    right_array = np.concatenate(right_parts) if right_parts else np.empty(0, np.int32)
    left_array.setflags(write=False)
    right_array.setflags(write=False)
    signal = SegmentSignal(
        index=segment.index,
        start_sample=start,
        end_sample=end,
        left=left_array,
        right=right_array,
        content_hash="",
        factor_hash=segment_factor_hash(segment, config),
    )
    return SegmentSignal(
        index=signal.index,
        start_sample=signal.start_sample,
        end_sample=signal.end_sample,
        left=signal.left,
        right=signal.right,
        content_hash=segment_signal_hash(signal),
        factor_hash=signal.factor_hash,
    )


def merged_ranges(
    raw_ranges: Iterable[tuple[int, int]], frame_count: int
) -> tuple[tuple[int, int], ...]:
    clamped: list[tuple[int, int]] = []
    for start, end in raw_ranges:
        start = max(0, min(frame_count, int(start)))
        end = max(0, min(frame_count, int(end)))
        if end > start:
            clamped.append((start, end))
    if not clamped:
        return ()
    clamped.sort()
    result: list[tuple[int, int]] = []
    current_start, current_end = clamped[0]
    for start, end in clamped[1:]:
        if start <= current_end:
            current_end = max(current_end, end)
        else:
            result.append((current_start, current_end))
            current_start, current_end = start, end
    result.append((current_start, current_end))
    return tuple(result)


def transition_ranges(
    intervals: Iterable[tuple[int, int]], frame_count: int
) -> tuple[tuple[int, int], ...]:
    expanded = ((start - 1, end + 1) for start, end in intervals if end > start)
    return merged_ranges(expanded, frame_count)


def complement_ranges(
    ranges: Sequence[tuple[int, int]], frame_count: int
) -> tuple[tuple[int, int], ...]:
    result: list[tuple[int, int]] = []
    cursor = 0
    for start, end in merged_ranges(ranges, frame_count):
        if cursor < start:
            result.append((cursor, start))
        cursor = max(cursor, end)
    if cursor < frame_count:
        result.append((cursor, frame_count))
    return tuple(result)


def _sum_segments_for_range(
    signals: Sequence[SegmentSignal], start: int, end: int
) -> tuple[np.ndarray, np.ndarray]:
    left_sum = np.zeros(end - start, dtype=np.int64)
    right_sum = np.zeros(end - start, dtype=np.int64)
    for signal in signals:
        overlap_start = max(start, signal.start_sample)
        overlap_end = min(end, signal.end_sample)
        if overlap_end <= overlap_start:
            continue
        source_start = overlap_start - signal.start_sample
        source_end = source_start + (overlap_end - overlap_start)
        target_start = overlap_start - start
        target_end = target_start + (overlap_end - overlap_start)
        left_sum[target_start:target_end] += signal.left[source_start:source_end]
        right_sum[target_start:target_end] += signal.right[source_start:source_end]
    return left_sum, right_sum


def _limit_pair(
    left_sum: np.ndarray, right_sum: np.ndarray, config: DeviceConfig
) -> tuple[np.ndarray, np.ndarray]:
    min_code = config.min_code
    max_code = config.max_code
    if config.limiter == "hard":
        return (
            np.clip(left_sum, min_code, max_code).astype(np.int32),
            np.clip(right_sum, min_code, max_code).astype(np.int32),
        )
    knee = math.floor(max_code * 0.8)
    span = max(1, max_code - knee)

    def soft(values: np.ndarray) -> np.ndarray:
        magnitude = np.abs(values.astype(np.float64))
        limited = knee + span * (1.0 - np.exp(-(magnitude - knee) / span))
        limited = np.where(magnitude > knee, limited, magnitude)
        limited = np.minimum(limited, float(max_code))
        return (np.sign(values) * round_half_away(limited)).astype(np.int32)

    return soft(left_sum), soft(right_sum)


def _limit_info(
    raw_left: np.ndarray, raw_right: np.ndarray, config: DeviceConfig
) -> tuple[LimitStats, np.ndarray]:
    left_overflow = np.abs(raw_left) > config.max_code
    right_overflow = np.abs(raw_right) > config.max_code
    overflow = left_overflow | right_overflow
    first: int | None = None
    if overflow.any():
        first = int(np.flatnonzero(overflow)[0])
    return (
        LimitStats(
            limited_sample_count=int(np.count_nonzero(overflow)),
            first_limited_sample=first,
            left_limited_count=int(np.count_nonzero(left_overflow)),
            right_limited_count=int(np.count_nonzero(right_overflow)),
            limiter=config.limiter,
        ),
        overflow.astype(np.uint8),
    )


def _signal_hash(
    result: SamplingResult,
    sequence: WaveSequence,
    config: DeviceConfig,
) -> str:
    return content_hash(
        {
            "sequence_hash": sequence.content_hash,
            "config_hash": config.content_hash,
            "total_frames": result.total_frames,
            "segments": [signal.to_metadata() for signal in result.segment_signals],
            "left_sha256": bytes_hash(result.left_bytes),
            "right_sha256": bytes_hash(result.right_bytes),
            "limited_sha256": bytes_hash(result.limited_bytes),
            "limit_stats": result.limit_stats.to_dict(),
        }
    )


def assemble_result(
    sequence: WaveSequence,
    config: DeviceConfig,
    segment_signals: Sequence[SegmentSignal],
    *,
    frame_count: int | None = None,
    recompute_ranges: Sequence[tuple[int, int]] | None = None,
    previous: SamplingResult | None = None,
    cancellation: CancelToken | None = None,
) -> SamplingResult:
    frame_count = (
        total_frames(sequence, config.sample_rate)
        if frame_count is None
        else frame_count
    )
    ordered = tuple(
        sorted(segment_signals, key=lambda item: (item.start_sample, item.index))
    )
    if previous is None or recompute_ranges is None:
        ranges = ((0, frame_count),) if frame_count > 0 else ()
        left_work = np.zeros(frame_count, dtype=np.int32)
        right_work = np.zeros(frame_count, dtype=np.int32)
    else:
        if previous.total_frames != frame_count:
            raise ValueError(
                "cannot reuse previous result with a different frame count"
            )
        ranges = merged_ranges(recompute_ranges, frame_count)
        left_work = np.frombuffer(previous.left_bytes, dtype="<i4").copy()
        right_work = np.frombuffer(previous.right_bytes, dtype="<i4").copy()

    raw_left = np.zeros(frame_count, dtype=np.int64)
    raw_right = np.zeros(frame_count, dtype=np.int64)
    for start, end in ranges:
        for chunk_start in range(start, end, SAMPLE_CHUNK):
            if cancellation is not None:
                cancellation.check()
            chunk_end = min(end, chunk_start + SAMPLE_CHUNK)
            range_left, range_right = _sum_segments_for_range(
                ordered, chunk_start, chunk_end
            )
            limited_left, limited_right = _limit_pair(
                range_left, range_right, config
            )
            left_work[chunk_start:chunk_end] = limited_left
            right_work[chunk_start:chunk_end] = limited_right
    # Limiter statistics describe the whole published signal. Cached segment
    # contributions are summed again for this read-only accounting step; no
    # segment is re-sampled and the affected output ranges remain incremental.
    raw_left, raw_right = _sum_segments_for_range(ordered, 0, frame_count)

    stats, limited_mask = _limit_info(raw_left, raw_right, config)
    left_bytes = int_array_bytes(left_work)
    right_bytes = int_array_bytes(right_work)
    limited_bytes = limited_mask.tobytes(order="C")
    provisional = SamplingResult(
        total_frames=frame_count,
        segment_signals=ordered,
        left_bytes=left_bytes,
        right_bytes=right_bytes,
        limited_bytes=limited_bytes,
        signal_hash="",
        limit_stats=stats,
    )
    return SamplingResult(
        total_frames=frame_count,
        segment_signals=ordered,
        left_bytes=left_bytes,
        right_bytes=right_bytes,
        limited_bytes=limited_bytes,
        signal_hash=_signal_hash(provisional, sequence, config),
        limit_stats=stats,
    )


def compute_full_signal(
    sequence: WaveSequence,
    config: DeviceConfig,
    *,
    cancellation: CancelToken | None = None,
) -> SamplingResult:
    validate_for_sampling(sequence, config)
    frame_count = total_frames(sequence, config.sample_rate)
    signals = [
        compute_segment_signal(
            segment,
            sequence,
            config,
            frame_count=frame_count,
            cancellation=cancellation,
        )
        for segment in sequence.sorted_segments
    ]
    return assemble_result(
        sequence,
        config,
        signals,
        frame_count=frame_count,
        cancellation=cancellation,
    )


def result_from_payload(
    payload: dict[str, object],
    sequence: WaveSequence,
    config: DeviceConfig,
) -> SamplingResult:
    try:
        total = int(payload["total_frames"])
        left_bytes = base64.b64decode(str(payload["left_b64"]))
        right_bytes = base64.b64decode(str(payload["right_b64"]))
        limited_bytes = base64.b64decode(str(payload["limited_b64"]))
        signals = tuple(
            SegmentSignal.from_payload(item)
            for item in payload["segment_signals"]  # type: ignore[union-attr]
        )
        expected_hash = str(payload["signal_hash"])
        stats_payload = payload["limit_stats"]
        stats = LimitStats(
            limited_sample_count=int(stats_payload["limited_sample_count"]),  # type: ignore[index]
            first_limited_sample=(
                None
                if stats_payload["first_limited_sample"] is None  # type: ignore[index]
                else int(stats_payload["first_limited_sample"])  # type: ignore[index]
            ),
            left_limited_count=int(stats_payload["left_limited_count"]),  # type: ignore[index]
            right_limited_count=int(stats_payload["right_limited_count"]),  # type: ignore[index]
            limiter=str(stats_payload["limiter"]),  # type: ignore[index]
        )
    except (KeyError, TypeError, ValueError) as exc:
        raise HapticError(
            ErrorCode.RECOVERY_INVALID,
            "invalid drive signal payload",
            details={"error": str(exc)},
        ) from exc
    if (
        len(left_bytes) != total * 4
        or len(right_bytes) != total * 4
        or len(limited_bytes) != total
    ):
        raise HapticError(
            ErrorCode.RECOVERY_INVALID,
            "drive signal byte length mismatch",
            details={"total_frames": total},
        )
    provisional = SamplingResult(
        total_frames=total,
        segment_signals=signals,
        left_bytes=left_bytes,
        right_bytes=right_bytes,
        limited_bytes=limited_bytes,
        signal_hash="",
        limit_stats=stats,
    )
    actual_hash = _signal_hash(provisional, sequence, config)
    if actual_hash != expected_hash:
        raise HapticError(
            ErrorCode.RECOVERY_INVALID,
            "drive signal hash mismatch",
            details={"expected": expected_hash, "actual": actual_hash},
        )
    return SamplingResult(
        total_frames=total,
        segment_signals=signals,
        left_bytes=left_bytes,
        right_bytes=right_bytes,
        limited_bytes=limited_bytes,
        signal_hash=expected_hash,
        limit_stats=stats,
    )


def result_to_payload(result: SamplingResult) -> dict[str, object]:
    return {
        "total_frames": result.total_frames,
        "segment_signals": [signal.to_payload() for signal in result.segment_signals],
        "left_b64": base64.b64encode(result.left_bytes).decode("ascii"),
        "right_b64": base64.b64encode(result.right_bytes).decode("ascii"),
        "limited_b64": base64.b64encode(result.limited_bytes).decode("ascii"),
        "signal_hash": result.signal_hash,
        "limit_stats": result.limit_stats.to_dict(),
    }
