"""Offline and incrementally committed loudness analysis."""

from __future__ import annotations

import bisect
from collections.abc import Iterable, Sequence
from dataclasses import dataclass, replace
from typing import Any

from .dsp import DspContext, TruePeak, lufs_from_energy
from .errors import LufsDivergenceError
from .params import AnalysisParameters, digest_json
from .wavio import AudioBuffer

DEFAULT_TOLERANCE = 1e-6


@dataclass(frozen=True)
class BlockMeasurement:
    index: int
    start_sample: int
    end_sample: int
    sync_marker: str
    content_hash: str
    mean_square: float
    momentary_lufs: float | None
    short_lufs: float | None
    true_peak: TruePeak
    measurement_hash: str


@dataclass(frozen=True)
class BlockResult:
    index: int
    start_sample: int
    end_sample: int
    sync_marker: str
    content_hash: str
    parameter_version: int
    mean_square: float
    momentary_lufs: float | None
    short_lufs: float | None
    integrated_lufs: float | None
    true_peak_dbtp: float | None
    true_peak_position_sample: float
    true_peak_channel: int
    peak_exceeded: bool
    measurement_hash: str

    def as_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "window_start_sample": self.start_sample,
            "window_end_sample": self.end_sample,
            "window_start_seconds": self.start_sample / 48_000.0,
            "window_end_seconds": self.end_sample / 48_000.0,
            "sync_marker": self.sync_marker,
            "content_hash": self.content_hash,
            "parameter_version": self.parameter_version,
            "mean_square": self.mean_square,
            "momentary_lufs": self.momentary_lufs,
            "short_lufs": self.short_lufs,
            "integrated_lufs": self.integrated_lufs,
            "true_peak_dbtp": self.true_peak_dbtp,
            "true_peak_position_sample": self.true_peak_position_sample,
            "true_peak_channel": self.true_peak_channel,
            "peak_exceeded": self.peak_exceeded,
            "measurement_hash": self.measurement_hash,
        }


@dataclass(frozen=True)
class AnalysisResult:
    audio: AudioBuffer
    params: AnalysisParameters
    parameter_version: int
    blocks: tuple[BlockResult, ...]
    cache_stats: dict[str, Any]

    @property
    def integrated_lufs(self) -> float | None:
        return self.blocks[-1].integrated_lufs if self.blocks else None


@dataclass(frozen=True)
class IntegratedPoint:
    integrated_lufs: float | None
    relative_gate_lufs: float | None
    absolute_gated_count: int
    relative_gated_count: int


class _EnergyBuckets:
    """Sorted energy multiset with O(sqrt(n)) insertion and threshold queries."""

    _BUCKET_SIZE = 128

    def __init__(self) -> None:
        self._buckets: list[list[float]] = []
        self._sums: list[float] = []
        self._maxima: list[float] = []

    def add(self, value: float) -> None:
        if not self._buckets:
            self._buckets.append([value])
            self._sums.append(value)
            self._maxima.append(value)
            return
        bucket_index = bisect.bisect_left(self._maxima, value)
        if bucket_index == len(self._buckets):
            bucket_index -= 1
        bucket = self._buckets[bucket_index]
        bisect.insort(bucket, value)
        self._sums[bucket_index] += value
        self._maxima[bucket_index] = bucket[-1]
        if len(bucket) > 2 * self._BUCKET_SIZE:
            midpoint = len(bucket) // 2
            right = bucket[midpoint:]
            del bucket[midpoint:]
            self._buckets.insert(bucket_index + 1, right)
            self._sums[bucket_index] = sum(bucket)
            self._sums.insert(bucket_index + 1, sum(right))
            self._maxima[bucket_index] = bucket[-1]
            self._maxima.insert(bucket_index + 1, right[-1])

    def sum_above(self, threshold: float) -> tuple[int, float]:
        count = 0
        total = 0.0
        for bucket, bucket_sum in zip(self._buckets, self._sums):
            if bucket[-1] <= threshold:
                continue
            if bucket[0] > threshold:
                count += len(bucket)
                total += bucket_sum
                continue
            index = bisect.bisect_right(bucket, threshold)
            count += len(bucket) - index
            total += sum(bucket[index:])
        return count, total


