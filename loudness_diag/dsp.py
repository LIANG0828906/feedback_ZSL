"""K-weighting, loudness conversion and 4x true-peak interpolation."""

from __future__ import annotations

import math

import numpy as np

K_WEIGHTING_COEFFICIENTS: tuple[tuple[tuple[float, ...], tuple[float, ...]], ...] = (
    (
        (1.53512485958697, -2.69169618940638, 1.19839281085285),
        (1.0, -1.69065929318241, 0.73248077421585),
    ),
    (
        (1.0, -2.0, 1.0),
        (1.0, -1.99004745483398, 0.99007225036621),
    ),
)

_MIN_LOUDNESS = -math.inf


def _biquad(signal: np.ndarray, b: tuple[float, ...], a: tuple[float, ...]) -> np.ndarray:
    """Small NumPy-only direct-form-I biquad with deterministic state handling."""

    values = np.asarray(signal, dtype=np.float64)
    output = np.empty_like(values)
    x1 = x2 = y1 = y2 = 0.0
    b0, b1, b2 = b
    _, a1, a2 = a
    for index in range(values.size):
        x0 = float(values[index])
        y0 = b0 * x0 + b1 * x1 + b2 * x2 - a1 * y1 - a2 * y2
        output[index] = y0
        x2, x1 = x1, x0
        y2, y1 = y1, y0
    return output


def k_weight(signal: np.ndarray) -> np.ndarray:
    """Apply both BS.1770 K-weighting stages to a mono signal."""

    result = np.asarray(signal, dtype=np.float64)
    for b, a in K_WEIGHTING_COEFFICIENTS:
        result = _biquad(result, b, a)
    return result


def loudness_to_energy(loudness_lufs: float) -> float:
    return 10.0 ** ((float(loudness_lufs) + 0.691) / 10.0)


def energy_to_loudness(energy: float) -> float | None:
    value = float(energy)
    if not math.isfinite(value) or value <= 0.0:
        return None
    return -0.691 + 10.0 * math.log10(value)


def weighted_downmix(samples: np.ndarray) -> np.ndarray:
    """Create a diagnostic mono waveform from 5.1 channels.

    BS.1770 gains are power gains, so their amplitude equivalent is sqrt(G).
    This waveform is for diagnostics only. Integrated energy is always computed
    channel-by-channel as sum(G_i * mean(x_i^2)) to avoid cross-channel terms.
    """

    values = np.asarray(samples, dtype=np.float64)
    if values.ndim != 2 or values.shape[1] != 6:
        raise ValueError("weighted_downmix expects an (frames, 6) array")
    gains = np.asarray([1.0, 1.0, 1.0, 0.0, math.sqrt(1.41), math.sqrt(1.41)])
    return values @ gains


def _interpolation_kernel(phase: int, factor: int, taps: int) -> np.ndarray:
    half = taps // 2
    offsets = np.arange(-half, taps - half, dtype=np.float64)
    fractional = phase / float(factor)
    kernel = np.sinc(fractional - offsets)
    window = 0.54 + 0.46 * np.cos(np.pi * offsets / half)
    kernel *= window
    total = float(np.sum(kernel))
    if total != 0.0:
        kernel /= total
    return kernel


def oversampled_peak_envelope(
    signal: np.ndarray, *, factor: int = 4, taps: int = 32
) -> np.ndarray:
    """Band-limited interpolation at ``factor`` times the input sample rate.

    Phase zero is an exact copy of the input samples. Other phases use a
    deterministic windowed-sinc FIR with edge extension. The return layout is
    ``[x0, x0+1/f, ..., x1, x1+1/f, ...]``.
    """

    if factor < 1:
        raise ValueError("oversampling factor must be >= 1")
    if taps < 8 or taps % 2 != 0:
        raise ValueError("tap count must be an even integer >= 8")
    values = np.asarray(signal, dtype=np.float64)
    if values.ndim != 1:
        raise ValueError("oversampled_peak_envelope expects a mono signal")
    if factor == 1 or values.size == 0:
        return values.copy()

    half = taps // 2
    padded = np.pad(values, (half, half), mode="edge")
    phases = [values]
    for phase in range(1, factor):
        kernel = _interpolation_kernel(phase, factor, taps)
        phase_values = np.zeros(values.size, dtype=np.float64)
        for offset_index, offset in enumerate(range(-half, taps - half)):
            phase_values += (
                padded[half + offset : half + offset + values.size]
                * kernel[offset_index]
            )
        phases.append(phase_values)
    return np.stack(phases, axis=1).reshape(-1)


def true_peak_4x(signal: np.ndarray) -> float:
    envelope = oversampled_peak_envelope(signal, factor=4)
    return float(np.max(np.abs(envelope))) if envelope.size else 0.0

