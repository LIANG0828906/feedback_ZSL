from __future__ import annotations

from dataclasses import dataclass

import numpy as np
from scipy import ndimage, signal


@dataclass(frozen=True)
class LimiterStats:
    applied: bool
    triggered_samples: int
    gain_min: float
    lookahead_samples: int
    sample_ceiling_dbfs: float
    passes: int = 0


def k_weighting_coefficients(sample_rate: int) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    # ITU-R BS.1770 pre-filter (high shelf) and RLB high-pass.
    f0 = 1681.974450955533
    gain_db = 3.999843853973347
    q = 0.7071752369554196
    k = np.tan(np.pi * f0 / sample_rate)
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
    shelf_a = np.array([1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0])

    f0 = 38.13547087602444
    q = 0.5003270373238773
    k = np.tan(np.pi * f0 / sample_rate)
    a0 = 1.0 + k / q + k * k
    highpass_b = np.array([1.0 / a0, -2.0 / a0, 1.0 / a0])
    highpass_a = np.array([1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0])
    return shelf_b, shelf_a, highpass_b, highpass_a


def k_weight(audio: np.ndarray, sample_rate: int) -> np.ndarray:
    values = np.asarray(audio, dtype=np.float64)
    shelf_b, shelf_a, highpass_b, highpass_a = k_weighting_coefficients(sample_rate)
    filtered = signal.lfilter(shelf_b, shelf_a, values, axis=0)
    return signal.lfilter(highpass_b, highpass_a, filtered, axis=0)