class IntegratedTracker:
    """Streaming implementation of the BS.1770 absolute/relative gates."""

    def __init__(self, absolute_gate_lufs: float, relative_gate_lu: float) -> None:
        self.absolute_gate_lufs = absolute_gate_lufs
        self.relative_gate_lu = relative_gate_lu
        self._energies = _EnergyBuckets()
        self._absolute_count = 0
        self._absolute_sum = 0.0

    def push(self, measurement: BlockMeasurement) -> IntegratedPoint:
        loudness = measurement.momentary_lufs
        if loudness is not None and loudness >= self.absolute_gate_lufs:
            self._absolute_count += 1
            self._absolute_sum += measurement.mean_square
            self._energies.add(measurement.mean_square)
        if self._absolute_count == 0:
            return IntegratedPoint(None, None, 0, 0)
        absolute_mean_lufs = lufs_from_energy(self._absolute_sum / self._absolute_count)
        if absolute_mean_lufs is None:
            return IntegratedPoint(None, None, self._absolute_count, 0)
        relative_gate_lufs = absolute_mean_lufs + self.relative_gate_lu
        relative_gate_energy = 10.0 ** ((relative_gate_lufs + 0.691) / 10.0)
        selected_count, selected_sum = self._energies.sum_above(relative_gate_energy)
        integrated = (
            lufs_from_energy(selected_sum / selected_count) if selected_count else None
        )
        return IntegratedPoint(
            integrated_lufs=integrated,
            relative_gate_lufs=relative_gate_lufs,
            absolute_gated_count=self._absolute_count,
            relative_gated_count=selected_count,
        )


class AnalysisCache:
    """Parameter-sensitive block cache shared by online and offline paths."""

    def __init__(self, audio: AudioBuffer) -> None:
        self.audio = audio
        self._contexts: dict[str, DspContext] = {}
        self._measurements: dict[tuple[str, int], BlockMeasurement] = {}
        self.measurement_hits = 0
        self.measurement_misses = 0

    def context(self, params: AnalysisParameters) -> DspContext:
        matrix_digest = digest_json(params.downmix)
        context = self._contexts.get(matrix_digest)
        if context is None:
            context = DspContext.build(self.audio, params)
            self._contexts[matrix_digest] = context
            if len(self._contexts) > 4:
                oldest = next(iter(self._contexts))
                if oldest != matrix_digest:
                    self._contexts.pop(oldest, None)
        return context

    def block_count(self, params: AnalysisParameters) -> int:
        return self.context(params).block_count

    def measurement(self, index: int, params: AnalysisParameters) -> BlockMeasurement:
        key = (params.measurement_signature(), index)
        cached = self._measurements.get(key)
        if cached is not None:
            self.measurement_hits += 1
            return cached
        self.measurement_misses += 1
        context = self.context(params)
        start, end = context.block_bounds[index]
        mean_square = context.mean_square(start, end, params.weights)
        short_lufs = None
        if end >= params.short_window_samples:
            short_energy = context.mean_square(
                end - params.short_window_samples,
                end,
                params.weights,
            )
            short_lufs = lufs_from_energy(short_energy)
        peak = context.true_peaks[index]
        content_hash = self.audio.content_hash(start, end)
        sync_marker = self.audio.sync_marker(start, end)
        payload = {
            "index": index,
            "start": start,
            "end": end,
            "sync": sync_marker,
            "content": content_hash,
            "energy": mean_square,
            "momentary": lufs_from_energy(mean_square),
            "short": short_lufs,
            "true_peak": peak.dbtp,
            "true_peak_position": peak.position_sample,
            "true_peak_channel": peak.channel,
        }
        measurement = BlockMeasurement(
            index=index,
            start_sample=start,
            end_sample=end,
            sync_marker=sync_marker,
            content_hash=content_hash,
            mean_square=mean_square,
            momentary_lufs=lufs_from_energy(mean_square),
            short_lufs=short_lufs,
            true_peak=peak,
            measurement_hash=digest_json(payload),
        )
        self._measurements[key] = measurement
        return measurement

    def stats(self, used_measurements: int = 0) -> dict[str, Any]:
        return {
            "measurement_hits": self.measurement_hits,
            "measurement_misses": self.measurement_misses,
            "cached_measurements": len(self._measurements),
            "cached_contexts": len(self._contexts),
            "used_measurements": used_measurements,
        }


def integrated_series(
    measurements: Sequence[BlockMeasurement],
    absolute_gate_lufs: float,
    relative_gate_lu: float,
) -> tuple[IntegratedPoint, ...]:
    tracker = IntegratedTracker(absolute_gate_lufs, relative_gate_lu)
    return tuple(tracker.push(measurement) for measurement in measurements)


def build_block_results(
    measurements: Sequence[BlockMeasurement],
    params: AnalysisParameters,
    parameter_version: int,
) -> tuple[BlockResult, ...]:
    points = integrated_series(
        measurements,
        params.absolute_gate_lufs,
        params.relative_gate_lu,
    )
    return tuple(
        block_result_from_measurement(
            measurement,
            params,
            parameter_version,
            point,
        )
        for measurement, point in zip(measurements, points)
    )


def block_result_from_measurement(
    measurement: BlockMeasurement,
    params: AnalysisParameters,
    parameter_version: int,
    point: IntegratedPoint,
) -> BlockResult:
    return BlockResult(
        index=measurement.index,
        start_sample=measurement.start_sample,
        end_sample=measurement.end_sample,
        sync_marker=measurement.sync_marker,
        content_hash=measurement.content_hash,
        parameter_version=parameter_version,
        mean_square=measurement.mean_square,
        momentary_lufs=measurement.momentary_lufs,
        short_lufs=measurement.short_lufs,
        integrated_lufs=point.integrated_lufs,
        true_peak_dbtp=measurement.true_peak.dbtp,
        true_peak_position_sample=measurement.true_peak.position_sample,
        true_peak_channel=measurement.true_peak.channel,
        peak_exceeded=(
            measurement.true_peak.dbtp is not None
            and measurement.true_peak.dbtp > params.true_peak_limit_dbtp
        ),
        measurement_hash=measurement.measurement_hash,
    )


