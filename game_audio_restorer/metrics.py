"""Deterministic audio metrics used by reports and version comparison."""

from __future__ import annotations

import math
from typing import Any

import numpy as np
from scipy.signal import lfilter

_SILENCE_DB = -120.0


def _as_frames(samples: np.ndarray) -> np.ndarray:
    arr = np.asarray(samples, dtype=np.float64)
    if arr.ndim == 1:
        arr = arr[:, None]
    if arr.ndim != 2:
        raise ValueError("audio must be frames x channels")
    return arr


def rms_dbfs(samples: np.ndarray) -> float:
    arr = _as_frames(samples)
    if arr.size == 0:
        return _SILENCE_DB
    rms = float(np.sqrt(np.mean(np.square(arr), dtype=np.float64)))
    if rms <= 1e-12:
        return _SILENCE_DB
    return max(_SILENCE_DB, 20.0 * math.log10(rms))


def peak_dbfs(samples: np.ndarray) -> float:
    arr = _as_frames(samples)
    if arr.size == 0:
        return _SILENCE_DB
    peak = float(np.max(np.abs(arr)))
    if peak <= 1e-12:
        return _SILENCE_DB
    return max(_SILENCE_DB, 20.0 * math.log10(peak))


def _k_weight_coefficients(sample_rate: int) -> tuple[np.ndarray, ...]:
    """ITU-R BS.1770 K-weighting biquads evaluated at the native rate."""

    fs = float(sample_rate)
    f0 = 1681.974450955533
    gain_db = 3.999843853973347
    q = 0.7071752369554196
    k = math.tan(math.pi * f0 / fs)
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
    k = math.tan(math.pi * f0 / fs)
    a0 = 1.0 + k / q + k * k
    highpass_b = np.array([1.0 / a0, -2.0 / a0, 1.0 / a0])
    highpass_a = np.array(
        [1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0]
    )
    return shelf_b, shelf_a, highpass_b, highpass_a


def k_weight(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    arr = _as_frames(samples)
    shelf_b, shelf_a, highpass_b, highpass_a = _k_weight_coefficients(sample_rate)
    filtered = lfilter(shelf_b, shelf_a, arr, axis=0)
    filtered = lfilter(highpass_b, highpass_a, filtered, axis=0)
    return filtered


def integrated_loudness_lufs(samples: np.ndarray, sample_rate: int) -> float:
    """Gated integrated loudness with a deterministic finite silence floor.

    The implementation follows the BS.1770 K-weighting and 400 ms gating
    structure.  Values are clamped to -120 LUFS so JSON never contains -inf.
    """

    arr = _as_frames(samples)
    if arr.size == 0:
        return _SILENCE_DB
    weighted = k_weight(arr, sample_rate)
    block = max(1, int(round(sample_rate * 0.4)))
    if weighted.shape[0] < block:
        blocks = weighted[None, :, :]
    else:
        hop = max(1, int(round(sample_rate * 0.1)))
        starts = list(range(0, weighted.shape[0] - block + 1, hop))
        blocks = np.stack([weighted[start : start + block] for start in starts], axis=0)
    energies = np.mean(np.square(blocks), axis=(1, 2))
    finite = energies[energies > 1e-24]
    if finite.size == 0:
        return _SILENCE_DB
    block_loudness = -0.691 + 10.0 * np.log10(finite)
    absolute_energies = finite[block_loudness > -70.0]
    if absolute_energies.size == 0:
        return _SILENCE_DB
    ungated = -0.691 + 10.0 * math.log10(float(np.mean(absolute_energies)))
    relative_gate = ungated - 10.0
    gated_energies = finite[block_loudness > max(-70.0, relative_gate)]
    if gated_energies.size == 0:
        return _SILENCE_DB
    loudness = -0.691 + 10.0 * math.log10(float(np.mean(gated_energies)))
    return max(_SILENCE_DB, float(loudness))


def residual_noise_dbfs(samples: np.ndarray, sample_rate: int) -> float:
    """Estimate the noise floor as the quietest 10% of 20 ms frames."""

    arr = _as_frames(samples)
    if arr.size == 0:
        return _SILENCE_DB
    frame = max(1, int(round(sample_rate * 0.02)))
    starts = np.arange(0, arr.shape[0], frame)
    energies = []
    for start in starts:
        block = arr[start : start + frame]
        if block.size:
            energies.append(float(np.mean(np.square(block))))
    finite = np.asarray([value for value in energies if value > 1e-24], dtype=np.float64)
    if finite.size == 0:
        return _SILENCE_DB
    floor = float(np.percentile(finite, 10))
    return max(_SILENCE_DB, 10.0 * math.log10(floor))


def audio_metrics(samples: np.ndarray, sample_rate: int) -> dict[str, Any]:
    arr = _as_frames(samples)
    channel_rms = [
        rms_dbfs(arr[:, index : index + 1]) for index in range(arr.shape[1])
    ]
    channel_peak = [
        peak_dbfs(arr[:, index : index + 1]) for index in range(arr.shape[1])
    ]
    return {
        "rms_dbfs": round(rms_dbfs(arr), 6),
        "peak_dbfs": round(peak_dbfs(arr), 6),
        "loudness_lufs": round(integrated_loudness_lufs(arr, sample_rate), 6),
        "residual_noise_dbfs": round(residual_noise_dbfs(arr, sample_rate), 6),
        "channel_rms_dbfs": [round(value, 6) for value in channel_rms],
        "channel_peak_dbfs": [round(value, 6) for value in channel_peak],
    }


def waveform_delta_db(reference: np.ndarray, candidate: np.ndarray) -> float:
    left = _as_frames(reference)
    right = _as_frames(candidate)
    if left.shape != right.shape:
        return 120.0
    delta = left - right
    return rms_dbfs(delta)
