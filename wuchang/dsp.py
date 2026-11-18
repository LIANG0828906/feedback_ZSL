"""Small, dependency-light DSP primitives used by the renderer."""

from __future__ import annotations

import math

import numpy as np


def db_to_linear(value_db: float) -> float:
    return float(10.0 ** (value_db / 20.0))


def linear_to_db(value: float, floor_db: float = -160.0) -> float:
    if not math.isfinite(value) or value <= 0.0:
        return floor_db
    return float(max(floor_db, 20.0 * math.log10(value)))


def rms(values: np.ndarray) -> float:
    if values.size == 0:
        return 0.0
    data = np.asarray(values, dtype=np.float64)
    return float(np.sqrt(np.mean(data * data)))


def peak(values: np.ndarray) -> float:
    if values.size == 0:
        return 0.0
    return float(np.max(np.abs(values)))


def shift_samples(signal: np.ndarray, amount: int) -> np.ndarray:
    """Shift by an integer number of samples, preserving length.

    Positive amounts delay the signal; negative amounts advance it. Vacated
    samples are zero-filled.
    """

    data = np.asarray(signal)
    if amount == 0:
        return data.copy()
    output = np.zeros_like(data)
    if amount > 0:
        if amount < data.shape[0]:
            output[amount:] = data[:-amount]
    else:
        amount = -amount
        if amount < data.shape[0]:
            output[:-amount] = data[amount:]
    return output


def reflection_decay_envelope(
    length: int,
    window_start: int,
    window_end: int,
) -> np.ndarray:
    """Deterministic decay envelope derived from the reference window."""

    span = max(1, int(window_end) - int(window_start))
    positions = np.arange(max(0, int(length)), dtype=np.float64)
    return np.exp(-3.0 * positions / float(span))


def _moving_average(values: np.ndarray, width: int) -> np.ndarray:
    if width <= 1:
        return values.copy()
    kernel = np.ones(width, dtype=np.float64) / float(width)
    return np.convolve(values, kernel, mode="same")


def estimate_onset(
    signal: np.ndarray,
    center: int,
    search_before: int,
    search_after: int,
) -> int | None:
    """Find a sharp attack sample near a nominal position.

    A derivative-domain detector is intentionally used instead of an absolute
    amplitude gate: long gong tails can remain louder than a new quiet hit,
    while the new hit still owns the sharpest rising edge in the search window.
    """

    data = np.asarray(signal, dtype=np.float64)
    if data.size == 0:
        return None
    low = max(0, int(center) - max(0, int(search_before)))
    high = min(data.size, int(center) + max(1, int(search_after)))
    if high - low < 4:
        return None
    padded_low = max(0, low - 1)
    segment = data[padded_low:high]
    derivative = np.abs(np.diff(segment, prepend=segment[0]))
    local_peak = float(np.max(derivative))
    if not math.isfinite(local_peak) or local_peak <= 1.0e-9:
        return None

    threshold = local_peak * 0.07
    candidates = np.flatnonzero(derivative >= threshold)
    if candidates.size == 0:
        return None
    onset = padded_low + int(candidates[0])
    if onset < low or onset >= high:
        return None
    return int(onset)


def apply_lookahead_limiter(
    stereo: np.ndarray,
    ceiling_dbtp: float,
    sample_rate: int,
    lookahead_ms: float = 5.0,
) -> tuple[np.ndarray, dict[str, float | int | bool]]:
    """Linked stereo lookahead limiter with explicit delay compensation.

    The gain envelope looks ahead of the source and is applied to the
    corresponding undelayed source sample. The lookahead buffer's processing
    delay is therefore removed exactly and the output frame count is unchanged.
    """

    data = np.asarray(stereo, dtype=np.float64)
    if data.ndim != 2 or data.shape[1] != 2:
        raise ValueError("limiter expects an (N, 2) stereo array")
    lookahead = max(1, round(sample_rate * lookahead_ms / 1000.0))
    ceiling = db_to_linear(ceiling_dbtp)
    peak_per_sample = np.max(np.abs(data), axis=1)
    desired = np.ones_like(peak_per_sample)
    active = peak_per_sample > ceiling
    desired[active] = ceiling / peak_per_sample[active]

    # Offline lookahead: each sample sees the minimum required gain over its
    # future window. This is the delay-compensated form of a lookahead buffer.
    from collections import deque

    target = np.ones_like(desired)
    queue: deque[int] = deque()
    added = 0
    for index in range(desired.size):
        future_end = min(desired.size, index + lookahead)
        while added < future_end:
            while queue and desired[queue[-1]] >= desired[added]:
                queue.pop()
            queue.append(added)
            added += 1
        while queue and queue[0] < index:
            queue.popleft()
        target[index] = desired[queue[0]]

    # A short release ramp prevents zipper noise while retaining an immediate
    # downward attack. Downward movement is never slowed.
    release_coefficient = math.exp(
        -1.0 / max(1.0, float(sample_rate) * 0.080)
    )
    gain = np.ones_like(target)
    previous = 1.0
    for index, value in enumerate(target):
        if value <= previous:
            smoothed = value
        else:
            smoothed = release_coefficient * previous + (
                1.0 - release_coefficient
            ) * value
        gain[index] = min(value, smoothed)
        previous = gain[index]

    output = data * gain[:, None]
    attenuation_db = -20.0 * np.log10(np.maximum(gain, 1.0e-12))
    meaningful = attenuation_db > 1.0e-4
    triggered = int(np.count_nonzero(meaningful))
    max_attenuation = (
        float(np.max(attenuation_db[meaningful])) if triggered else 0.0
    )
    info: dict[str, float | int | bool] = {
        "triggered": bool(triggered > 0),
        "triggered_samples": triggered,
        "max_attenuation_db": max_attenuation,
        "lookahead_samples": int(lookahead),
        "compensated_delay_samples": int(lookahead),
        "ceiling_dbtp": float(ceiling_dbtp),
    }
    return output, info


