from __future__ import annotations

import math
from functools import lru_cache

import numpy as np


_ABSOLUTE_GATE_LUFS = -70.0
_RELATIVE_GATE_LU = -10.0


def _high_shelf_coefficients(sample_rate: int) -> tuple[float, float, float, float, float, float]:
    gain_db = 3.999843853973347
    q = 0.7071752369554196
    fc = 1681.974450955533
    amplitude = 10.0 ** (gain_db / 40.0)
    omega = 2.0 * math.pi * fc / sample_rate
    cosine = math.cos(omega)
    sine = math.sin(omega)
    alpha = sine / (2.0 * q)
    sqrt_a = math.sqrt(amplitude)

    b0 = amplitude * (
        (amplitude + 1.0) + (amplitude - 1.0) * cosine + 2.0 * sqrt_a * alpha
    )
    b1 = -2.0 * amplitude * ((amplitude - 1.0) + (amplitude + 1.0) * cosine)
    b2 = amplitude * (
        (amplitude + 1.0) + (amplitude - 1.0) * cosine - 2.0 * sqrt_a * alpha
    )
    a0 = (amplitude + 1.0) - (amplitude - 1.0) * cosine + 2.0 * sqrt_a * alpha
    a1 = 2.0 * ((amplitude - 1.0) - (amplitude + 1.0) * cosine)
    a2 = (amplitude + 1.0) - (amplitude - 1.0) * cosine - 2.0 * sqrt_a * alpha
    return b0 / a0, b1 / a0, b2 / a0, a1 / a0, a2 / a0, 0.0


def _high_pass_coefficients(sample_rate: int) -> tuple[float, float, float, float, float, float]:
    q = 0.5003270373238773
    fc = 38.13547087602444
    omega = 2.0 * math.pi * fc / sample_rate
    cosine = math.cos(omega)
    sine = math.sin(omega)
    alpha = sine / (2.0 * q)

    b0 = (1.0 + cosine) / 2.0
    b1 = -(1.0 + cosine)
    b2 = (1.0 + cosine) / 2.0
    a0 = 1.0 + alpha
    a1 = -2.0 * cosine
    a2 = 1.0 - alpha
    return b0 / a0, b1 / a0, b2 / a0, a1 / a0, a2 / a0, 0.0


def _impulse_response(
    first: tuple[float, float, float, float, float, float],
    second: tuple[float, float, float, float, float, float],
    length: int,
) -> np.ndarray:
    impulse = np.zeros(length, dtype=np.float64)
    impulse[0] = 1.0
    return _biquad(
        _biquad(impulse, first),
        second,
    )


def _biquad(
    samples: np.ndarray,
    coefficients: tuple[float, float, float, float, float, float],
) -> np.ndarray:
    b0, b1, b2, a1, a2, _ = coefficients
    output = np.empty_like(samples, dtype=np.float64)
    z1 = 0.0
    z2 = 0.0
    for index in range(samples.size):
        value = float(samples[index])
        result = b0 * value + z1
        z1 = b1 * value - a1 * result + z2
        z2 = b2 * value - a2 * result
        output[index] = result
    return output


@lru_cache(maxsize=16)
def _k_weighting_cache(sample_rate: int) -> np.ndarray:
    shelf = _high_shelf_coefficients(sample_rate)
    high_pass = _high_pass_coefficients(sample_rate)
    response_length = max(4096, int(round(sample_rate * 2.0)))
    impulse = _impulse_response(shelf, high_pass, response_length)
    impulse.setflags(write=False)
    return impulse


def apply_k_weighting(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    if samples.ndim != 2:
        raise ValueError("samples must have shape (frames, channels)")
    if samples.shape[0] == 0:
        return samples.astype(np.float64, copy=True)
    impulse = _k_weighting_cache(sample_rate)
    frame_count = samples.shape[0]
    fft_size = 1 << (frame_count + impulse.size - 1).bit_length()
    spectrum = np.fft.rfft(impulse, n=fft_size)
    output = np.empty_like(samples, dtype=np.float64)
    for channel in range(samples.shape[1]):
        source = samples[:, channel].astype(np.float64, copy=False)
        transformed = np.fft.rfft(source, n=fft_size) * spectrum
        filtered = np.fft.irfft(transformed, n=fft_size)
        output[:, channel] = filtered[:frame_count]
    return output


def mean_square_blocks(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    block_frames = max(1, int(round(sample_rate * 0.4)))
    hop_frames = max(1, int(round(sample_rate * 0.1)))
    if samples.shape[0] < block_frames:
        padded = np.zeros((block_frames, samples.shape[1]), dtype=np.float64)
        padded[: samples.shape[0]] = samples
        samples = padded
    block_count = 1 + (samples.shape[0] - block_frames) // hop_frames
    energy = np.empty(block_count, dtype=np.float64)
    squared = samples * samples
    for index in range(block_count):
        start = index * hop_frames
        block = squared[start : start + block_frames]
        channel_energy = np.mean(block, axis=0)
        energy[index] = float(np.sum(channel_energy))
    return energy


def integrated_loudness(samples: np.ndarray, sample_rate: int) -> float | None:
    if samples.ndim != 2 or samples.shape[0] == 0:
        return None
    filtered = apply_k_weighting(samples, sample_rate)
    energy = mean_square_blocks(filtered, sample_rate)
    if energy.size == 0 or not np.any(energy > 0.0):
        return None
    with np.errstate(divide="ignore"):
        block_loudness = -0.691 + 10.0 * np.log10(np.maximum(energy, 1e-30))
    absolute_mask = block_loudness > _ABSOLUTE_GATE_LUFS
    if not np.any(absolute_mask):
        return None
    absolute_energy = float(np.mean(energy[absolute_mask]))
    if absolute_energy <= 0.0:
        return None
    relative_gate = -0.691 + 10.0 * math.log10(absolute_energy) + _RELATIVE_GATE_LU
    gated_mask = absolute_mask & (block_loudness > relative_gate)
    if not np.any(gated_mask):
        return None
    gated_energy = float(np.mean(energy[gated_mask]))
    if gated_energy <= 0.0:
        return None
    return -0.691 + 10.0 * math.log10(gated_energy)


def active_ratio(
    samples: np.ndarray,
    sample_rate: int,
    threshold_dbfs: float,
    frame_ms: float,
) -> float:
    frame_frames = max(1, int(round(sample_rate * frame_ms / 1000.0)))
    frame_count = int(math.ceil(samples.shape[0] / frame_frames))
    if frame_count == 0:
        return 0.0
    threshold = 10.0 ** (threshold_dbfs / 20.0)
    active = 0
    for index in range(frame_count):
        frame = samples[index * frame_frames : (index + 1) * frame_frames]
        if frame.size == 0:
            continue
        peak = float(np.max(np.abs(frame)))
        if peak > threshold:
            active += 1
    return active / float(frame_count)

