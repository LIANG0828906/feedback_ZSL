from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Callable

import numpy as np
from scipy.ndimage import maximum_filter1d
from scipy.signal import resample_poly, sosfilt

from .manifest import ProjectSpec


def kweighting_sos(sample_rate: int) -> np.ndarray:
    f0 = 1681.974450955533
    gain_db = 3.999843853973347
    q = 0.7071752369554196
    k = math.tan(math.pi * f0 / sample_rate)
    vh = 10.0 ** (gain_db / 20.0)
    vb = vh**0.4996667741545416
    a0 = 1.0 + k / q + k * k
    shelf_b = np.array(
        [
            (vh + vb * k / q + k * k) / a0,
            2.0 * (k * k - vh) / a0,
            (vh - vb * k / q + k * k) / a0,
        ]
    )
    shelf_a = np.array(
        [1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0]
    )

    f0 = 38.13547087602444
    q = 0.5003270373238773
    k = math.tan(math.pi * f0 / sample_rate)
    a0 = 1.0 + k / q + k * k
    highpass_b = np.array([1.0, -2.0, 1.0]) / a0
    highpass_a = np.array(
        [1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0]
    )
    return np.vstack(
        (
            np.concatenate((shelf_b, shelf_a)),
            np.concatenate((highpass_b, highpass_a)),
        )
    )


def true_peak_db(samples: np.ndarray, sample_rate: int, oversample: int = 4) -> float:
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    if data.size == 0:
        return -math.inf
    oversampled = resample_poly(data, oversample, 1, axis=0)
    peak = float(np.max(np.abs(oversampled)))
    return 20.0 * math.log10(max(peak, 1e-15))


def _gating_powers(
    samples: np.ndarray,
    sample_rate: int,
    block_seconds: float = 0.4,
    hop_seconds: float = 0.1,
) -> list[float]:
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    block = max(1, int(round(block_seconds * sample_rate)))
    hop = max(1, int(round(hop_seconds * sample_rate)))
    weighted = sosfilt(kweighting_sos(sample_rate), data, axis=0)
    channel_power = np.mean(np.square(weighted), axis=0)
    if weighted.shape[0] < block:
        return [float(np.sum(channel_power))]
    powers: list[float] = []
    for start in range(0, weighted.shape[0] - block + 1, hop):
        means = np.mean(np.square(weighted[start : start + block]), axis=0)
        powers.append(float(np.sum(means)))
    return powers


def integrated_loudness(samples: np.ndarray, sample_rate: int) -> float:
    powers = np.asarray(_gating_powers(samples, sample_rate), dtype=np.float64)
    if powers.size == 0:
        return -math.inf
    with np.errstate(divide="ignore"):
        block_loudness = -0.691 + 10.0 * np.log10(np.maximum(powers, 1e-30))
    absolute = powers[block_loudness >= -70.0]
    if absolute.size == 0:
        return -math.inf
    absolute_mean = float(np.mean(absolute))
    relative_gate = -0.691 + 10.0 * math.log10(max(absolute_mean, 1e-30)) - 10.0
    rel_loudness = -0.691 + 10.0 * np.log10(np.maximum(absolute, 1e-30))
    relative = absolute[rel_loudness >= relative_gate]
    if relative.size == 0:
        relative = absolute
    return -0.691 + 10.0 * math.log10(max(float(np.mean(relative)), 1e-30))


@dataclass(frozen=True)
class LimiterStats:
    triggered_samples: int
    max_gain_reduction_db: float
    processing_delay_samples: int = 0


