from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np
from scipy.ndimage import minimum_filter1d
from scipy.signal import lfilter, resample_poly

EPSILON = 1e-15


def db_to_linear(value_db: float) -> float:
    return float(10.0 ** (value_db / 20.0))


def linear_to_db(value: float) -> float:
    return float(20.0 * math.log10(max(value, EPSILON)))


def _k_weighting_coefficients(sample_rate: int) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    shelf_f0 = 1681.974450955533
    shelf_gain = 3.999843853973347
    shelf_q = 0.7071752369554196
    k = math.tan(math.pi * shelf_f0 / sample_rate)
    vh = 10.0 ** (shelf_gain / 20.0)
    vb = vh**0.4996667741545416
    a0 = 1.0 + k / shelf_q + k * k
    shelf_b = np.array(
        [
            (vh + vb * k / shelf_q + k * k) / a0,
            2.0 * (k * k - vh) / a0,
            (vh - vb * k / shelf_q + k * k) / a0,
        ]
    )
    shelf_a = np.array(
        [
            1.0,
            2.0 * (k * k - 1.0) / a0,
            (1.0 - k / shelf_q + k * k) / a0,
        ]
    )

    highpass_f0 = 38.13547087602444
    highpass_q = 0.5003270373238773
    k = math.tan(math.pi * highpass_f0 / sample_rate)
    a0 = 1.0 + k / highpass_q + k * k
    highpass_b = np.array([1.0 / a0, -2.0 / a0, 1.0 / a0])
    highpass_a = np.array(
        [
            1.0,
            2.0 * (k * k - 1.0) / a0,
            (1.0 - k / highpass_q + k * k) / a0,
        ]
    )
    return shelf_b, shelf_a, highpass_b, highpass_a


