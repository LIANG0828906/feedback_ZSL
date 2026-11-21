"""Segmented and whole-recording cross-correlation delay estimation."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .models import SegmentDelay

_EPS = 1e-12


def segment_bounds(frame_count: int, sample_rate: int, segment_seconds: float) -> list[tuple[int, int]]:
    segment_samples = max(1, int(round(segment_seconds * sample_rate)))
    bounds = [
        (start, min(frame_count, start + segment_samples))
        for start in range(0, frame_count, segment_samples)
    ]
    return [(start, stop) for start, stop in bounds if stop > start]


def _next_power_of_two(value: int) -> int:
    return 1 << max(0, int(value - 1).bit_length())


def _window(length: int) -> np.ndarray:
    if length < 8:
        return np.ones(length, dtype=np.float64)
    return np.hanning(length).astype(np.float64)


@dataclass(frozen=True)
class CcfResult:
    delay_samples: float | None
    peak_correlation: float
    ci_low_samples: float | None
    ci_high_samples: float | None
    lag_index: int | None


def cross_correlation_delay(
    reference: np.ndarray,
    channel: np.ndarray,
    max_lag_samples: int,
) -> CcfResult:
    reference = np.asarray(reference, dtype=np.float64)
    channel = np.asarray(channel, dtype=np.float64)
    if reference.size != channel.size:
        raise ValueError("reference and channel must have equal length")
    if reference.size < 8 or max_lag_samples < 1:
        return CcfResult(None, 0.0, None, None, None)

    window = _window(reference.size)
    ref = (reference - np.mean(reference)) * window
    ch = (channel - np.mean(channel)) * window
    ref_energy = float(np.dot(ref, ref))
    ch_energy = float(np.dot(ch, ch))
    if ref_energy <= _EPS or ch_energy <= _EPS:
        return CcfResult(None, 0.0, None, None, None)

    nfft = _next_power_of_two(2 * reference.size - 1)
    ref_spectrum = np.fft.rfft(ref, n=nfft)
    ch_spectrum = np.fft.rfft(ch, n=nfft)
    circular = np.fft.irfft(ch_spectrum * np.conjugate(ref_spectrum), n=nfft)
    normalization = np.sqrt(ref_energy * ch_energy)

    max_lag = int(min(max_lag_samples, reference.size - 1))
    lags = np.arange(-max_lag, max_lag + 1, dtype=np.int64)
    indices = np.where(lags >= 0, lags, nfft + lags)
    normalized = circular[indices] / normalization
    magnitude = np.abs(normalized)
    peak_offset = int(np.argmax(magnitude))
    peak_value = float(magnitude[peak_offset])
    peak_lag = int(lags[peak_offset])

    fractional = float(peak_lag)
    if 0 < peak_offset < lags.size - 1:
        left = float(magnitude[peak_offset - 1])
        center = float(magnitude[peak_offset])
        right = float(magnitude[peak_offset + 1])
        denominator = left - 2.0 * center + right
        if abs(denominator) > _EPS:
            offset = 0.5 * (left - right) / denominator
            fractional += float(np.clip(offset, -0.5, 0.5))

    curvature = 0.0
    if 0 < peak_offset < lags.size - 1:
        curvature = (
            float(magnitude[peak_offset - 1])
            - 2.0 * float(magnitude[peak_offset])
            + float(magnitude[peak_offset + 1])
        )
    if curvature < -_EPS:
        sigma = float(np.sqrt(-1.0 / curvature))
    else:
        sigma = float(max(0.75, (1.0 - peak_value) * 8.0))
    quality_widening = 1.0 + 4.0 * max(0.0, 1.0 - peak_value)
    ci_half = float(min(max_lag, 1.96 * sigma * quality_widening + 0.05))

    return CcfResult(
        delay_samples=fractional,
        peak_correlation=peak_value,
        ci_low_samples=fractional - ci_half,
        ci_high_samples=fractional + ci_half,
        lag_index=peak_lag,
    )


def estimate_segment(
    reference: np.ndarray,
    channel: np.ndarray,
    sample_rate: int,
    segment_index: int,
    start_sample: int,
    end_sample: int,
    max_lag_seconds: float,
    min_correlation: float,
    method: str,
) -> SegmentDelay:
    max_lag = max(1, int(round(max_lag_seconds * sample_rate)))
    result = cross_correlation_delay(reference, channel, max_lag)
    valid = bool(
        result.delay_samples is not None and result.peak_correlation >= min_correlation
    )
    return SegmentDelay(
        segment_index=segment_index,
        start_sample=start_sample,
        end_sample=end_sample,
        center_sample=(start_sample + end_sample - 1) / 2.0,
        delay_samples=result.delay_samples if valid else None,
        peak_correlation=result.peak_correlation,
        ci_low_samples=result.ci_low_samples if valid else None,
        ci_high_samples=result.ci_high_samples if valid else None,
        valid=valid,
        method=method,
    )


def reference_segment(
    segment_index: int,
    start_sample: int,
    end_sample: int,
) -> SegmentDelay:
    return SegmentDelay(
        segment_index=segment_index,
        start_sample=start_sample,
        end_sample=end_sample,
        center_sample=(start_sample + end_sample - 1) / 2.0,
        delay_samples=0.0,
        peak_correlation=1.0,
        ci_low_samples=0.0,
        ci_high_samples=0.0,
        valid=True,
        method="reference",
    )


def estimate_channel_segments(
    reader,
    channel: int,
    reference_channel: int,
    sample_rate: int,
    bounds: list[tuple[int, int]],
    max_lag_seconds: float,
    min_correlation: float,
    method: str,
    checkpoint=None,
) -> tuple[SegmentDelay, ...]:
    estimates: list[SegmentDelay] = []
    for segment_index, (start, stop) in enumerate(bounds):
        if checkpoint is not None:
            checkpoint(f"delay:channel-{channel}:segment-{segment_index}")
        if channel == reference_channel:
            estimates.append(reference_segment(segment_index, start, stop))
            continue
        reference = reader.read_float(reference_channel, start, stop)
        signal = reader.read_float(channel, start, stop)
        estimates.append(
            estimate_segment(
                reference=reference,
                channel=signal,
                sample_rate=sample_rate,
                segment_index=segment_index,
                start_sample=start,
                end_sample=stop,
                max_lag_seconds=max_lag_seconds,
                min_correlation=min_correlation,
                method=method,
            )
        )
    return tuple(estimates)


def estimate_whole_recording(
    reader,
    channel: int,
    reference_channel: int,
    sample_rate: int,
    max_lag_seconds: float,
    min_correlation: float,
    checkpoint=None,
) -> tuple[SegmentDelay, ...]:
    if checkpoint is not None:
        checkpoint(f"delay:channel-{channel}:whole")
    if channel == reference_channel:
        return (reference_segment(0, 0, reader.frames),)
    reference = reader.read_float(reference_channel)
    signal = reader.read_float(channel)
    return (
        estimate_segment(
            reference=reference,
            channel=signal,
            sample_rate=sample_rate,
            segment_index=0,
            start_sample=0,
            end_sample=reader.frames,
            max_lag_seconds=max_lag_seconds,
            min_correlation=min_correlation,
            method="whole_recording",
        ),
    )


def delay_sequence_diff(
    old_sequences: dict[int, list[SegmentDelay]],
    new_sequences: dict[int, list[SegmentDelay]],
    sample_rate: int,
) -> dict:
    channels: list[dict] = []
    overall_max = 0.0
    overall_location: dict | None = None
    for channel in sorted(set(old_sequences) & set(new_sequences)):
        old = [
            (item.center_sample / sample_rate, item.delay_samples)
            for item in old_sequences[channel]
            if item.valid and item.delay_samples is not None
        ]
        new = [
            (item.center_sample / sample_rate, item.delay_samples)
            for item in new_sequences[channel]
            if item.valid and item.delay_samples is not None
        ]
        if not old or not new:
            channels.append(
                {
                    "channel": channel,
                    "valid_pairs": 0,
                    "max_abs_deviation_samples": None,
                    "max_location_seconds": None,
                    "mean_abs_deviation_samples": None,
                }
            )
            continue
        old_time = np.asarray([item[0] for item in old], dtype=np.float64)
        old_delay = np.asarray([item[1] for item in old], dtype=np.float64)
        new_time = np.asarray([item[0] for item in new], dtype=np.float64)
        new_delay = np.asarray([item[1] for item in new], dtype=np.float64)
        if old_time.size == 1:
            interpolated = np.full_like(new_time, old_delay[0])
        else:
            interpolated = np.interp(new_time, old_time, old_delay)
        difference = new_delay - interpolated
        max_index = int(np.argmax(np.abs(difference)))
        maximum = float(abs(difference[max_index]))
        location = float(new_time[max_index])
        channels.append(
            {
                "channel": channel,
                "valid_pairs": int(difference.size),
                "max_abs_deviation_samples": maximum,
                "max_location_seconds": location,
                "mean_abs_deviation_samples": float(np.mean(np.abs(difference))),
            }
        )
        if maximum > overall_max:
            overall_max = maximum
            overall_location = {"channel": channel, "time_seconds": location}

    return {
        "compared_channels": len(channels),
        "max_abs_deviation_samples": overall_max if overall_location else None,
        "max_location": overall_location,
        "channels": channels,
    }
