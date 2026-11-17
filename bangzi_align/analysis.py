from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from .constants import E_ENTRY_DETECTION, E_HIT_DETECTION
from .errors import Diagnostic, DiagnosticError
from .manifest import ChannelSpec, Manifest, SegmentSpec, SourceSet


@dataclass(frozen=True)
class SegmentAlignment:
    index: int
    segment: SegmentSpec
    bangzi_correction_samples: int
    channel_shifts_samples: dict[str, int]
    detected_hits_samples: tuple[int, ...]
    aligned_hit_errors_samples: tuple[int, ...]

    @property
    def max_abs_hit_error_samples(self) -> int:
        return max((abs(value) for value in self.aligned_hit_errors_samples), default=0)


@dataclass(frozen=True)
class SeparationResult:
    window_start_sample: int
    window_end_sample: int
    laosheng_energy_before: float
    laosheng_energy_after: float
    bangqiang_energy_before: float
    bangqiang_energy_after: float
    bangqiang_share_before: float
    bangqiang_share_after: float
    modified_samples: int


@dataclass(frozen=True)
class EntryResult:
    reference_sample: int
    detected_source_sample: int
    detected_aligned_sample: int
    error_samples: int
    separation: SeparationResult


@dataclass(frozen=True)
class AlignmentPlan:
    segments: tuple[SegmentAlignment, ...]
    entries: tuple[EntryResult, ...]
    aligned_lengths: dict[str, int]
    master_total_samples: int

    @property
    def max_abs_hit_error_samples(self) -> int:
        return max((item.max_abs_hit_error_samples for item in self.segments), default=0)

    @property
    def max_abs_entry_error_samples(self) -> int:
        return max((abs(item.error_samples) for item in self.entries), default=0)

    @property
    def bangzi_offsets_samples(self) -> tuple[int, ...]:
        return tuple(item.bangzi_correction_samples for item in self.segments)

    @property
    def entry_samples(self) -> tuple[int, ...]:
        return tuple(item.detected_aligned_sample for item in self.entries)


def _smoothstep(values: np.ndarray) -> np.ndarray:
    clipped = np.clip(values, 0.0, 1.0)
    return clipped * clipped * (3.0 - 2.0 * clipped)


def detect_onsets(signal: np.ndarray, sample_rate: int) -> np.ndarray:
    values = np.asarray(signal, dtype=np.float64)
    if values.size == 0:
        return np.empty(0, dtype=np.int64)
    absolute = np.abs(values)
    frame = max(8, int(round(0.0015 * sample_rate)))
    squared = np.concatenate([[0.0], np.cumsum(absolute * absolute)])
    starts_all = np.arange(values.size, dtype=np.int64)
    ends_all = np.minimum(values.size, starts_all + frame)
    frame_energy = (squared[ends_all] - squared[starts_all]) / np.maximum(
        1, ends_all - starts_all
    )
    envelope = np.sqrt(frame_energy)
    maximum = float(np.max(envelope))
    threshold = max(1e-7, maximum * 0.012)
    active = envelope > threshold
    starts = np.flatnonzero(active & np.concatenate([[True], ~active[:-1]]))
    onsets: list[int] = []
    local_floor = max(1e-8, maximum * 0.0001)
    for start in starts:
        search_start = max(0, start - 20 * frame)
        search_end = min(values.size, start + 20 * frame)
        local = absolute[search_start:search_end]
        if local.size == 0:
            continue
        above = np.flatnonzero(local > local_floor)
        if above.size == 0:
            continue
        onset = search_start + int(above[0])
        if not onsets or onset - onsets[-1] >= int(round(0.025 * sample_rate)):
            onsets.append(onset)
    return np.asarray(onsets, dtype=np.int64)


def _nearest(values: np.ndarray, target: int, tolerance: int) -> int | None:
    if values.size == 0:
        return None
    index = int(np.searchsorted(values, target))
    candidates = []
    if index < values.size:
        candidates.append(int(values[index]))
    if index > 0:
        candidates.append(int(values[index - 1]))
    if not candidates:
        return None
    nearest = min(candidates, key=lambda value: (abs(value - target), value))
    return nearest if abs(nearest - target) <= tolerance else None


def _source_slice(source: np.ndarray, shift: int, start: int, end: int) -> np.ndarray:
    start = max(0, int(start))
    end = max(start, int(end))
    indices = np.arange(start, end, dtype=np.int64) + int(shift)
    result = np.zeros(indices.size, dtype=np.float64)
    valid = (indices >= 0) & (indices < source.size)
    result[valid] = source[indices[valid]]
    return result


