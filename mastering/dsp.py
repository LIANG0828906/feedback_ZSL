from __future__ import annotations

import math
from collections import deque
from dataclasses import dataclass

import numpy as np

from .wavio import db_to_gain, gain_to_db


@dataclass(frozen=True)
class FitResult:
    samples: np.ndarray
    original_frames: int
    adapted_frames: int
    trimmed_samples: int
    padded_samples: int


@dataclass(frozen=True)
class DuckResult:
    attenuation_db: np.ndarray
    gain: np.ndarray
    ducked_samples: int
    max_attenuation_db: float


@dataclass(frozen=True)
class LimiterResult:
    samples: np.ndarray
    gain: np.ndarray
    triggered_samples: int
    max_attenuation_db: float
    lookahead_samples: int
    delay_compensation_samples: int
    measured_true_peak_dbtp: float


def trailing_silence_frames(
    samples: np.ndarray,
    sample_rate: int,
    threshold_dbfs: float,
) -> int:
    del sample_rate
    if samples.shape[0] == 0:
        return 0
    threshold = 10.0 ** (threshold_dbfs / 20.0)
    active = np.any(np.abs(samples) > threshold, axis=1)
    active_indices = np.flatnonzero(active)
    if active_indices.size == 0:
        return samples.shape[0]
    return samples.shape[0] - 1 - int(active_indices[-1])


def fit_to_length(
    samples: np.ndarray,
    target_frames: int,
    sample_rate: int,
    threshold_dbfs: float,
) -> tuple[FitResult | None, str | None]:
    original = samples.shape[0]
    if original == target_frames:
        return FitResult(samples, original, original, 0, 0), None
    if original < target_frames:
        padding = target_frames - original
        fitted = np.zeros((target_frames, samples.shape[1]), dtype=np.float64)
        fitted[:original] = samples
        return FitResult(fitted, original, target_frames, 0, padding), None

    excess = original - target_frames
    tail_silence = trailing_silence_frames(samples, sample_rate, threshold_dbfs)
    if tail_silence < excess:
        return (
            None,
            f"track is {excess} frames longer than music, but only {tail_silence} trailing "
            "silent frames are available; trimming would remove audible audio",
        )
    fitted = samples[:target_frames].copy()
    return FitResult(fitted, original, target_frames, excess, 0), None


def _frame_energy_db(samples: np.ndarray, frame_frames: int) -> np.ndarray:
    frame_count = int(math.ceil(samples.shape[0] / frame_frames))
    energy = np.zeros(frame_count, dtype=np.float64)
    for index in range(frame_count):
        frame = samples[index * frame_frames : (index + 1) * frame_frames]
        if frame.size:
            channel_rms = np.sqrt(np.mean(frame * frame, axis=0))
            energy[index] = float(np.max(channel_rms))
    with np.errstate(divide="ignore"):
        return 20.0 * np.log10(np.maximum(energy, 1e-15))


def compute_ducking(
    voice: np.ndarray,
    sample_rate: int,
    depth_db: float,
    threshold_dbfs: float,
    attack_ms: float,
    release_ms: float,
    frame_ms: float,
) -> DuckResult:
    frame_frames = max(1, int(round(sample_rate * frame_ms / 1000.0)))
    frame_db = _frame_energy_db(voice, frame_frames)
    active = frame_db > threshold_dbfs
    attack_samples = max(1, int(round(sample_rate * attack_ms / 1000.0)))
    release_samples = max(1, int(round(sample_rate * release_ms / 1000.0)))
    frames_ahead = int(math.ceil(attack_samples / frame_frames))
    lookahead_active = active.copy()
    if frames_ahead > 0 and active.size:
        shifted = np.zeros_like(active)
        shifted[:-frames_ahead] = active[frames_ahead:]
        lookahead_active |= shifted

    target_frames = np.where(lookahead_active, depth_db, 0.0)
    target = np.repeat(target_frames, frame_frames)[: voice.shape[0]]
    if target.size < voice.shape[0]:
        target = np.pad(target, (0, voice.shape[0] - target.size))

    attenuation = np.empty_like(target)
    attack_coefficient = 1.0 - math.exp(-1.0 / float(attack_samples))
    release_coefficient = 1.0 - math.exp(-1.0 / float(release_samples))
    current = 0.0
    for index, desired in enumerate(target):
        coefficient = attack_coefficient if desired > current else release_coefficient
        current += (float(desired) - current) * coefficient
        attenuation[index] = current

    # Report only audibly meaningful attenuation; the exponential release can
    # leave a mathematically nonzero tail long after the voice has stopped.
    ducked_samples = int(np.count_nonzero(attenuation >= 1.0))
    maximum = float(np.max(attenuation)) if attenuation.size else 0.0
    gain = np.power(10.0, -attenuation / 20.0)
    return DuckResult(attenuation, gain, ducked_samples, maximum)


def _sinc(value: np.ndarray) -> np.ndarray:
    result = np.ones_like(value, dtype=np.float64)
    nonzero = value != 0.0
    result[nonzero] = np.sin(np.pi * value[nonzero]) / (np.pi * value[nonzero])
    return result


def _oversampling_filter(factor: int, taps_per_phase: int = 32) -> np.ndarray:
    half = taps_per_phase * factor
    positions = np.arange(-half, half + 1, dtype=np.float64)
    return _sinc(positions / factor)


