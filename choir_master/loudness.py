from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import MasterError
from .util import linear_to_db


# ITU-R BS.1770-4 K-weighting coefficients for 48 kHz.
K_SHELF_B = np.array([1.53512485958697, -2.69169618940638, 1.19839281085285])
K_SHELF_A = np.array([1.0, -1.69065929318241, 0.73248077421585])
K_HIGHPASS_B = np.array([1.0, -2.0, 1.0])
K_HIGHPASS_A = np.array([1.0, -1.99004745483398, 0.99007225036621])


def _biquad(signal: np.ndarray, b: np.ndarray, a: np.ndarray) -> np.ndarray:
    x = np.asarray(signal, dtype=np.float64)
    y = np.empty_like(x)
    b0, b1, b2 = (float(value) for value in b)
    a1, a2 = float(a[1]), float(a[2])
    z1 = 0.0
    z2 = 0.0
    for index in range(len(x)):
        sample = float(x[index])
        output = b0 * sample + z1
        z1 = b1 * sample - a1 * output + z2
        z2 = b2 * sample - a2 * output
        y[index] = output
    return y


def k_weight(signal: np.ndarray, sample_rate: int) -> np.ndarray:
    if sample_rate != 48_000:
        raise MasterError(
            "E_UNSUPPORTED_SAMPLE_RATE",
            "The built-in BS.1770 coefficients require 48 kHz.",
            details={"actual": sample_rate},
        )
    array = np.asarray(signal, dtype=np.float64)
    if array.ndim == 1:
        return _biquad(_biquad(array, K_SHELF_B, K_SHELF_A), K_HIGHPASS_B, K_HIGHPASS_A)
    result = np.empty_like(array)
    for channel in range(array.shape[1]):
        result[:, channel] = _biquad(
            _biquad(array[:, channel], K_SHELF_B, K_SHELF_A),
            K_HIGHPASS_B,
            K_HIGHPASS_A,
        )
    return result


def integrated_loudness(signal: np.ndarray, sample_rate: int = 48_000) -> float:
    array = np.asarray(signal, dtype=np.float64)
    if array.ndim == 1:
        array = array[:, None]
    if array.shape[0] == 0:
        return -math.inf
    block = int(round(0.400 * sample_rate))
    hop = int(round(0.100 * sample_rate))
    if array.shape[0] < block:
        array = np.pad(array, ((0, block - array.shape[0]), (0, 0)))
    weighted = k_weight(array, sample_rate)
    block_count = 1 + (weighted.shape[0] - block) // hop
    energies = np.empty(block_count, dtype=np.float64)
    for index in range(block_count):
        start = index * hop
        segment = weighted[start : start + block]
        # BS.1770 sums channel mean-square energies (all channel weights are
        # 1.0 for this stereo programme). Averaging channels would under-read
        # loudness by 3.01 dB for decorrelated stereo content.
        energies[index] = float(np.sum(np.mean(segment**2, axis=0)))
    with np.errstate(divide="ignore"):
        loudness = -0.691 + 10.0 * np.log10(np.maximum(energies, 1e-30))
    absolute = energies[loudness > -70.0]
    if absolute.size == 0:
        return -math.inf
    relative_threshold = -0.691 + 10.0 * math.log10(float(np.mean(absolute))) - 10.0
    gated = absolute[
        (-0.691 + 10.0 * np.log10(np.maximum(absolute, 1e-30))) > relative_threshold
    ]
    if gated.size == 0:
        return -math.inf
    return float(-0.691 + 10.0 * math.log10(float(np.mean(gated))))