def _separation_gains(
    target_samples: np.ndarray, window_start: int, window_end: int
) -> tuple[np.ndarray, np.ndarray]:
    laosheng_gain = np.ones(target_samples.size, dtype=np.float64)
    bangqiang_gain = np.ones(target_samples.size, dtype=np.float64)
    if window_end <= window_start:
        return laosheng_gain, bangqiang_gain
    position = (target_samples - window_start) / float(window_end - window_start)
    inside = (position >= 0.0) & (position <= 1.0)
    ramp = _smoothstep(position[inside])
    laosheng_gain[inside] = 1.0 - 0.44 * ramp
    bangqiang_gain[inside] = 1.0 + 0.18 * ramp
    return laosheng_gain, bangqiang_gain


def separation_energy(
    manifest: Manifest,
    sources: SourceSet,
    segment: SegmentSpec,
    shifts: dict[str, int],
) -> SeparationResult:
    laosheng = manifest.laosheng
    bangqiang = manifest.bangqiang
    window_start, window_end = segment.separation_window_samples
    target_samples = np.arange(window_start, window_end, dtype=np.int64)
    laosheng_slice = _source_slice(
        sources.data[laosheng.id].samples,
        shifts[laosheng.id],
        window_start,
        window_end,
    )
    bangqiang_slice = _source_slice(
        sources.data[bangqiang.id].samples,
        shifts[bangqiang.id],
        window_start,
        window_end,
    )
    laosheng_gain, bangqiang_gain = _separation_gains(
        target_samples, window_start, window_end
    )
    laosheng_after = laosheng_slice * laosheng_gain
    bangqiang_after = bangqiang_slice * bangqiang_gain
    energy_laosheng_before = float(np.sum(laosheng_slice * laosheng_slice))
    energy_bangqiang_before = float(np.sum(bangqiang_slice * bangqiang_slice))
    energy_laosheng_after = float(np.sum(laosheng_after * laosheng_after))
    energy_bangqiang_after = float(np.sum(bangqiang_after * bangqiang_after))
    before_total = energy_laosheng_before + energy_bangqiang_before
    after_total = energy_laosheng_after + energy_bangqiang_after
    share_before = (
        energy_bangqiang_before / before_total if before_total > 0.0 else 0.0
    )
    share_after = energy_bangqiang_after / after_total if after_total > 0.0 else 0.0
    modified = int(np.count_nonzero((laosheng_gain != 1.0) | (bangqiang_gain != 1.0)))
    return SeparationResult(
        window_start_sample=window_start,
        window_end_sample=window_end,
        laosheng_energy_before=energy_laosheng_before,
        laosheng_energy_after=energy_laosheng_after,
        bangqiang_energy_before=energy_bangqiang_before,
        bangqiang_energy_after=energy_bangqiang_after,
        bangqiang_share_before=share_before,
        bangqiang_share_after=share_after,
        modified_samples=modified,
    )


def _aligned_length(source_length: int, shifts: list[int]) -> int:
    lengths = [source_length - shift for shift in shifts]
    return max(0, max(lengths, default=source_length))