def limit_true_peak(
    samples: np.ndarray,
    sample_rate: int,
    ceiling_dbtp: float,
    oversample: int = 4,
) -> tuple[np.ndarray, LimiterStats]:
    data = np.asarray(samples, dtype=np.float64).copy()
    if data.ndim == 1:
        data = data[:, None]
    original_peak = np.max(np.abs(data), axis=1)
    working_ceiling_dbtp = ceiling_dbtp - 0.05
    ceiling = 10.0 ** (working_ceiling_dbtp / 20.0)
    total_gain = np.ones(data.shape[0], dtype=np.float64)
    look = max(8, int(round(0.006 * sample_rate)))
    kernel_length = max(9, int(round(0.0015 * sample_rate)) | 1)
    kernel = np.hanning(kernel_length)
    kernel /= float(np.sum(kernel))
    internal_ceiling = ceiling * 10.0 ** (-0.10 / 20.0)

    for _ in range(12):
        if (
            true_peak_db(data, sample_rate, oversample=oversample)
            <= working_ceiling_dbtp
        ):
            break
        base_peak = np.max(np.abs(data), axis=1)
        envelope = maximum_filter1d(
            base_peak, size=2 * look + 1, mode="nearest"
        )
        smoothed = np.convolve(envelope, kernel, mode="same")
        required = np.minimum(
            1.0, internal_ceiling / np.maximum(smoothed, 1e-15)
        )
        data *= required[:, None]
        total_gain *= required

    if true_peak_db(data, sample_rate, oversample=oversample) > working_ceiling_dbtp:
        current_peak = 10.0 ** (
            true_peak_db(data, sample_rate, oversample=oversample) / 20.0
        )
        safety = ceiling / max(current_peak, 1e-15)
        data *= safety
        total_gain *= safety

    triggered = int(np.count_nonzero(original_peak > ceiling))
    minimum_gain = float(np.min(total_gain))
    reduction_db = (
        -20.0 * math.log10(minimum_gain) if minimum_gain < 1.0 else 0.0
    )
    return data, LimiterStats(
        triggered_samples=triggered,
        max_gain_reduction_db=reduction_db,
        processing_delay_samples=0,
    )


@dataclass
class MasteringStats:
    loudness_lufs: float
    true_peak_dbtp: float
    limiter: LimiterStats
    source_gain: float
    contributions: dict[str, float]


def _segment_window(
    length: int,
    sample_rate: int,
    segment_start: int,
    segment_end: int,
    is_first: bool,
    is_last: bool,
    fade_samples: int,
) -> np.ndarray:
    window = np.zeros(length, dtype=np.float64)
    half = fade_samples // 2
    active_start = segment_start - half if not is_first else 0
    active_end = segment_end + (fade_samples - half) if not is_last else length
    active_start = max(0, active_start)
    active_end = min(length, active_end)
    if active_end <= active_start:
        return window
    window[active_start:active_end] = 1.0
    if not is_first and fade_samples > 0:
        fade_in_end = min(active_end, segment_start + (fade_samples - half))
        count = max(0, fade_in_end - active_start)
        if count:
            phase = np.arange(count, dtype=np.float64) / max(1, fade_samples - 1)
            window[active_start : active_start + count] = (
                0.5 - 0.5 * np.cos(math.pi * phase)
            )
    if not is_last and fade_samples > 0:
        fade_out_start = max(active_start, segment_end - half)
        count = max(0, active_end - fade_out_start)
        if count:
            phase = np.arange(count, dtype=np.float64) / max(1, fade_samples - 1)
            window[fade_out_start : fade_out_start + count] = (
                0.5 + 0.5 * np.cos(math.pi * phase)
            )
    return window


def build_gain_envelopes(
    project: ProjectSpec,
) -> tuple[np.ndarray, dict[str, np.ndarray]]:
    total = project.total_samples
    fade = int(round(project.crossfade_seconds * project.sample_rate))
    windows: dict[str, np.ndarray] = {}
    boundaries = [
        int(round(segment.start_seconds * project.sample_rate))
        for segment in project.segments
    ]
    boundaries.append(total)
    for index, segment in enumerate(project.segments):
        start = boundaries[index]
        end = boundaries[index + 1]
        windows[segment.name] = _segment_window(
            total,
            project.sample_rate,
            start,
            end,
            index == 0,
            index == len(project.segments) - 1,
            fade,
        )

    envelopes = np.zeros((len(project.channels), total), dtype=np.float64)
    for channel_index, channel in enumerate(project.channels):
        for segment in project.segments:
            envelopes[channel_index] += windows[segment.name] * segment.gains[channel.name]
    return envelopes, windows


