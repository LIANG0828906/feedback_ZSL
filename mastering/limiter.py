from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np


def _fractional_sinc_channel(x: np.ndarray, fraction: float, half_taps: int = 8) -> np.ndarray:
    """Windowed-sinc interpolation at x[n + fraction] for one channel."""

    length = x.shape[0]
    padded = np.pad(x, (half_taps, half_taps), mode="constant")
    result = np.zeros(length, dtype=np.float64)
    normalization = 0.0
    for offset in range(-half_taps + 1, half_taps + 1):
        distance = fraction - offset
        if abs(distance) >= half_taps:
            continue
        window = math.cos(math.pi * distance / half_taps) ** 2
        weight = math.sin(math.pi * distance) / (math.pi * distance) * window
        normalization += weight
        result += padded[half_taps + offset : half_taps + offset + length] * weight
    if abs(normalization) > 1e-15:
        result /= normalization
    return result


def true_peak_frame_envelope(
    samples: np.ndarray,
    factor: int = 4,
    half_taps: int = 8,
) -> np.ndarray:
    """Estimate per-frame absolute true peak with 4x windowed-sinc oversampling."""

    if factor < 1:
        raise ValueError("oversampling factor must be >= 1")
    envelope = np.max(np.abs(samples), axis=1)
    if factor == 1:
        return envelope
    for phase in range(1, factor):
        fraction = phase / float(factor)
        for channel in range(samples.shape[1]):
            interpolated = _fractional_sinc_channel(
                samples[:, channel], fraction, half_taps
            )
            np.maximum(envelope, np.abs(interpolated), out=envelope)
    return envelope


def true_peak_dbtp(samples: np.ndarray, factor: int = 4) -> tuple[float, float]:
    envelope = true_peak_frame_envelope(samples, factor)
    peak_linear = float(np.max(envelope)) if envelope.size else 0.0
    dbtp = 20.0 * math.log10(max(peak_linear, 1e-30))
    return dbtp, peak_linear


def rolling_min(values: np.ndarray, window: int) -> np.ndarray:
    """Sliding minimum via per-block prefix/suffix minima, O(N) and numpy-only."""

    values = np.asarray(values, dtype=np.float64)
    length = values.shape[0]
    if window <= 1:
        return values.copy()
    if window >= length:
        return np.full(length, float(np.min(values)), dtype=np.float64)
    pad = window - 1
    padded = np.pad(values, (pad, pad), mode="constant", constant_values=np.inf)
    block_count = int(math.ceil(padded.shape[0] / window))
    padded = np.pad(
        padded,
        (0, block_count * window - padded.shape[0]),
        mode="constant",
        constant_values=np.inf,
    )
    blocks = padded.reshape(block_count, window)
    prefix = np.minimum.accumulate(blocks, axis=1)
    suffix = np.minimum.accumulate(blocks[:, ::-1], axis=1)[:, ::-1]

    original_starts = np.arange(length - window + 1, dtype=np.int64) + pad
    block_index = original_starts // window
    offset = original_starts % window
    result = np.empty(length - window + 1, dtype=np.float64)
    aligned = offset == 0
    result[aligned] = prefix[block_index[aligned], window - 1]
    crossing = ~aligned
    result[crossing] = np.minimum(
        suffix[block_index[crossing], offset[crossing]],
        prefix[block_index[crossing] + 1, offset[crossing] - 1],
    )
    return result[:length]


def _asymmetric_min_envelope(
    target_gain: np.ndarray,
    lookahead: int,
    release: int,
) -> np.ndarray:
    """min(target[j-lookahead : j+release+1]) with unity outside the signal."""

    left = max(0, int(lookahead))
    right = max(0, int(release))
    window = left + right + 1
    padded = np.concatenate(
        (
            np.ones(left, dtype=np.float64),
            np.asarray(target_gain, dtype=np.float64),
            np.ones(right, dtype=np.float64),
        )
    )
    envelope = rolling_min(padded, window)
    if envelope.shape[0] != target_gain.shape[0]:
        raise RuntimeError("internal limiter envelope length mismatch")
    return envelope


@dataclass(frozen=True)
class LimiterResult:
    samples: np.ndarray
    report: dict[str, Any]


def limit_true_peak(
    samples: np.ndarray,
    sample_rate: int,
    ceiling_dbtp: float,
    *,
    lookahead_ms: float,
    release_ms: float,
    max_iterations: int,
    safety_margin_db: float,
    oversample: int,
) -> LimiterResult:
    """True-peak limiter with non-causal lookahead and exact frame preservation."""

    input_samples = np.asarray(samples, dtype=np.float64)
    ceiling_linear = 10.0 ** (float(ceiling_dbtp) / 20.0)
    internal_ceiling = ceiling_linear * 10.0 ** (-float(safety_margin_db) / 20.0)
    lookahead = max(1, int(round(float(lookahead_ms) * sample_rate / 1000.0)))
    release = max(0, int(round(float(release_ms) * sample_rate / 1000.0)))
    output = input_samples.copy()
    cumulative_gain = np.ones(output.shape[0], dtype=np.float64)
    initial_envelope = true_peak_frame_envelope(input_samples, oversample)
    triggered_frames = int(np.count_nonzero(initial_envelope > ceiling_linear))
    initial_peak_dbtp = 20.0 * math.log10(
        max(float(np.max(initial_envelope)), 1e-30)
    )
    iterations_used = 0
    global_trim_db = 0.0

    for iteration in range(int(max_iterations)):
        current_envelope = true_peak_frame_envelope(output, oversample)
        over = current_envelope > internal_ceiling
        if not np.any(over):
            break
        iterations_used = iteration + 1
        required = np.ones(output.shape[0], dtype=np.float64)
        required[over] = internal_ceiling / current_envelope[over]
        smoothed = _asymmetric_min_envelope(required, lookahead, release)
        output *= smoothed[:, None]
        cumulative_gain *= smoothed

    final_envelope = true_peak_frame_envelope(output, oversample)
    final_peak = float(np.max(final_envelope))
    if final_peak > internal_ceiling:
        trim = internal_ceiling / final_peak
        output *= trim
        cumulative_gain *= trim
        global_trim_db = 20.0 * math.log10(trim)

    measured_dbtp, _ = true_peak_dbtp(output, oversample)
    minimum_gain = float(np.min(cumulative_gain))
    max_attenuation = -20.0 * math.log10(max(minimum_gain, 1e-30))
    sample_peak = float(np.max(np.abs(output)))
    return LimiterResult(
        samples=output,
        report={
            "ceiling_dbtp": round(float(ceiling_dbtp), 9),
            "internal_ceiling_dbtp": round(
                float(ceiling_dbtp) - float(safety_margin_db), 9
            ),
            "oversample": int(oversample),
            "lookahead_samples": int(lookahead),
            "lookahead_ms": round(lookahead * 1000.0 / sample_rate, 9),
            "release_samples": int(release),
            "release_ms": round(release * 1000.0 / sample_rate, 9),
            "delay_compensated": True,
            "delay_compensation_samples": int(lookahead),
            "triggered_frames": triggered_frames,
            "max_attenuation_db": round(max_attenuation, 9),
            "iterations": iterations_used,
            "global_trim_db": round(global_trim_db, 9),
            "input_true_peak_dbtp": round(initial_peak_dbtp, 9),
            "output_true_peak_dbtp": round(measured_dbtp, 9),
            "output_sample_peak": round(sample_peak, 12),
            "ceiling_respected": bool(measured_dbtp <= float(ceiling_dbtp) + 1e-9),
        },
    )