def integrated_loudness(audio: np.ndarray, sample_rate: int) -> float:
    values = np.asarray(audio, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    block = max(1, int(round(0.400 * sample_rate)))
    hop = max(1, int(round(0.100 * sample_rate)))
    filtered = k_weight(values, sample_rate)
    if filtered.shape[0] < block:
        filtered = np.pad(filtered, ((0, block - filtered.shape[0]), (0, 0)))
    count = 1 + (filtered.shape[0] - block) // hop
    if count <= 0:
        return float("-inf")
    squared = np.sum(filtered * filtered, axis=1)
    cumulative = np.concatenate(([0.0], np.cumsum(squared, dtype=np.float64)))
    starts = np.arange(count) * hop
    power = (cumulative[starts + block] - cumulative[starts]) / block
    if not np.any(power > 0):
        return float("-inf")
    block_loudness = -0.691 + 10.0 * np.log10(np.maximum(power, 1e-30))
    absolute_gate = block_loudness > -70.0
    if not np.any(absolute_gate):
        return float("-inf")
    absolute_mean = float(np.mean(power[absolute_gate]))
    relative_gate = block_loudness > (-10.0 + -0.691 + 10.0 * np.log10(max(absolute_mean, 1e-30)))
    selected = power[absolute_gate & relative_gate]
    if selected.size == 0:
        selected = power[absolute_gate]
    return float(-0.691 + 10.0 * np.log10(max(float(np.mean(selected)), 1e-30)))


def true_peak_dbtp(audio: np.ndarray, oversample: int = 4) -> float:
    values = np.asarray(audio, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    peak = 0.0
    for channel in range(values.shape[1]):
        oversampled = signal.resample_poly(values[:, channel], oversample, 1, window=("kaiser", 6.0))
        if oversampled.size:
            peak = max(peak, float(np.max(np.abs(oversampled))))
    if peak <= 0.0:
        return float("-inf")
    return 20.0 * np.log10(peak)


def sample_peak_dbfs(audio: np.ndarray) -> tuple[float, float, float]:
    values = np.asarray(audio, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    peaks = np.max(np.abs(values), axis=0)
    left = 20.0 * np.log10(float(peaks[0])) if peaks[0] > 0 else float("-inf")
    right = 20.0 * np.log10(float(peaks[1])) if values.shape[1] > 1 and peaks[1] > 0 else float("-inf")
    maximum = 20.0 * np.log10(float(np.max(peaks))) if np.max(peaks) > 0 else float("-inf")
    return left, right, maximum


def lookahead_limiter(
    audio: np.ndarray,
    sample_rate: int,
    ceiling_dbfs: float,
    *,
    lookahead_ms: float = 5.0,
    release_ms: float = 120.0,
) -> tuple[np.ndarray, LimiterStats]:
    values = np.asarray(audio, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    ceiling = 10.0 ** (ceiling_dbfs / 20.0)
    lookahead = max(1, int(round(lookahead_ms * sample_rate / 1000.0)))
    peak = np.max(np.abs(values), axis=1)
    requested = np.minimum(1.0, ceiling / np.maximum(peak, 1e-30))
    envelope = ndimage.minimum_filter1d(requested, size=2 * lookahead + 1, mode="nearest")
    release_coefficient = 1.0 - np.exp(-1.0 / max(1.0, release_ms * sample_rate / 1000.0))
    gain = np.empty_like(envelope)
    current = 1.0
    for index, target in enumerate(envelope):
        if target < current:
            current = target
        else:
            current += (target - current) * release_coefficient
        gain[index] = current
    limited = values * gain[:, None]
    triggered = int(np.count_nonzero(gain < 1.0 - 1e-12))
    stats = LimiterStats(
        applied=triggered > 0,
        triggered_samples=triggered,
        gain_min=float(np.min(gain)),
        lookahead_samples=lookahead,
        sample_ceiling_dbfs=ceiling_dbfs,
        passes=1,
    )
    return limited, stats


def _scale(audio: np.ndarray, gain: float) -> np.ndarray:
    return np.asarray(audio, dtype=np.float64) * float(gain)


def finalize_loudness(
    mix: np.ndarray,
    sample_rate: int,
    target_lufs: float,
    true_peak_limit_dbtp: float,
) -> tuple[np.ndarray, dict[str, object]]:
    values = np.asarray(mix, dtype=np.float64)
    if values.ndim != 2 or values.shape[1] != 2:
        raise ValueError("master mix must be stereo")
    current_loudness = integrated_loudness(values, sample_rate)
    if not np.isfinite(current_loudness):
        raise ValueError("master mix is silent")

    initial_gain = 10.0 ** ((target_lufs - current_loudness) / 20.0)
    candidate = _scale(values, initial_gain)
    peak_control_target = true_peak_limit_dbtp - 0.25
    candidate_tp = true_peak_dbtp(candidate)
    limiter_stats = LimiterStats(False, 0, 1.0, 0, true_peak_limit_dbtp - 0.4, 0)
    limiter_used = False

    if candidate_tp > peak_control_target:
        limiter_used = True
        sample_ceiling = peak_control_target - 0.4
        candidate, limiter_stats = lookahead_limiter(
            candidate, sample_rate, sample_ceiling
        )

    for _ in range(14):
        loudness = integrated_loudness(candidate, sample_rate)
        delta = target_lufs - loudness
        peak = true_peak_dbtp(candidate)
        target_gain = 10.0 ** (delta / 20.0)
        if abs(delta) <= 0.03 and peak <= peak_control_target:
            break
        if abs(delta) > 0.03:
            candidate = _scale(candidate, target_gain)
        peak = true_peak_dbtp(candidate)
        if peak > peak_control_target:
            limiter_used = True
            current_ceiling = limiter_stats.sample_ceiling_dbfs
            reduction = max(0.05, peak - peak_control_target + 0.10)
            current_ceiling -= reduction
            candidate, new_stats = lookahead_limiter(
                candidate, sample_rate, current_ceiling
            )
            limiter_stats = LimiterStats(
                applied=True,
                triggered_samples=new_stats.triggered_samples,
                gain_min=new_stats.gain_min,
                lookahead_samples=new_stats.lookahead_samples,
                sample_ceiling_dbfs=current_ceiling,
                passes=limiter_stats.passes + 1,
            )

    final_loudness = integrated_loudness(candidate, sample_rate)
    final_peak = true_peak_dbtp(candidate)
    if final_peak > peak_control_target:
        security_gain = 10.0 ** ((peak_control_target - final_peak) / 20.0)
        candidate = _scale(candidate, security_gain)
        limiter_used = True
        limiter_stats = LimiterStats(
            applied=True,
            triggered_samples=limiter_stats.triggered_samples,
            gain_min=limiter_stats.gain_min * security_gain,
            lookahead_samples=limiter_stats.lookahead_samples,
            sample_ceiling_dbfs=limiter_stats.sample_ceiling_dbfs,
            passes=limiter_stats.passes,
        )
        final_loudness = integrated_loudness(candidate, sample_rate)
        final_peak = true_peak_dbtp(candidate)

    result: dict[str, object] = {
        "limiter_used": limiter_used,
        "limiter": limiter_stats,
        "integrated_lufs": final_loudness,
        "true_peak_dbtp": final_peak,
    }
    return candidate, result