class StereoMixer:
    def __init__(
        self,
        project: ProjectSpec,
        aligned_channels: dict[str, np.ndarray],
    ) -> None:
        self.project = project
        self.envelopes, self.windows = build_gain_envelopes(project)
        pan = np.array([channel.pan for channel in project.channels], dtype=np.float64)
        angle = (pan + 1.0) * math.pi / 4.0
        self.pan_left = np.cos(angle)
        self.pan_right = np.sin(angle)
        self.stack = np.stack(
            [
                np.asarray(aligned_channels[channel.name], dtype=np.float64)
                for channel in project.channels
            ]
        )

    def render_block(self, start: int, stop: int) -> tuple[np.ndarray, np.ndarray]:
        weighted = self.stack[:, start:stop] * self.envelopes[:, start:stop]
        left = np.sum(weighted * self.pan_left[:, None], axis=0)
        right = np.sum(weighted * self.pan_right[:, None], axis=0)
        chunk = np.column_stack((left, right))
        energy = np.sum(np.square(weighted), axis=1) * (
            np.square(self.pan_left) + np.square(self.pan_right)
        )
        return chunk, energy

    def contributions(self, block_size: int) -> dict[str, float]:
        channel_energy = np.zeros(len(self.project.channels), dtype=np.float64)
        for start in range(0, self.project.total_samples, block_size):
            stop = min(self.project.total_samples, start + block_size)
            _, energy = self.render_block(start, stop)
            channel_energy += energy
        total_energy = float(np.sum(channel_energy))
        if total_energy <= 0:
            return {channel.name: 0.0 for channel in self.project.channels}
        return {
            channel.name: float(channel_energy[index] / total_energy)
            for index, channel in enumerate(self.project.channels)
        }


def render_stereo_blocks(
    project: ProjectSpec,
    aligned_channels: dict[str, np.ndarray],
    block_size: int,
    callback: Callable[[np.ndarray, int, int], None] | None = None,
) -> tuple[np.ndarray, dict[str, float], dict[str, np.ndarray]]:
    total = project.total_samples
    mixer = StereoMixer(project, aligned_channels)
    output = np.zeros((total, 2), dtype=np.float64)
    for start in range(0, total, block_size):
        stop = min(total, start + block_size)
        chunk, _ = mixer.render_block(start, stop)
        output[start:stop] = chunk
        if callback is not None:
            callback(chunk, start, stop)
    return output, mixer.contributions(block_size), mixer.windows


def master_compliance(
    source: np.ndarray,
    sample_rate: int,
    target_loudness_lufs: float,
    true_peak_limit_dbtp: float,
) -> tuple[np.ndarray, MasteringStats]:
    measured = integrated_loudness(source, sample_rate)
    if not math.isfinite(measured):
        source_gain = 1.0
    else:
        source_gain = 10.0 ** ((target_loudness_lufs - measured) / 20.0)
    gain = source_gain
    best: tuple[np.ndarray, MasteringStats] | None = None

    for _ in range(10):
        candidate = source * gain
        limited, limiter_stats = limit_true_peak(
            candidate, sample_rate, true_peak_limit_dbtp
        )
        loudness = integrated_loudness(limited, sample_rate)
        peak = true_peak_db(limited, sample_rate)
        stats = MasteringStats(
            loudness_lufs=loudness,
            true_peak_dbtp=peak,
            limiter=limiter_stats,
            source_gain=gain,
            contributions={},
        )
        if best is None or abs(stats.loudness_lufs - target_loudness_lufs) < abs(
            best[1].loudness_lufs - target_loudness_lufs
        ):
            best = (limited, stats)
        if (
            abs(stats.loudness_lufs - target_loudness_lufs) <= 0.08
            and stats.true_peak_dbtp <= true_peak_limit_dbtp
        ):
            return limited, stats
        if not math.isfinite(stats.loudness_lufs):
            break
        error_db = target_loudness_lufs - stats.loudness_lufs
        gain *= 10.0 ** (error_db / 20.0)

    assert best is not None
    return best