def _oversample_4x(signal: np.ndarray) -> np.ndarray:
    x = np.asarray(signal, dtype=np.float64)
    length = x.shape[0]
    output_length = length * 4
    spectrum = np.fft.rfft(x, axis=0)
    target_shape = (output_length // 2 + 1, x.shape[1])
    expanded = np.zeros(target_shape, dtype=np.complex128)
    expanded[: spectrum.shape[0]] = spectrum
    return np.fft.irfft(expanded, n=output_length, axis=0) * 4.0


def true_peak_linear(signal: np.ndarray) -> float:
    array = np.asarray(signal, dtype=np.float64)
    if array.ndim == 1:
        array = array[:, None]
    if array.shape[0] == 0:
        return 0.0
    oversampled = _oversample_4x(array)
    return float(np.max(np.abs(oversampled)))


def true_peak_dbtp(signal: np.ndarray) -> float:
    peak = true_peak_linear(signal)
    return linear_to_db(peak)


def _moving_min_future(values: np.ndarray, width: int) -> np.ndarray:
    """Minimum over values[n:n+width], implemented with a monotonic deque."""

    n = len(values)
    width = max(1, min(width, n))
    result = np.empty(n, dtype=np.float64)
    deque_indices: list[int] = []
    deque_values: list[float] = []
    head = 0
    next_index = 0
    for index in range(n):
        window_end = min(n, index + width)
        while next_index < window_end:
            value = float(values[next_index])
            while len(deque_values) > head and deque_values[-1] >= value:
                deque_values.pop()
                deque_indices.pop()
            deque_values.append(value)
            deque_indices.append(next_index)
            next_index += 1
        while head < len(deque_indices) and deque_indices[head] < index:
            head += 1
        if head > 4096:
            deque_indices = deque_indices[head:]
            deque_values = deque_values[head:]
            head = 0
        result[index] = deque_values[head]
    return result


@dataclass
class LimiterResult:
    audio: np.ndarray
    gain: np.ndarray
    triggered_samples: int
    lookahead_samples: int
    delay_compensated: bool
    ceiling_scale: float


def true_peak_limit(
    signal: np.ndarray,
    sample_rate: int,
    limit_dbtp: float,
    *,
    lookahead_samples: int = 48,
    release_s: float = 0.060,
) -> LimiterResult:
    array = np.asarray(signal, dtype=np.float64)
    if array.ndim == 1:
        array = array[:, None]
    length = array.shape[0]
    if length == 0:
        return LimiterResult(array.copy(), np.ones(0), 0, lookahead_samples, True, 1.0)
    limit_linear = 10.0 ** (limit_dbtp / 20.0)
    ceiling_scale = 0.94
    final_audio = array.copy()
    final_gain = np.ones(length, dtype=np.float64)
    for _ in range(4):
        oversampled = _oversample_4x(array)
        peaks = np.max(np.abs(oversampled.reshape(length, 4, -1)), axis=(1, 2))
        required = np.minimum(1.0, (limit_linear * ceiling_scale) / np.maximum(peaks, 1e-24))
        anticipated = _moving_min_future(required, lookahead_samples)
        gain = np.empty(length, dtype=np.float64)
        current = 1.0
        release = math.exp(-1.0 / max(1.0, release_s * sample_rate))
        for index in range(length):
            target = float(anticipated[index])
            if target < current:
                current = target
            else:
                current = target + (current - target) * release
            gain[index] = current
        delayed_signal = np.pad(array, ((lookahead_samples, 0), (0, 0)))
        delayed_gain = np.pad(gain, (lookahead_samples, 0), constant_values=1.0)
        candidate = (
            delayed_signal * delayed_gain[:, None]
        )[lookahead_samples:]
        if true_peak_linear(candidate) <= limit_linear * (1.0 + 1e-9):
            final_audio = candidate
            final_gain = gain
            break
        measured = true_peak_linear(candidate)
        ceiling_scale *= max(0.70, limit_linear / max(measured, 1e-24)) * 0.995
        final_audio = candidate
        final_gain = gain
    return LimiterResult(
        audio=final_audio,
        gain=final_gain,
        triggered_samples=int(np.count_nonzero(final_gain < 1.0 - 1e-9)),
        lookahead_samples=lookahead_samples,
        delay_compensated=True,
        ceiling_scale=float(ceiling_scale),
    )


@dataclass
class LoudnessResult:
    audio: np.ndarray
    measured_lufs: float
    target_lufs: float
    delta_lu: float
    true_peak_dbtp: float
    master_gain_db: float
    limiter: LimiterResult


def finalize_loudness(
    pre_master: np.ndarray,
    sample_rate: int,
    target_lufs: float,
    true_peak_limit_dbtp: float,
) -> LoudnessResult:
    base = np.asarray(pre_master, dtype=np.float64)
    if base.ndim == 1:
        base = base[:, None]
    initial_lufs = integrated_loudness(base, sample_rate)
    if not math.isfinite(initial_lufs):
        raise MasterError("E_SILENT_MASTER", "The mixed master has no measurable loudness.")
    gain_db = target_lufs - initial_lufs
    best: LoudnessResult | None = None
    best_error = math.inf
    for _ in range(7):
        trial = base * (10.0 ** (gain_db / 20.0))
        limited = true_peak_limit(
            trial,
            sample_rate,
            true_peak_limit_dbtp,
        )
        measured = integrated_loudness(limited.audio, sample_rate)
        delta = target_lufs - measured
        result = LoudnessResult(
            audio=limited.audio,
            measured_lufs=float(measured),
            target_lufs=float(target_lufs),
            delta_lu=float(delta),
            true_peak_dbtp=true_peak_dbtp(limited.audio),
            master_gain_db=float(gain_db),
            limiter=limited,
        )
        if abs(delta) < best_error:
            best = result
            best_error = abs(delta)
        if abs(delta) <= 0.04:
            break
        gain_db += delta
    assert best is not None
    if abs(best.delta_lu) > 0.5:
        raise MasterError(
            "E_LOUDNESS_UNACHIEVABLE",
            "Cannot reach the requested integrated loudness while honoring the true-peak limit.",
            details={
                "target_lufs": target_lufs,
                "measured_lufs": best.measured_lufs,
                "true_peak_limit_dbtp": true_peak_limit_dbtp,
                "limiter_triggered_samples": best.limiter.triggered_samples,
            },
        )
    if best.true_peak_dbtp > true_peak_limit_dbtp + 0.01:
        raise MasterError(
            "E_TRUE_PEAK_EXCEEDED",
            "Rendered master exceeds the declared true-peak limit.",
            details={
                "limit_dbtp": true_peak_limit_dbtp,
                "measured_dbtp": best.true_peak_dbtp,
            },
        )
    return best


def energy_fractions(
    stems: dict[str, np.ndarray],
    gain_envelope: np.ndarray,
) -> dict[str, float]:
    energies: dict[str, float] = {}
    for voice, stem in stems.items():
        array = np.asarray(stem, dtype=np.float64)
        if array.ndim == 1:
            array = array[:, None]
        weighted = array * gain_envelope[:, None]
        energies[voice] = float(np.sum(weighted**2))
    total = sum(energies.values())
    if total <= 1e-30:
        return {voice: 0.0 for voice in stems}
    return {voice: value / total for voice, value in energies.items()}

