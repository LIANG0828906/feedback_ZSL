"""Vibration-coupled feedback source tracing.

The monitor reference track carries the narrowband resonance component that
the stage vibration couples into the microphone batch.  Every mix channel is
compared against the reference: a channel is flagged when it contains a
narrowband component at the reference fundamental whose amplitude envelope
tracks the reference envelope and which forms a prominent spectral peak.
"""

from __future__ import annotations

import numpy as np
from scipy.signal import welch

from .dsp import (
    bandpass_zero_phase,
    find_segments,
    moving_rms,
    parabolic_peak,
    pearson,
    spectrum,
)

SEARCH_MIN_HZ = 60.0
SEARCH_MAX_HZ = 400.0
BAND_HALF_WIDTH_HZ = 6.0
CORR_THRESHOLD = 0.65
PROMINENCE_THRESHOLD_DB = 8.0
SEGMENT_THRESHOLD = 0.25


def reference_fundamental(ref: np.ndarray, sr: int) -> float:
    freqs, mag = spectrum(ref, sr)
    bin_hz = float(freqs[1] - freqs[0])
    lo = int(SEARCH_MIN_HZ / bin_hz)
    hi = int(SEARCH_MAX_HZ / bin_hz)
    idx = lo + int(np.argmax(mag[lo:hi]))
    f0, _ = parabolic_peak(mag, idx, bin_hz)
    return f0


def _prominence_db(ch: np.ndarray, sr: int, f0: float) -> float:
    f, p = welch(ch, fs=sr, nperseg=16384)
    peak_idx = int(np.argmin(np.abs(f - f0)))
    peak = float(p[peak_idx])
    neighbor = ((f >= f0 - 45.0) & (f <= f0 - 12.0)) | ((f >= f0 + 12.0) & (f <= f0 + 45.0))
    floor = float(np.median(p[neighbor])) if neighbor.any() else 1e-30
    return 10.0 * np.log10(max(peak, 1e-30) / max(floor, 1e-30))


def _bandwidth_hz(mag: np.ndarray, bin_hz: float, idx: int) -> float:
    peak = mag[idx]
    thresh = peak / np.sqrt(2.0)
    lo = idx
    while lo > 0 and mag[lo] > thresh:
        lo -= 1
    hi = idx
    while hi < len(mag) - 1 and mag[hi] > thresh:
        hi += 1
    return float((hi - lo) * bin_hz)


def analyze_channel(ch: np.ndarray, ref: np.ndarray, sr: int, f_ref: float) -> dict:
    """Analyse one channel against the reference; returns a result record."""
    ch_band = bandpass_zero_phase(ch, sr, f_ref - BAND_HALF_WIDTH_HZ, f_ref + BAND_HALF_WIDTH_HZ)
    ref_band = bandpass_zero_phase(ref, sr, f_ref - BAND_HALF_WIDTH_HZ, f_ref + BAND_HALF_WIDTH_HZ)
    env_ch = moving_rms(ch_band, sr, 0.1)
    env_ref = moving_rms(ref_band, sr, 0.1)
    corr = pearson(env_ch, env_ref)
    prominence = _prominence_db(ch, sr, f_ref)
    detected = bool(corr >= CORR_THRESHOLD and prominence >= PROMINENCE_THRESHOLD_DB)

    freqs, mag = spectrum(ch, sr)
    bin_hz = float(freqs[1] - freqs[0])
    idx = int(np.argmin(np.abs(freqs - f_ref)))
    lo = max(1, idx - int(4.0 / bin_hz))
    hi = min(len(mag) - 2, idx + int(4.0 / bin_hz))
    peak_idx = lo + int(np.argmax(mag[lo:hi]))
    f_est, _ = parabolic_peak(mag, peak_idx, bin_hz)
    bandwidth = _bandwidth_hz(mag, bin_hz, peak_idx)

    segments: list[list[int]] = []
    if detected and env_ch.max() > 0:
        mask = env_ch > SEGMENT_THRESHOLD * float(env_ch.max())
        segments = find_segments(mask, sr, min_len_s=0.25, merge_gap_s=0.15)

    confidence = 0.0
    if detected:
        confidence = min(1.0, 0.6 * corr + 0.4 * min(1.0, prominence / 24.0))

    return {
        "detected": detected,
        "fundamental_hz": round(f_est, 4) if detected else None,
        "bandwidth_hz": round(bandwidth, 3) if detected else None,
        "segments_samples": segments if detected else [],
        "envelope_correlation": round(corr, 6),
        "prominence_db": round(prominence, 3),
        "confidence": round(confidence, 4),
    }


def trace_feedback(channels: dict[str, np.ndarray], reference: np.ndarray, sr: int) -> dict:
    """Trace feedback across all mix channels.

    ``channels`` maps channel name -> mono samples (reference excluded).
    Returns a deterministic dict with the reference fundamental and a
    per-channel analysis record.
    """
    f_ref = reference_fundamental(reference, sr)
    results = {}
    for name in sorted(channels):
        results[name] = analyze_channel(channels[name], reference, sr, f_ref)
    return {
        "reference_fundamental_hz": round(f_ref, 4),
        "channels": results,
        "flagged_channels": sorted(n for n, r in results.items() if r["detected"]),
    }