def analyze_audio(
    audio: AudioBuffer,
    params: AnalysisParameters,
    *,
    parameter_version: int = 1,
    cache: AnalysisCache | None = None,
) -> AnalysisResult:
    params.validate(audio.channels)
    active_cache = cache or AnalysisCache(audio)
    count = active_cache.block_count(params)
    measurements = [active_cache.measurement(index, params) for index in range(count)]
    blocks = build_block_results(measurements, params, parameter_version)
    return AnalysisResult(
        audio=audio,
        params=params,
        parameter_version=parameter_version,
        blocks=blocks,
        cache_stats=active_cache.stats(count),
    )


def _values_equal(
    left: float | None,
    right: float | None,
    tolerance: float,
) -> bool:
    if left is None or right is None:
        return left is None and right is None
    return abs(left - right) <= tolerance


def compare_online_offline(
    online: Iterable[BlockResult],
    offline: Iterable[BlockResult],
    *,
    tolerance: float = DEFAULT_TOLERANCE,
) -> dict[str, Any]:
    online_blocks = tuple(online)
    offline_blocks = tuple(offline)
    online_by_sync = {block.sync_marker: block for block in online_blocks}
    offline_by_sync = {block.sync_marker: block for block in offline_blocks}
    extra = [
        block
        for block in online_by_sync.values()
        if block.sync_marker not in offline_by_sync
    ]
    if extra:
        first_extra = min(extra, key=lambda block: block.index)
        raise LufsDivergenceError(
            "online snapshot contains a block absent from the offline baseline",
            block_index=first_extra.index,
            sync_marker=first_extra.sync_marker,
            online=_block_metrics(first_extra),
            offline=None,
        )
    compared = 0
    max_abs_delta = 0.0
    for offline_block in offline_blocks:
        online_block = online_by_sync.get(offline_block.sync_marker)
        if online_block is None:
            raise LufsDivergenceError(
                "online snapshot is missing an offline observation block",
                block_index=offline_block.index,
                sync_marker=offline_block.sync_marker,
                online=None,
                offline=_block_metrics(offline_block),
            )
        for field in (
            "momentary_lufs",
            "short_lufs",
            "integrated_lufs",
            "true_peak_dbtp",
        ):
            online_value = getattr(online_block, field)
            offline_value = getattr(offline_block, field)
            if not _values_equal(online_value, offline_value, tolerance):
                raise LufsDivergenceError(
                    "online and offline loudness values diverged",
                    block_index=offline_block.index,
                    sync_marker=offline_block.sync_marker,
                    field=field,
                    online=online_value,
                    offline=offline_value,
                    tolerance=tolerance,
                )
            if online_value is not None and offline_value is not None:
                max_abs_delta = max(max_abs_delta, abs(online_value - offline_value))
        if (
            abs(
                online_block.true_peak_position_sample
                - offline_block.true_peak_position_sample
            )
            > 1e-9
        ):
            raise LufsDivergenceError(
                "true-peak position diverged",
                block_index=offline_block.index,
                sync_marker=offline_block.sync_marker,
                field="true_peak_position_sample",
                online=online_block.true_peak_position_sample,
                offline=offline_block.true_peak_position_sample,
            )
        compared += 1
    return {
        "compared_blocks": compared,
        "tolerance": tolerance,
        "max_abs_delta": max_abs_delta,
    }


def _block_metrics(block: BlockResult) -> dict[str, Any]:
    return {
        "momentary_lufs": block.momentary_lufs,
        "short_lufs": block.short_lufs,
        "integrated_lufs": block.integrated_lufs,
        "true_peak_dbtp": block.true_peak_dbtp,
    }


def recompute_integrated_blocks(
    old_blocks: Sequence[BlockResult],
    measurements: Sequence[BlockMeasurement],
    params: AnalysisParameters,
    new_version: int,
) -> tuple[tuple[BlockResult, ...], tuple[int, ...]]:
    points = integrated_series(
        measurements,
        params.absolute_gate_lufs,
        params.relative_gate_lu,
    )
    output: list[BlockResult] = []
    affected: list[int] = []
    for old, measurement, point in zip(old_blocks, measurements, points):
        if old.integrated_lufs != point.integrated_lufs:
            affected.append(old.index)
            output.append(
                replace(
                    old,
                    integrated_lufs=point.integrated_lufs,
                    parameter_version=new_version,
                )
            )
        else:
            output.append(old)
    return tuple(output), tuple(affected)
