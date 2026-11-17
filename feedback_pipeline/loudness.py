"""BS.1770-style integrated loudness and true-peak measurement."""

from __future__ import annotations

import numpy as np
from scipy.signal import lfilter, resample_poly

# De Man's K-weighting filter constants (valid for any sample rate).
_SHELF_F0 = 1681.974450955533
_SHELF_G = 3.999843853973347
_SHELF_Q = 0.7071752369554196
_HP_F0 = 38.13547087602444
_HP_Q = 0.5003270373238773


def _high_shelf(sr: int, f0: float, gain_db: float, q: float) -> tuple[np.ndarray, np.ndarray]:
    A = 10.0 ** (gain_db / 40.0)
    w0 = 2.0 * np.pi * f0 / sr
    alpha = np.sin(w0) / (2.0 * q)
    cosw = np.cos(w0)
    sq = 2.0 * np.sqrt(A) * alpha
    b0 = A * ((A + 1.0) + (A - 1.0) * cosw + sq)
    b1 = -2.0 * A * ((A - 1.0) + (A + 1.0) * cosw)
    b2 = A * ((A + 1.0) + (A - 1.0) * cosw - sq)
    a0 = (A + 1.0) - (A - 1.0) * cosw + sq
    a1 = 2.0 * ((A - 1.0) - (A + 1.0) * cosw)
    a2 = (A + 1.0) - (A - 1.0) * cosw - sq
    return np.array([b0, b1, b2]) / a0, np.array([1.0, a1 / a0, a2 / a0])


def _high_pass(sr: int, f0: float, q: float) -> tuple[np.ndarray, np.ndarray]:
    w0 = 2.0 * np.pi * f0 / sr
    alpha = np.sin(w0) / (2.0 * q)
    cosw = np.cos(w0)
    b0 = (1.0 + cosw) / 2.0
    b1 = -(1.0 + cosw)
    b2 = (1.0 + cosw) / 2.0
    a0 = 1.0 + alpha
    a1 = -2.0 * cosw
    a2 = 1.0 - alpha
    return np.array([b0, b1, b2]) / a0, np.array([1.0, a1 / a0, a2 / a0])


def k_weight(x: np.ndarray, sr: int) -> np.ndarray:
    """Apply the two K-weighting biquad stages to a mono signal."""
    b, a = _high_shelf(sr, _SHELF_F0, _SHELF_G, _SHELF_Q)
    y = lfilter(b, a, x)
    b, a = _high_pass(sr, _HP_F0, _HP_Q)
    return lfilter(b, a, y)


def integrated_loudness(channels: list[np.ndarray], sr: int) -> float:
    """Gated integrated loudness (LUFS) of the given full-range channels."""
    block = int(0.4 * sr)
    hop = int(0.1 * sr)
    filtered = [k_weight(ch, sr) for ch in channels]
    n = min(len(ch) for ch in filtered)
    if n < block:
        block = max(1, n)
        hop = max(1, n)
    starts = list(range(0, n - block + 1, hop))
    if not starts:
        starts = [0]
    z = []
    for s in starts:
        e = s + block
        z.append(sum(float(np.mean(ch[s:e] ** 2)) for ch in filtered))
    z = np.asarray(z)
    # Absolute gate at -70 LUFS.
    keep = z > 10.0 ** ((-70.0 + 0.691) / 10.0)
    if not keep.any():
        return -70.0
    z_abs = z[keep]
    l_abs = -0.691 + 10.0 * np.log10(np.mean(z_abs))
    # Relative gate 10 LU below the absolute-gated loudness.
    rel_thresh = 10.0 ** ((l_abs - 10.0 + 0.691) / 10.0)
    z_rel = z_abs[z_abs > rel_thresh]
    if z_rel.size == 0:
        return l_abs
    return float(-0.691 + 10.0 * np.log10(np.mean(z_rel)))


def true_peak(channels: list[np.ndarray], sr: int, oversample: int = 4) -> float:
    """True peak in dBTP via polyphase oversampling."""
    peak = 0.0
    for ch in channels:
        up = resample_poly(ch, oversample, 1)
        peak = max(peak, float(np.max(np.abs(up))))
    if peak <= 0.0:
        return -np.inf
    return 20.0 * np.log10(peak)