def analyze_alignment(manifest: Manifest, sources: SourceSet) -> AlignmentPlan:
    bangzi = manifest.bangzi
    bangzi_source = sources.data[bangzi.id].samples
    bangzi_onsets = detect_onsets(bangzi_source, manifest.sample_rate)
    segment_results: list[SegmentAlignment] = []
    for segment_index, segment in enumerate(manifest.segments):
        tolerance = int(round(0.060 * manifest.sample_rate))
        candidates: set[int] = set()
        for reference in segment.bangzi_reference_samples:
            expected_source = reference + bangzi.start_offset_samples
            index = int(np.searchsorted(bangzi_onsets, expected_source))
            for candidate_index in range(max(0, index - 3), min(bangzi_onsets.size, index + 4)):
                detected = int(bangzi_onsets[candidate_index])
                if abs(detected - expected_source) <= tolerance:
                    candidates.add(reference - detected)
        def score(candidate: int) -> tuple[int, int, int]:
            count = sum(
                1
                for reference in segment.bangzi_reference_samples
                if _nearest(bangzi_onsets, reference - candidate, 1) is not None
            )
            return (
                count,
                -abs(candidate + bangzi.start_offset_samples),
                -abs(candidate),
            )

        if not candidates:
            raise DiagnosticError(
                [
                    Diagnostic(
                        E_HIT_DETECTION,
                        bangzi.id,
                        f"段落 {segment.id} 未检测到击点锚点候选",
                    )
                ]
            )
        correction = int(max(candidates, key=score))
        if score(correction)[0] != len(segment.bangzi_reference_samples):
            raise DiagnosticError(
                [
                    Diagnostic(
                        E_HIT_DETECTION,
                        bangzi.id,
                        f"段落 {segment.id} 无法用单一整数偏移匹配全部 {len(segment.bangzi_reference_samples)} 个击点",
                    )
                ]
            )
        aligned_errors = tuple(
            int(
                _nearest(bangzi_onsets, reference - correction, 1)
                + correction
                - reference
            )
            for reference in segment.bangzi_reference_samples
        )
        if any(abs(error) > 1 for error in aligned_errors):
            raise DiagnosticError(
                [
                    Diagnostic(
                        E_HIT_DETECTION,
                        bangzi.id,
                        f"段落 {segment.id} 无法以单一整数偏移对齐全部锚点",
                    )
                ]
            )
        shifts = {
            channel.id: correction + bangzi.start_offset_samples - channel.start_offset_samples
            for channel in manifest.channels
        }
        segment_results.append(
            SegmentAlignment(
                index=segment_index,
                segment=segment,
                bangzi_correction_samples=correction,
                channel_shifts_samples=shifts,
                detected_hits_samples=tuple(int(value) for value in bangzi_onsets),
                aligned_hit_errors_samples=aligned_errors,
            )
        )

    bangqiang = manifest.bangqiang
    bangqiang_source = sources.data[bangqiang.id].samples
    bangqiang_onsets = detect_onsets(bangqiang_source, manifest.sample_rate)
    entry_results: list[EntryResult] = []
    for alignment in segment_results:
        segment = alignment.segment
        expected = segment.bangqiang_entry_sample + bangqiang.start_offset_samples
        detected = _nearest(
            bangqiang_onsets,
            expected,
            int(round(0.10 * manifest.sample_rate)),
        )
        if detected is None:
            raise DiagnosticError(
                [
                    Diagnostic(
                        E_ENTRY_DETECTION,
                        bangqiang.id,
                        f"段落 {segment.id} 未检测到帮腔进入",
                    )
                ]
            )
        aligned_entry = int(detected + alignment.channel_shifts_samples[bangqiang.id])
        error = aligned_entry - segment.bangqiang_entry_sample
        if abs(error) > 2:
            raise DiagnosticError(
                [
                    Diagnostic(
                        E_ENTRY_DETECTION,
                        bangqiang.id,
                        f"段落 {segment.id} 帮腔进入误差 {error} 采样，超过 2",
                    )
                ]
            )
        separation = separation_energy(
            manifest, sources, segment, alignment.channel_shifts_samples
        )
        entry_results.append(
            EntryResult(
                reference_sample=segment.bangqiang_entry_sample,
                detected_source_sample=int(detected),
                detected_aligned_sample=aligned_entry,
                error_samples=int(error),
                separation=separation,
            )
        )

    aligned_lengths: dict[str, int] = {}
    for channel in manifest.channels:
        shifts = [
            alignment.channel_shifts_samples[channel.id]
            for alignment in segment_results
        ]
        aligned_lengths[channel.id] = _aligned_length(
            int(sources.data[channel.id].samples.size), shifts
        )
    master_total = max(aligned_lengths.values(), default=0)
    if master_total <= 0:
        raise DiagnosticError(
            [Diagnostic(E_HIT_DETECTION, "master", "对齐后的母带长度为零")]
        )
    return AlignmentPlan(
        segments=tuple(segment_results),
        entries=tuple(entry_results),
        aligned_lengths=aligned_lengths,
        master_total_samples=int(master_total),
    )


def segment_channel_slice(
    source: np.ndarray,
    shift: int,
    start: int,
    end: int,
) -> np.ndarray:
    return _source_slice(source, shift, start, end)


def separation_gain_arrays(
    target_samples: np.ndarray, window_start: int, window_end: int
) -> tuple[np.ndarray, np.ndarray]:
    return _separation_gains(target_samples, window_start, window_end)


def decibels_ratio(after: float, before: float) -> float:
    if before <= 0.0 or after <= 0.0:
        return 0.0
    return float(10.0 * math.log10(after / before))