def biquad(
    signal: np.ndarray,
    b: tuple[float, float, float],
    a: tuple[float, float, float],
) -> np.ndarray:
    """Direct-form-I IIR filtering without a SciPy dependency."""

    x = np.asarray(signal, dtype=np.float64)
    y = np.empty_like(x)
    x1 = x2 = y1 = y2 = 0.0
    b0, b1, b2 = b
    _, a1, a2 = a
    for index, sample in enumerate(x):
        value = b0 * sample + b1 * x1 + b2 * x2 - a1 * y1 - a2 * y2
        y[index] = value
        x2, x1 = x1, sample
        y2, y1 = y1, value
    return y


def k_weighting_coefficients(sample_rate: int) -> tuple[
    tuple[float, float, float],
    tuple[float, float, float],
    tuple[float, float, float],
    tuple[float, float, float],
]:
    """BS.1770 K-weighting coefficients for an arbitrary sample rate."""

    # Stage 1: high-shelf pre-filter.
    frequency = 1681.974450955533
    gain_db = 3.999843853973347
    q = 0.7071752369554196
    k = math.tan(math.pi * frequency / sample_rate)
    vh = 10.0 ** (gain_db / 20.0)
    vb = vh**0.4996667741545416
    a0 = 1.0 + k / q + k * k
    shelf_b = (
        (vh + vb * k / q + k * k) / a0,
        2.0 * (k * k - vh) / a0,
        (vh - vb * k / q + k * k) / a0,
    )
    shelf_a = (
        1.0,
        2.0 * (k * k - 1.0) / a0,
        (1.0 - k / q + k * k) / a0,
    )

    # Stage 2: RLB high-pass.
    frequency = 38.13547087602444
    q = 0.5003270373238773
    k = math.tan(math.pi * frequency / sample_rate)
    a0 = 1.0 + k / q + k * k
    highpass_b = (1.0, -2.0, 1.0)
    highpass_a = (
        1.0,
        2.0 * (k * k - 1.0) / a0,
        (1.0 - k / q + k * k) / a0,
    )
    return shelf_b, shelf_a, highpass_b, highpass_a


def k_weight(signal: np.ndarray, sample_rate: int) -> np.ndarray:
    shelf_b, shelf_a, highpass_b, highpass_a = k_weighting_coefficients(
        sample_rate
    )
    return biquad(biquad(signal, shelf_b, shelf_a), highpass_b, highpass_a)


def integrated_loudness_lufs(stereo: np.ndarray, sample_rate: int) -> float:
    """ITU-R BS.1770-4 integrated loudness for a stereo programme."""

    data = np.asarray(stereo, dtype=np.float64)
    if data.ndim != 2 or data.shape[1] != 2:
        raise ValueError("integrated loudness expects an (N, 2) array")
    if data.shape[0] < int(0.4 * sample_rate):
        raise ValueError("programme must be at least 400 ms")

    weighted = np.column_stack(
        [k_weight(data[:, channel], sample_rate) for channel in range(2)]
    )
    block_size = round(0.400 * sample_rate)
    hop = round(0.100 * sample_rate)
    energies: list[float] = []
    for start in range(0, weighted.shape[0] - block_size + 1, hop):
        block = weighted[start : start + block_size]
        energy = float(np.mean(np.sum(block * block, axis=1)))
        energies.append(energy)
    if not energies:
        raise ValueError("no gating blocks available")
    powers = np.asarray(energies, dtype=np.float64)
    with np.errstate(divide="ignore"):
        loudness = -0.691 + 10.0 * np.log10(np.maximum(powers, 1.0e-30))
    absolute = powers[loudness > -70.0]
    if absolute.size == 0:
        return -70.0
    relative_threshold = (
        -0.691 + 10.0 * math.log10(float(np.mean(absolute))) - 10.0
    )
    absolute_loudness = -0.691 + 10.0 * np.log10(
        np.maximum(absolute, 1.0e-30)
    )
    gated = absolute[absolute_loudness > relative_threshold]
    if gated.size == 0:
        gated = absolute
    integrated = -0.691 + 10.0 * math.log10(float(np.mean(gated)))
    return float(integrated)


def _true_peak_phases() -> list[np.ndarray]:
    oversampling = 4
    taps = 16
    phases: list[np.ndarray] = []
    for phase in range(1, oversampling):
        offsets = np.arange(-taps + 1, taps + 1, dtype=np.float64)
        fraction = phase / float(oversampling)
        kernel = np.sinc(offsets - fraction)
        window = np.kaiser(kernel.size, 8.0)
        kernel *= window
        kernel /= np.sum(kernel)
        phases.append(kernel)
    return phases


_TRUE_PEAK_PHASES = _true_peak_phases()


def true_peak_dbtp(stereo: np.ndarray) -> float:
    """4x oversampled true-peak estimate per channel."""

    data = np.asarray(stereo, dtype=np.float64)
    if data.ndim != 2:
        raise ValueError("true peak expects a two-dimensional array")
    if data.size == 0:
        return -160.0
    maximum = float(np.max(np.abs(data)))
    for channel in range(data.shape[1]):
        signal = data[:, channel]
        for kernel in _TRUE_PEAK_PHASES:
            interpolated = np.convolve(signal, kernel, mode="same")
            maximum = max(maximum, float(np.max(np.abs(interpolated))))
    return linear_to_db(maximum)

