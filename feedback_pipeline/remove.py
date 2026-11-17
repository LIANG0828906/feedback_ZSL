"""Narrowband feedback removal on the flagged channels.

The reference track holds an exact (scaled) copy of the injected resonance,
so a least-squares gain estimated inside the detected segments lets us
subtract the component with a very deep residual null.  Samples outside the
declared segments and channels that were not flagged are left bit-identical.
"""

from __future__ import annotations

import numpy as np

from .dsp import bandpass_zero_phase

BAND_HALF_WIDTH_HZ = 6.0
FADE_S = 0.005


def estimate_gain(ch_band: np.ndarray, ref_band: np.ndarray, segments: list[list[int]]) -> float:
    num = 0.0
    den = 0.0
    for s, e in segments:
        num += float(np.dot(ch_band[s:e], ref_band[s:e]))
        den += float(np.dot(ref_band[s:e], ref_band[s:e]))
    if den <= 0.0:
        return 0.0
    return num / den


def remove_feedback(ch: np.ndarray, ref: np.ndarray, sr: int, f_est: float,
                    segments: list[list[int]]) -> tuple[np.ndarray, float]:
    """Return (processed, gain). Only samples inside segments are modified."""
    ch_band = bandpass_zero_phase(ch, sr, f_est - BAND_HALF_WIDTH_HZ, f_est + BAND_HALF_WIDTH_HZ)
    ref_band = bandpass_zero_phase(ref, sr, f_est - BAND_HALF_WIDTH_HZ, f_est + BAND_HALF_WIDTH_HZ)
    gain = estimate_gain(ch_band, ref_band, segments)
    corrected = ch - gain * ref_band

    out = ch.copy()
    fade = max(1, int(FADE_S * sr))
    for s, e in segments:
        out[s:e] = corrected[s:e]
        if e - s <= 2 * fade:
            continue
        ramp = 0.5 - 0.5 * np.cos(np.linspace(0.0, np.pi, fade))
        out[s:s + fade] = (1.0 - ramp) * ch[s:s + fade] + ramp * corrected[s:s + fade]
        out[e - fade:e] = ramp * ch[e - fade:e] + (1.0 - ramp) * corrected[e - fade:e]
    return out, gain


def narrowband_energy(x: np.ndarray, sr: int, f_est: float, segments: list[list[int]]) -> float:
    """Energy of the narrowband component inside the given segments."""
    band = bandpass_zero_phase(x, sr, f_est - BAND_HALF_WIDTH_HZ, f_est + BAND_HALF_WIDTH_HZ)
    total = 0.0
    for s, e in segments:
        total += float(np.dot(band[s:e], band[s:e]))
    return total
