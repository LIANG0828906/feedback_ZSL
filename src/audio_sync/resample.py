"""Deterministic fractional-delay compensation with selectable interpolation."""

from __future__ import annotations

import numpy as np

from .errors import E_COMPENSATION_FAILED, SyncError
from .models import DriftModel

_EPS = 1e-12


def _reflect_to_int(indices: np.ndarray, length: int) -> np.ndarray:
    if length <= 1:
        return np.zeros_like(indices, dtype=np.int64)
    wrapped = np.mod(indices, 2 * length)
    reflected = np.where(wrapped >= length, 2 * length - 1 - wrapped, wrapped)
    return reflected.astype(np.int64, copy=False)


def _linear_interpolate(signal: np.ndarray, positions: np.ndarray) -> np.ndarray:
    base = np.floor(positions).astype(np.int64)
    fraction = positions - base
    left = signal[_reflect_to_int(base, signal.size)]
    right = signal[_reflect_to_int(base + 1, signal.size)]
    return left + fraction * (right - left)


def _windowed_sinc_interpolate(
    signal: np.ndarray,
    positions: np.ndarray,
    half_taps: int,
) -> np.ndarray:
    base = np.floor(positions).astype(np.int64)
    output = np.zeros(positions.size, dtype=np.float64)
    weight_total = np.zeros(positions.size, dtype=np.float64)
    for tap in range(-half_taps + 1, half_taps + 1):
        distance = positions - (base + tap)
        weights = np.sinc(distance) * np.sinc(distance / float(half_taps))
        samples = signal[_reflect_to_int(base + tap, signal.size)]
        output += weights * samples
        weight_total += weights
    return output / np.maximum(np.abs(weight_total), _EPS)


def compensate_segment(
    signal_i16: np.ndarray,
    model: DriftModel,
    global_start_sample: int,
    interpolation: str,
    sinc_half_taps: int = 8,
    chunk_samples: int = 262_144,
) -> np.ndarray:
    """Apply ``y[n] = x[n + delay(n)]`` and return an int16 segment."""

    signal = np.asarray(signal_i16, dtype=np.float64) / 32768.0
    if signal.ndim != 1:
        raise SyncError(E_COMPENSATION_FAILED, "compensation expects one channel")
    if interpolation not in {"linear", "windowed_sinc"}:
        raise SyncError(E_COMPENSATION_FAILED, f"unknown interpolation mode: {interpolation}")

    output = np.empty(signal.size, dtype=np.float64)
    try:
        for start in range(0, signal.size, max(1, chunk_samples)):
            stop = min(signal.size, start + chunk_samples)
            global_indices = np.arange(
                global_start_sample + start,
                global_start_sample + stop,
                dtype=np.float64,
            )
            # The drift model is a function of absolute recording time, while
            # interpolation indexes into the current segment buffer.
            positions = (
                global_indices
                + np.asarray(model.delay_at(global_indices))
                - float(global_start_sample)
            )
            if interpolation == "linear":
                corrected = _linear_interpolate(signal, positions)
            else:
                corrected = _windowed_sinc_interpolate(signal, positions, sinc_half_taps)
            output[start:stop] = corrected
    except SyncError:
        raise
    except Exception as exc:
        raise SyncError(
            E_COMPENSATION_FAILED,
            f"fractional-delay interpolation failed: {exc}",
        ) from exc

    quantized = np.rint(np.clip(output, -1.0, 32767.0 / 32768.0) * 32768.0)
    return np.clip(quantized, -32768, 32767).astype(np.int16)


def compensate_array(
    signal_i16: np.ndarray,
    model: DriftModel,
    interpolation: str,
    sinc_half_taps: int = 8,
) -> np.ndarray:
    return compensate_segment(
        signal_i16,
        model,
        global_start_sample=0,
        interpolation=interpolation,
        sinc_half_taps=sinc_half_taps,
    )