def oversample(samples: np.ndarray, factor: int) -> np.ndarray:
    if factor == 1:
        return samples.astype(np.float64, copy=True)
    if samples.ndim != 2:
        raise ValueError("samples must have shape (frames, channels)")
    frame_count, channels = samples.shape
    zeros = np.zeros((frame_count, factor, channels), dtype=np.float64)
    zeros[:, 0, :] = samples
    upsampled = zeros.reshape(frame_count * factor, channels)
    taps = _oversampling_filter(factor)
    delay = taps.size // 2
    output = np.empty_like(upsampled)
    for channel in range(channels):
        source = upsampled[:, channel]
        if source.size > 4096:
            fft_size = 1 << (source.size + taps.size - 1).bit_length()
            full = np.fft.irfft(
                np.fft.rfft(source, n=fft_size) * np.fft.rfft(taps, n=fft_size),
                n=fft_size,
            )[: source.size + taps.size - 1]
        else:
            full = np.convolve(source, taps, mode="full")
        output[:, channel] = full[delay : delay + upsampled.shape[0]]
    return output


def true_peak_frame_peaks(samples: np.ndarray, factor: int) -> np.ndarray:
    if samples.shape[0] == 0:
        return np.zeros(0, dtype=np.float64)
    if factor == 1:
        return np.max(np.abs(samples), axis=1)
    upsampled = np.abs(oversample(samples, factor))
    return upsampled.reshape(samples.shape[0], factor, samples.shape[1]).max(axis=(1, 2))


def true_peak_dbtp(samples: np.ndarray, factor: int = 4) -> float:
    peaks = true_peak_frame_peaks(samples, factor)
    if peaks.size == 0:
        return -300.0
    maximum = float(np.max(peaks))
    return gain_to_db(maximum)


def _forward_minimum(values: np.ndarray, window: int) -> np.ndarray:
    if window <= 0 or values.size == 0:
        return values.copy()
    output = np.empty_like(values)
    candidates: deque[int] = deque()
    for index in range(values.size + window):
        if index < values.size:
            while candidates and values[candidates[-1]] >= values[index]:
                candidates.pop()
            candidates.append(index)
        output_index = index - window
        if output_index < 0:
            continue
        while candidates and candidates[0] < output_index:
            candidates.popleft()
        if not candidates:
            output[output_index] = values[min(output_index, values.size - 1)]
        else:
            output[output_index] = values[candidates[0]]
    return output


def _smooth_limiter_gain(target_gain: np.ndarray, attack_samples: int, release_samples: int) -> np.ndarray:
    output = np.empty_like(target_gain)
    attack = 1.0 - math.exp(-1.0 / float(max(1, attack_samples)))
    release = 1.0 - math.exp(-1.0 / float(max(1, release_samples)))
    current = 1.0
    for index, desired in enumerate(target_gain):
        value = float(desired)
        coefficient = attack if value < current else release
        current += (value - current) * coefficient
        if current > 1.0:
            current = 1.0
        output[index] = current
    return output


def lookahead_limit(
    samples: np.ndarray,
    sample_rate: int,
    ceiling_dbtp: float,
    lookahead_ms: float,
    release_ms: float,
    oversample_factor: int,
    safety_margin_db: float,
) -> LimiterResult:
    source = samples.astype(np.float64, copy=False)
    frame_count = source.shape[0]
    lookahead_samples = max(0, int(round(sample_rate * lookahead_ms / 1000.0)))
    release_samples = max(1, int(round(sample_rate * release_ms / 1000.0)))
    attack_samples = max(1, lookahead_samples // 8)
    internal_ceiling = db_to_gain(ceiling_dbtp - safety_margin_db)
    cumulative_gain = np.ones(frame_count, dtype=np.float64)
    limited = source.copy()
    measured = true_peak_dbtp(limited, oversample_factor)

    for _ in range(5):
        measured = true_peak_dbtp(limited, oversample_factor)
        if measured <= ceiling_dbtp - safety_margin_db + 1e-9:
            break
        peaks = true_peak_frame_peaks(limited, oversample_factor)
        required = np.minimum(1.0, internal_ceiling / np.maximum(peaks, 1e-15))
        if lookahead_samples > 0:
            required = _forward_minimum(required, lookahead_samples * 2)
        smoothed = _smooth_limiter_gain(required, attack_samples, release_samples)
        cumulative_gain *= smoothed
        limited = source * cumulative_gain[:, None]

    measured = true_peak_dbtp(limited, oversample_factor)
    if measured > ceiling_dbtp:
        scalar = min(1.0, internal_ceiling / max(db_to_gain(measured), 1e-15))
        cumulative_gain *= scalar
        limited = source * cumulative_gain[:, None]
        measured = true_peak_dbtp(limited, oversample_factor)

    triggered = int(np.count_nonzero(cumulative_gain < db_to_gain(-0.01)))
    max_attenuation = max(0.0, -gain_to_db(float(np.min(cumulative_gain)))) if frame_count else 0.0
    return LimiterResult(
        samples=limited,
        gain=cumulative_gain,
        triggered_samples=triggered,
        max_attenuation_db=max_attenuation,
        lookahead_samples=lookahead_samples,
        delay_compensation_samples=lookahead_samples,
        measured_true_peak_dbtp=measured,
    )