def integrated_loudness(signal: np.ndarray, sample_rate: int) -> float:
    """ITU-R BS.1770-4 integrated loudness in LUFS for a stereo signal."""
    values = np.asarray(signal, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    if values.shape[0] == 0:
        return float("-inf")
    shelf_b, shelf_a, highpass_b, highpass_a = _k_weighting_coefficients(sample_rate)
    weighted = lfilter(shelf_b, shelf_a, values, axis=0)
    weighted = lfilter(highpass_b, highpass_a, weighted, axis=0)
    block = int(round(0.4 * sample_rate))
    step = int(round(0.1 * sample_rate))
    if values.shape[0] < block:
        return float("-inf")
    cumulative = np.concatenate(
        [np.zeros((1, values.shape[1]), dtype=np.float64), np.cumsum(weighted * weighted, axis=0)],
        axis=0,
    )
    starts = np.arange(0, values.shape[0] - block + 1, step, dtype=np.int64)
    energies = (cumulative[starts + block] - cumulative[starts]) / float(block)
    channel_energy = np.sum(energies, axis=1)
    with np.errstate(divide="ignore", invalid="ignore"):
        block_loudness = -0.691 + 10.0 * np.log10(channel_energy)
    absolute = block_loudness > -70.0
    if not np.any(absolute):
        return float("-inf")
    relative_energy = float(np.mean(channel_energy[absolute]))
    relative_threshold = -0.691 + 10.0 * math.log10(max(relative_energy, EPSILON)) - 10.0
    gated = absolute & (block_loudness > relative_threshold)
    if not np.any(gated):
        return float("-inf")
    final_energy = float(np.mean(channel_energy[gated]))
    return float(-0.691 + 10.0 * math.log10(max(final_energy, EPSILON)))


def true_peak_linear(signal: np.ndarray, oversample: int = 4) -> float:
    values = np.asarray(signal, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    if values.size == 0:
        return 0.0
    peak = 0.0
    for channel in range(values.shape[1]):
        oversampled = resample_poly(
            values[:, channel],
            up=oversample,
            down=1,
            window=("kaiser", 8.0),
        )
        peak = max(peak, float(np.max(np.abs(oversampled))))
    return peak


def true_peak_dbtp(signal: np.ndarray, oversample: int = 4) -> float:
    return linear_to_db(true_peak_linear(signal, oversample=oversample))


@dataclass(frozen=True)
class LimiterResult:
    signal: np.ndarray
    triggered_samples: int
    threshold_dbtp: float
    lookahead_samples: int
    minimum_gain_db: float


def sample_limiter(
    signal: np.ndarray,
    ceiling_dbtp: float,
    lookahead_samples: int = 64,
    margin_db: float = 0.9,
) -> LimiterResult:
    """Zero-net-delay lookahead limiter.

    The gain envelope looks into future samples, so no signal delay is introduced
    and no compensating trim is needed. The complete master length is preserved.
    """
    values = np.asarray(signal, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    threshold_dbtp = ceiling_dbtp - margin_db
    threshold = db_to_linear(threshold_dbtp)
    envelope = np.max(np.abs(values), axis=1)
    above = envelope > threshold
    triggered = int(np.count_nonzero(above))
    if triggered == 0:
        return LimiterResult(
            signal=values,
            triggered_samples=0,
            threshold_dbtp=threshold_dbtp,
            lookahead_samples=lookahead_samples,
            minimum_gain_db=0.0,
        )
    desired = np.ones_like(envelope)
    desired[above] = threshold / envelope[above]
    window = max(3, int(lookahead_samples))
    if window % 2 == 0:
        window += 1
    origin = -(window // 2)
    gain = minimum_filter1d(desired, size=window, mode="nearest", origin=origin)
    gain = minimum_filter1d(gain, size=window, mode="nearest", origin=origin)
    gain = np.clip(gain, 0.0, 1.0)
    limited = values * gain[:, None]
    return LimiterResult(
        signal=limited,
        triggered_samples=triggered,
        threshold_dbtp=threshold_dbtp,
        lookahead_samples=lookahead_samples,
        minimum_gain_db=linear_to_db(float(np.min(gain))),
    )


@dataclass(frozen=True)
class FinalizeResult:
    signal: np.ndarray
    gain_db: float
    measured_lufs: float
    measured_true_peak_dbtp: float
    peak_before_limiting_dbtp: float
    limiter_triggered_samples: int
    limiter_threshold_dbtp: float
    lookahead_samples: int
    limiter_minimum_gain_db: float
    peak_reduction_db: float
    iterations: int


def finalize_master(
    pre_master: np.ndarray,
    sample_rate: int,
    target_lufs: float,
    true_peak_max_dbtp: float,
) -> FinalizeResult:
    values = np.asarray(pre_master, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    source_lufs = integrated_loudness(values, sample_rate)
    if not math.isfinite(source_lufs):
        raise ValueError("silent master")
    gain_db = target_lufs - source_lufs
    ceiling_linear = db_to_linear(true_peak_max_dbtp)
    best: FinalizeResult | None = None
    best_error = float("inf")
    peak_before = float("-inf")
    trigger_count = 0
    threshold_dbtp = true_peak_max_dbtp
    min_gain_db = 0.0
    for iteration in range(1, 9):
        candidate = values * db_to_linear(gain_db)
        candidate_peak = true_peak_linear(candidate)
        peak_before = linear_to_db(candidate_peak)
        if candidate_peak > ceiling_linear * 0.999:
            limited = sample_limiter(candidate, true_peak_max_dbtp, lookahead_samples=96, margin_db=0.9)
            rendered = limited.signal
            trigger_count = limited.triggered_samples
            threshold_dbtp = limited.threshold_dbtp
            min_gain_db = limited.minimum_gain_db
        else:
            rendered = candidate
            trigger_count = 0
            threshold_dbtp = true_peak_max_dbtp
            min_gain_db = 0.0
        measured_peak = true_peak_linear(rendered)
        if measured_peak > ceiling_linear:
            rendered = rendered * (ceiling_linear / measured_peak)
            measured_peak = true_peak_linear(rendered)
        measured_lufs = integrated_loudness(rendered, sample_rate)
        error = target_lufs - measured_lufs
        if abs(error) < best_error:
            best_error = abs(error)
            best = FinalizeResult(
                signal=rendered,
                gain_db=gain_db,
                measured_lufs=measured_lufs,
                measured_true_peak_dbtp=linear_to_db(measured_peak),
                peak_before_limiting_dbtp=peak_before,
                limiter_triggered_samples=trigger_count,
                limiter_threshold_dbtp=threshold_dbtp,
                lookahead_samples=96,
                limiter_minimum_gain_db=min_gain_db,
                peak_reduction_db=max(0.0, peak_before - linear_to_db(measured_peak)),
                iterations=iteration,
            )
        if abs(error) <= 0.08 and measured_peak <= ceiling_linear * (1.0 + 1e-9):
            break
        gain_db += error * 0.92
    if best is None:
        raise RuntimeError("loudness normalization did not run")
    return best

