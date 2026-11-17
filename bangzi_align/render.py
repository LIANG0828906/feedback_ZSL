from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from .analysis import (
    AlignmentPlan,
    separation_gain_arrays,
    segment_channel_slice,
)
from .dsp import (
    FinalizeResult,
    db_to_linear,
    finalize_master,
    integrated_loudness,
    true_peak_dbtp,
)
from .manifest import Manifest, SegmentSpec, SourceSet
from .wavio import quantize_pcm


@dataclass(frozen=True)
class SegmentMasterStats:
    lufs: float
    true_peak_dbtp: float


@dataclass(frozen=True)
class MasterBundle:
    master: np.ndarray
    pre_master: np.ndarray
    finalize: FinalizeResult
    segment_stats: tuple[SegmentMasterStats, ...]


def _pan_gains(pan: float) -> tuple[float, float]:
    angle = (float(pan) + 1.0) * math.pi / 4.0
    return math.cos(angle), math.sin(angle)


def _crossfade_ramp(
    target_samples: np.ndarray,
    start: int,
    duration: int,
    rising: bool,
) -> np.ndarray:
    position = np.clip((target_samples - start) / float(max(1, duration)), 0.0, 1.0)
    if rising:
        return np.sin(position * math.pi / 2.0)
    return np.cos(position * math.pi / 2.0)


def _boundary(previous: SegmentSpec, current: SegmentSpec) -> int:
    if current.start_sample <= previous.end_sample:
        return current.start_sample
    return (previous.end_sample + current.start_sample) // 2


def _segment_render_range(
    segment: SegmentSpec,
    previous: SegmentSpec | None,
    following: SegmentSpec | None,
    crossfade: int,
    total: int,
) -> tuple[int, int]:
    low = max(0, segment.start_sample)
    high = min(total, segment.end_sample)
    if previous is not None:
        boundary = _boundary(previous, segment)
        low = min(low, max(0, boundary - crossfade // 2))
    if following is not None:
        boundary = _boundary(segment, following)
        high = max(high, min(total, boundary + crossfade // 2 + (crossfade % 2)))
    return low, high


def _segment_weight(
    target_samples: np.ndarray,
    segment: SegmentSpec,
    previous: SegmentSpec | None,
    following: SegmentSpec | None,
    crossfade: int,
) -> np.ndarray:
    weight = np.zeros(target_samples.size, dtype=np.float64)
    inside = (target_samples >= segment.start_sample) & (target_samples < segment.end_sample)
    weight[inside] = 1.0
    if previous is not None:
        boundary = _boundary(previous, segment)
        start = boundary - crossfade // 2
        ramp = _crossfade_ramp(target_samples, start, crossfade, rising=True)
        weight[target_samples < start] = 0.0
        active = (target_samples >= start) & (target_samples < start + crossfade)
        weight[active] = ramp[active]
        weight[target_samples >= start + crossfade] = 1.0
    if following is not None:
        boundary = _boundary(segment, following)
        start = boundary - crossfade // 2
        ramp = _crossfade_ramp(target_samples, start, crossfade, rising=False)
        active = (target_samples >= start) & (target_samples < start + crossfade)
        weight[active] = np.minimum(weight[active], ramp[active])
        weight[target_samples >= start + crossfade] = 0.0
    if previous is None:
        weight[target_samples < segment.start_sample] = 0.0
    if following is None:
        weight[target_samples >= segment.end_sample] = 0.0
    allowed_start = (
        segment.start_sample - crossfade // 2
        if previous is not None
        else segment.start_sample
    )
    allowed_end = (
        segment.end_sample + crossfade // 2 + (crossfade % 2)
        if following is not None
        else segment.end_sample
    )
    weight[(target_samples < allowed_start) | (target_samples >= allowed_end)] = 0.0
    return np.clip(weight, 0.0, 1.0)


def render_pre_master(
    manifest: Manifest,
    sources: SourceSet,
    plan: AlignmentPlan,
) -> np.ndarray:
    total = plan.master_total_samples
    output = np.zeros((total, 2), dtype=np.float64)
    pan_gains = {
        channel.id: _pan_gains(channel.pan) for channel in manifest.channels
    }
    for alignment_index, alignment in enumerate(plan.segments):
        segment = alignment.segment
        previous = (
            manifest.segments[alignment_index - 1] if alignment_index > 0 else None
        )
        following = (
            manifest.segments[alignment_index + 1]
            if alignment_index + 1 < len(manifest.segments)
            else None
        )
        low, high = _segment_render_range(
            segment,
            previous,
            following,
            manifest.crossfade_samples,
            total,
        )
        if high <= low:
            continue
        target_samples = np.arange(low, high, dtype=np.int64)
        weight = _segment_weight(
            target_samples,
            segment,
            previous,
            following,
            manifest.crossfade_samples,
        )
        if not np.any(weight):
            continue
        for channel in manifest.channels:
            samples = segment_channel_slice(
                sources.data[channel.id].samples,
                alignment.channel_shifts_samples[channel.id],
                low,
                high,
            )
            if channel.id in (manifest.laosheng.id, manifest.bangqiang.id):
                laosheng_gain, bangqiang_gain = separation_gain_arrays(
                    target_samples,
                    segment.separation_window_samples[0],
                    segment.separation_window_samples[1],
                )
                if channel.id == manifest.laosheng.id:
                    samples = samples * laosheng_gain
                else:
                    samples = samples * bangqiang_gain
            gain = db_to_linear(segment.channel_gains_db[channel.id])
            panned_left, panned_right = pan_gains[channel.id]
            contribution = samples * gain * weight
            output[low:high, 0] += contribution * panned_left
            output[low:high, 1] += contribution * panned_right
    return output


def _segment_stats(master: np.ndarray, segment: SegmentSpec, sample_rate: int) -> SegmentMasterStats:
    low = max(0, segment.start_sample)
    high = min(master.shape[0], segment.end_sample)
    if high <= low:
        return SegmentMasterStats(lufs=float("-inf"), true_peak_dbtp=float("-inf"))
    portion = master[low:high]
    return SegmentMasterStats(
        lufs=integrated_loudness(portion, sample_rate),
        true_peak_dbtp=true_peak_dbtp(portion),
    )


def prepare_master(
    manifest: Manifest,
    sources: SourceSet,
    plan: AlignmentPlan,
) -> MasterBundle:
    pre_master = render_pre_master(manifest, sources, plan)
    finalized = finalize_master(
        pre_master,
        manifest.sample_rate,
        manifest.target_lufs,
        manifest.true_peak_max_dbtp,
    )
    stats = tuple(
        _segment_stats(finalized.signal, segment, manifest.sample_rate)
        for segment in manifest.segments
    )
    return MasterBundle(
        master=finalized.signal,
        pre_master=pre_master,
        finalize=finalized,
        segment_stats=stats,
    )


def master_block_bytes(master: np.ndarray, bit_depth: int, start: int, end: int) -> bytes:
    block = np.asarray(master[start:end], dtype=np.float64)
    interleaved = np.ascontiguousarray(block).reshape(-1)
    return quantize_pcm(interleaved, bit_depth)

