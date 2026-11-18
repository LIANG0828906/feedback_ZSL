from __future__ import annotations

import math

import numpy as np
from scipy.signal import lfilter


# ITU-R BS.1770-4 K-weighting coefficients at 48 kHz.
_SHELF_B_48K = (1.53512485958697, -2.69169618940638, 1.19839281085285)
_SHELF_A_48K = (1.0, -1.69065929318241, 0.73248077421585)
_HIGHPASS_B_48K = (1.0, -2.0, 1.0)
_HIGHPASS_A_48K = (1.0, -1.99004745483398, 0.99007225036621)


def _rbj_high_shelf(sample_rate: int, gain_db: float = 4.0, frequency: float = 1500.0):
    a = 10.0 ** (gain_db / 40.0)
    q = 1.0 / math.sqrt(2.0)
    omega = 2.0 * math.pi * frequency / sample_rate
    cosine = math.cos(omega)
    sine = math.sin(omega)
    alpha = sine / (2.0 * q)
    b0 = a * ((a + 1.0) + (a - 1.0) * cosine + 2.0 * math.sqrt(a) * alpha)
    b1 = -2.0 * a * ((a - 1.0) + (a + 1.0) * cosine)
    b2 = a * ((a + 1.0) + (a - 1.0) * cosine - 2.0 * math.sqrt(a) * alpha)
    a0 = (a + 1.0) - (a - 1.0) * cosine + 2.0 * math.sqrt(a) * alpha
    a1 = 2.0 * ((a - 1.0) - (a + 1.0) * cosine)
    a2 = (a + 1.0) - (a - 1.0) * cosine - 2.0 * math.sqrt(a) * alpha
    return (
        np.asarray([b0, b1, b2], dtype=np.float64) / a0,
        np.asarray([a0, a1, a2], dtype=np.float64) / a0,
    )


def _rbj_high_pass(sample_rate: int, frequency: float = 38.0, q: float = 0.5):
    omega = 2.0 * math.pi * frequency / sample_rate
    cosine = math.cos(omega)
    alpha = math.sin(omega) / (2.0 * q)
    a0 = 1.0 + alpha
    b = np.asarray(
        [(1.0 + cosine) / 2.0, -(1.0 + cosine), (1.0 + cosine) / 2.0],
        dtype=np.float64,
    ) / a0
    a = np.asarray([a0, -2.0 * cosine, 1.0 - alpha], dtype=np.float64) / a0
    return b, a


def k_weighting_coefficients(sample_rate: int):
    if sample_rate == 48000:
        return (
            np.asarray(_SHELF_B_48K, dtype=np.float64),
            np.asarray(_SHELF_A_48K, dtype=np.float64),
            np.asarray(_HIGHPASS_B_48K, dtype=np.float64),
            np.asarray(_HIGHPASS_A_48K, dtype=np.float64),
        )
    shelf_b, shelf_a = _rbj_high_shelf(sample_rate)
    highpass_b, highpass_a = _rbj_high_pass(sample_rate)
    return shelf_b, shelf_a, highpass_b, highpass_a


def k_weight(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    data = np.asarray(samples, dtype=np.float64)
    shelf_b, shelf_a, highpass_b, highpass_a = k_weighting_coefficients(sample_rate)
    filtered = lfilter(shelf_b, shelf_a, data, axis=0)
    return lfilter(highpass_b, highpass_a, filtered, axis=0)


def _block_mean_square(samples: np.ndarray, block: int, hop: int) -> np.ndarray:
    frames = samples.shape[0]
    if frames == 0:
        return np.zeros(0, dtype=np.float64)
    if frames < block:
        padded = np.zeros((block, samples.shape[1]), dtype=np.float64)
        padded[:frames] = samples
        return np.mean(padded * padded, axis=0, keepdims=True)

    starts = np.arange(0, frames - block + 1, hop, dtype=np.int64)
    cumulative = np.concatenate(
        [np.zeros((1, samples.shape[1]), dtype=np.float64), np.cumsum(samples * samples, axis=0)],
        axis=0,
    )
    sums = cumulative[starts + block] - cumulative[starts]
    return sums / float(block)


def integrated_loudness(samples: np.ndarray, sample_rate: int) -> float:
    """ITU-R BS.1770-4 integrated loudness in LUFS, with absolute and relative gating."""
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim != 2 or data.shape[0] == 0:
        return float("-inf")
    weighted = k_weight(data, sample_rate)
    block = max(1, int(round(0.400 * sample_rate)))
    hop = max(1, int(round(0.100 * sample_rate)))
    channel_energy = _block_mean_square(weighted, block, hop)
    if channel_energy.size == 0:
        return float("-inf")
    z = np.sum(channel_energy, axis=1)
    positive = z > 0
    if not np.any(positive):
        return float("-inf")

    loudness = -0.691 + 10.0 * np.log10(z[positive])
    absolute = loudness >= -70.0
    if not np.any(absolute):
        return float("-inf")
    z_absolute = z[positive][absolute]
    loudness_absolute = loudness[absolute]
    relative_threshold = -0.691 + 10.0 * math.log10(float(np.mean(z_absolute))) - 10.0
    relative = loudness_absolute >= relative_threshold
    if not np.any(relative):
        return float("-inf")
    final_energy = float(np.mean(z_absolute[relative]))
    if final_energy <= 0:
        return float("-inf")
    return -0.691 + 10.0 * math.log10(final_energy)
