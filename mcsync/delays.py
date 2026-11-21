"""Segmented cross-correlation delay estimation against a reference channel."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable

import numpy as np

from .models import Marker, SegmentDelay, SegmentationConfig
from .wavio import AudioData


@dataclass(frozen=True)
class SegmentWindow:
    index: int
    start_sample: int
    end_sample: int


@dataclass
class SegmentEstimate:
    segment: SegmentDelay
    reused: bool = False
    content_hash: str = ""


def segment_windows(
    frame_count: int,
    config: SegmentationConfig,
) -> tuple[SegmentWindow, ...]:
    length = int(config.segment_length_samples)
    output: list[SegmentWindow] = []
    start = 0
    index = 0
    while start < frame_count:
        end = min(frame_count, start + length)
        output.append(
            SegmentWindow(index=index, start_sample=start, end_sample=end)
        )
        start = end
        index += 1
    return tuple(output)


def _marker_map(markers: Iterable[Marker]) -> dict[int, float]:
    return {marker.marker_index: marker.arrival_sample for marker in markers}


def _robust_line(
    x: np.ndarray,
    y: np.ndarray,
) -> tuple[float, float, np.ndarray]:
    if x.size == 0:
        return 0.0, 0.0, np.zeros(0, dtype=np.float64)
    if x.size == 1 or float(np.ptp(x)) == 0.0:
        values = y - float(np.mean(y))
        return 0.0, float(np.mean(y)), values
    design = np.column_stack((np.ones_like(x), x))
    weights = np.ones_like(y)
    coefficients = np.array([float(np.mean(y)), 0.0])
    residuals = y.copy()
    for _ in range(3):
        weighted = design * np.sqrt(weights)[:, None]
        target = y * np.sqrt(weights)
        coefficients, _, _, _ = np.linalg.lstsq(weighted, target, rcond=None)
        residuals = y - design @ coefficients
        center = float(np.median(residuals))
        scale = 1.4826 * float(np.median(np.abs(residuals - center)))
        scale = max(scale, 0.5)
        normalized = np.abs(residuals - center) / (1.345 * scale)
        weights = np.ones_like(normalized)
        positive = normalized > 1.0
        weights[positive] = 1.0 / normalized[positive]
    return float(coefficients[1]), float(coefficients[0]), residuals


def _next_fast_length(value: int) -> int:
    return 1 << max(1, int(value - 1).bit_length())


def _normalized_xcorr(
    reference: np.ndarray,
    channel: np.ndarray,
    *,
    coarse_lag: float,
    search_samples: int,
) -> tuple[float, float, float, float]:
    length = int(reference.size)
    if length == 0 or channel.size != length:
        return float(coarse_lag), 0.0, 0.5, 0.5
    centered_reference = reference - float(np.mean(reference))
    centered_channel = channel - float(np.mean(channel))
    energy = float(
        np.sqrt(
            np.sum(centered_reference**2, dtype=np.float64)
            * np.sum(centered_channel**2, dtype=np.float64)
        )
    )
    if energy <= 1.0e-15:
        return float(coarse_lag), 0.0, 0.5, 0.5
    nfft = _next_fast_length(2 * length - 1)
    spectrum = np.fft.rfft(centered_channel, n=nfft) * np.conj(
        np.fft.rfft(centered_reference, n=nfft)
    )
    circular = np.fft.irfft(spectrum, n=nfft)
    correlation = np.concatenate(
        (circular[-(length - 1) :], circular[:length])
    )
    lags = np.arange(-length + 1, length, dtype=np.int64)
    low = int(np.floor(coarse_lag - search_samples))
    high = int(np.ceil(coarse_lag + search_samples))
    mask = (lags >= low) & (lags <= high)
    if not np.any(mask):
        return float(coarse_lag), 0.0, 0.5, 0.5
    search_indices = np.flatnonzero(mask)
    local = correlation[search_indices]
    peak_offset = int(np.argmax(local))
    peak_index = int(search_indices[peak_offset])
    peak_lag = float(lags[peak_index])
    if 0 < peak_index < correlation.size - 1:
        left = float(correlation[peak_index - 1])
        center = float(correlation[peak_index])
        right = float(correlation[peak_index + 1])
        denominator = left - 2.0 * center + right
        if abs(denominator) > 1.0e-15:
            peak_lag += float(
                np.clip(0.5 * (left - right) / denominator, -0.5, 0.5)
            )
    peak_correlation = float(
        np.clip(correlation[peak_index] / energy, -1.0, 1.0)
    )
    local_normalized = local / energy
    plateau = local_normalized >= max(
        peak_correlation * 0.95,
        peak_correlation - 1.0e-4,
    )
    plateau_indices = np.flatnonzero(plateau)
    if plateau_indices.size:
        width = (
            float(plateau_indices[-1] - plateau_indices[0] + 1)
            * 0.5
        )
    else:
        width = 0.5
    return peak_lag, peak_correlation, max(0.5, width), max(0.5, width)


def estimate_segment_delay(
    reference_samples: np.ndarray,
    channel_samples: np.ndarray,
    *,
    channel: int,
    window: SegmentWindow,
    reference_markers: Iterable[Marker],
    channel_markers: Iterable[Marker],
    config: SegmentationConfig,
    sample_rate: int,
    fallback_delay: float = 0.0,
) -> SegmentDelay:
    reference_map = _marker_map(reference_markers)
    channel_map = _marker_map(channel_markers)
    common = sorted(set(reference_map).intersection(channel_map))
    pair_positions = np.array(
        [reference_map[index] for index in common], dtype=np.float64
    )
    pair_delays = np.array(
        [channel_map[index] - reference_map[index] for index in common],
        dtype=np.float64,
    )
    if pair_positions.size:
        slope, intercept, pair_residuals = _robust_line(
            pair_positions, pair_delays
        )
        midpoint = 0.5 * (window.start_sample + window.end_sample - 1)
        coarse = intercept + slope * midpoint
        if pair_positions.size >= 2:
            scatter = float(np.std(pair_residuals, ddof=1)) / np.sqrt(
                float(pair_positions.size)
            )
            scatter_ci = 1.96 * scatter
        else:
            scatter_ci = 1.0
    else:
        coarse = float(fallback_delay)
        scatter_ci = 2.0
    reference_segment = reference_samples[window.start_sample : window.end_sample]
    channel_segment = channel_samples[window.start_sample : window.end_sample]
    delay, correlation, lower_width, upper_width = _normalized_xcorr(
        reference_segment,
        channel_segment,
        coarse_lag=coarse,
        search_samples=(
            min(config.delay_search_samples, config.refine_search_samples)
            if pair_positions.size
            else config.delay_search_samples
        ),
    )
    ci_half = max(scatter_ci, 0.5 * (lower_width + upper_width), 0.5)
    return SegmentDelay(
        channel=channel,
        segment_index=window.index,
        start_sample=window.start_sample,
        end_sample=window.end_sample,
        delay_samples=float(delay),
        peak_correlation=float(correlation),
        ci_low_samples=float(delay - ci_half),
        ci_high_samples=float(delay + ci_half),
        marker_count=len(common),
        method="segmented_normalized_xcorr",
    )


def estimate_channel_segments(
    audio: AudioData,
    *,
    channel: int,
    reference_channel: int,
    marker_map: dict[int, tuple[Marker, ...]],
    config: SegmentationConfig,
    sample_rate: int | None = None,
) -> tuple[SegmentDelay, ...]:
    rate = int(sample_rate or audio.sample_rate)
    windows = segment_windows(audio.frame_count, config)
    if channel == reference_channel:
        return tuple(
            SegmentDelay(
                channel=channel,
                segment_index=window.index,
                start_sample=window.start_sample,
                end_sample=window.end_sample,
                delay_samples=0.0,
                peak_correlation=1.0,
                ci_low_samples=0.0,
                ci_high_samples=0.0,
                marker_count=len(
                    [
                        marker
                        for marker in marker_map.get(channel, ())
                        if window.start_sample <= marker.arrival_sample < window.end_sample
                    ]
                ),
                method="reference",
            )
            for window in windows
        )
    output: list[SegmentDelay] = []
    previous_delay = 0.0
    previous_slope = 0.0
    for window in windows:
        reference_markers = tuple(
            marker
            for marker in marker_map.get(reference_channel, ())
            if window.start_sample <= marker.arrival_sample < window.end_sample
        )
        channel_markers = tuple(
            marker
            for marker in marker_map.get(channel, ())
            if window.start_sample <= marker.arrival_sample < window.end_sample
        )
        estimate = estimate_segment_delay(
            audio.channel(reference_channel),
            audio.channel(channel),
            channel=channel,
            window=window,
            reference_markers=reference_markers,
            channel_markers=channel_markers,
            config=config,
            sample_rate=rate,
            fallback_delay=previous_delay
            + previous_slope * config.segment_length_samples,
        )
        if window.index >= 1:
            previous_slope = estimate.delay_samples - previous_delay
        previous_delay = estimate.delay_samples
        output.append(estimate)
    return tuple(output)


def estimate_all_segment_delays(
    audio: AudioData,
    *,
    reference_channel: int,
    marker_map: dict[int, tuple[Marker, ...]],
    config: SegmentationConfig,
    channels: Iterable[int] | None = None,
) -> tuple[SegmentDelay, ...]:
    channel_list = tuple(
        range(audio.channel_count) if channels is None else sorted(set(channels))
    )
    output: list[SegmentDelay] = []
    for channel in channel_list:
        output.extend(
            estimate_channel_segments(
                audio,
                channel=channel,
                reference_channel=reference_channel,
                marker_map=marker_map,
                config=config,
            )
        )
    return tuple(output)

