"""Shared deterministic DSP helpers."""

from __future__ import annotations

import numpy as np
from scipy.signal import fftconvolve, firwin


def bandpass_zero_phase(x: np.ndarray, sr: int, f_lo: float, f_hi: float, numtaps: int = 8193) -> np.ndarray:
    """Zero-phase FIR band-pass implemented with FFT convolution."""
    taps = firwin(numtaps, [f_lo, f_hi], pass_zero=False, fs=sr)
    y = fftconvolve(x, taps, mode="full")
    delay = (numtaps - 1) // 2
    return y[delay : delay + len(x)]


def moving_rms(x: np.ndarray, sr: int, window_s: float) -> np.ndarray:
    win = max(1, int(round(window_s * sr)))
    kernel = np.ones(win, dtype=np.float64) / win
    return np.sqrt(np.maximum(fftconvolve(x * x, kernel, mode="same"), 0.0))


def pearson(a: np.ndarray, b: np.ndarray) -> float:
    a = a - a.mean()
    b = b - b.mean()
    denom = float(np.sqrt(np.dot(a, a) * np.dot(b, b)))
    if denom <= 0.0:
        return 0.0
    return float(np.dot(a, b) / denom)


def parabolic_peak(mag: np.ndarray, idx: int, bin_hz: float) -> tuple[float, float]:
    """Sub-bin peak estimate; returns (frequency_hz, peak_magnitude)."""
    if idx <= 0 or idx >= len(mag) - 1:
        return idx * bin_hz, float(mag[idx])
    y0, y1, y2 = mag[idx - 1], mag[idx], mag[idx + 1]
    denom = y0 - 2.0 * y1 + y2
    if abs(denom) < 1e-18:
        return idx * bin_hz, float(y1)
    shift = 0.5 * (y0 - y2) / denom
    shift = float(np.clip(shift, -1.0, 1.0))
    peak_mag = y1 - 0.25 * (y0 - y2) * shift
    return (idx + shift) * bin_hz, float(peak_mag)


def spectrum(x: np.ndarray, sr: int) -> tuple[np.ndarray, np.ndarray]:
    """Single-sided amplitude spectrum of the whole signal (Hann window)."""
    win = np.hanning(len(x))
    spec = np.abs(np.fft.rfft(x * win))
    freqs = np.fft.rfftfreq(len(x), d=1.0 / sr)
    return freqs, spec


def find_segments(mask: np.ndarray, sr: int, min_len_s: float, merge_gap_s: float) -> list[list[int]]:
    """Boolean mask -> merged [start, end) sample ranges."""
    if not mask.any():
        return []
    edges = np.diff(mask.astype(np.int8))
    starts = list(np.flatnonzero(edges == 1) + 1)
    ends = list(np.flatnonzero(edges == -1) + 1)
    if mask[0]:
        starts.insert(0, 0)
    if mask[-1]:
        ends.append(len(mask))
    segs = [[int(s), int(e)] for s, e in zip(starts, ends)]
    merged: list[list[int]] = []
    gap = int(merge_gap_s * sr)
    for seg in segs:
        if merged and seg[0] - merged[-1][1] <= gap:
            merged[-1][1] = seg[1]
        else:
            merged.append(list(seg))
    min_len = int(min_len_s * sr)
    return [s for s in merged if s[1] - s[0] >= min_len]
