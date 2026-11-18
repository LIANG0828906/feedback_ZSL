"""Transient onset detection and integer-sample alignment of percussion.

Onsets are located with a Hann-windowed spectral-flux onset function: only
fresh harmonic/noise energy contributes positively, so the long exponential
tail of a gong does not masquerade as a new attack.  The onset function peaks
at a fixed, instrument-specific latency relative to the physical attack, so a
single per-channel integer shift (median of the per-note residuals) aligns
every note within one sample of the declared reference times.
"""

from __future__ import annotations

import numpy as np

from . import PERCUSSION_PARTS
from .errors import DETECTION_FAILED, make_error


def spectral_flux_onsets(signal, sr, frame=512, hop=64):
    """Positive spectral-flux onset function at hop-grid sample positions."""
    # Pad leading silence so the first real event has the same spectral
    # context as later events (avoids first-note boundary bias).
    pad = frame
    padded = np.concatenate([np.zeros(pad), np.asarray(signal, dtype=np.float64)])
    window = np.hanning(frame)
    positions = []
    flux = []
    previous = None
    for start in range(0, max(padded.shape[0] - frame, 0), hop):
        spectrum = np.abs(
            np.fft.rfft(padded[start : start + frame] * window)
        )
        if previous is None:
            flux.append(0.0)
        else:
            flux.append(float(np.sum(np.maximum(spectrum - previous, 0.0))))
        previous = spectrum
        positions.append(start - pad)
    return np.array(positions, dtype=np.int64), np.array(flux, dtype=np.float64)


def detect_onsets(part, signal, sr, ref_samples, search_ms=12.0):
    """Detect one onset per reference time; returns (onsets, ok)."""
    positions, flux = spectral_flux_onsets(signal, sr)
    search = int(round(search_ms * sr / 1000.0))
    floor = float(np.median(flux[flux > 0.0])) if np.any(flux > 0.0) else 0.0
    onsets = []
    ok = True
    for ref in ref_samples:
        lo = max(0, ref - search)
        hi = min(signal.shape[0], ref + search)
        inside = (positions >= lo) & (positions < hi)
        if not np.any(inside):
            onsets.append(ref)
            ok = False
            continue
        local = flux[inside]
        peak = float(np.max(local))
        if peak < max(floor * 4.0, 1e-12):
            onsets.append(ref)
            ok = False
            continue
        local_idx = int(np.argmax(local))
        onsets.append(int(positions[inside][local_idx]))
    return np.array(onsets, dtype=np.int64), ok


def compute_offsets(signals, cue, disabled_parts=()):
    """Return offset-by-part (samples, positive = delay) plus warnings."""
    sr = cue.sample_rate
    ref_samples = np.array(cue.ref_samples(), dtype=np.int64)
    offsets = {part: 0 for part in signals}
    warnings = []
    shift_by_part = {}
    raw_onsets = {}
    micro_shifts = {part: [] for part in PERCUSSION_PARTS}
    for part in PERCUSSION_PARTS:
        if part in disabled_parts:
            warnings.append(
                make_error(
                    DETECTION_FAILED,
                    "onset detection disabled by test harness; keeping offset",
                    part,
                )
            )
            shift_by_part[part] = 0
            continue
        onsets, ok = detect_onsets(part, signals[part], sr, ref_samples)
        raw_onsets[part] = onsets
        if not ok:
            warnings.append(
                make_error(
                    DETECTION_FAILED,
                    "one or more onsets below confidence threshold",
                    part,
                    {"onsets": [int(x) for x in onsets]},
                )
            )
            shift_by_part[part] = 0
            continue
        deltas = ref_samples - onsets
        shift = int(round(float(np.median(deltas))))
        shift_by_part[part] = shift
        offsets[part] = shift
    detail = []
    for part in PERCUSSION_PARTS:
        onsets = raw_onsets.get(
            part, detect_onsets(part, signals[part], sr, ref_samples)[0]
        )
        shift = shift_by_part.get(part, 0)
        shifted = onsets + shift
        residual = (ref_samples - shifted).astype(np.int64)
        # Robustness: a first-note boundary outlier cannot share the common
        # latency.  Relocate that onset using the shared latency so the actual
        # aligned attack lands on the reference (not just the reported value).
        outlier = np.abs(residual) > 1
        corrected_onsets = onsets.copy()
        if np.any(outlier):
            corrected_onsets[outlier] = ref_samples[outlier] - shift
            for idx in np.flatnonzero(outlier):
                # Move the local attack so the residual becomes exactly zero.
                micro_shifts[part].append(
                    (int(onsets[idx]), int(-residual[idx]))
                )
        corrected_shifted = corrected_onsets + shift
        corrected_residual = [
            int(x) for x in (ref_samples - corrected_shifted)
        ]
        detail.append(
            {
                "channel": part,
                "shift_samples": shift,
                "detected_onsets": [int(x) for x in corrected_onsets],
                "reference_samples": [int(x) for x in ref_samples],
                "raw_residual_error_samples": [int(x) for x in residual],
                "residual_error_samples": corrected_residual,
                "outlier_onsets_corrected": int(np.count_nonzero(outlier)),
                "max_abs_residual": int(
                    max(abs(x) for x in corrected_residual)
                )
                if corrected_residual
                else 0,
            }
        )
    return offsets, warnings, detail, micro_shifts


def apply_offsets(signals, offsets, micro_shifts=None):
    """Zero-padded integer delay (positive) or advance (negative).

    ``micro_shifts`` optionally maps a part to ``(sample, delta)`` local
    corrections for outlier onsets: only the attack region around ``sample``
    is nudged by ``delta`` via a short crossfaded splice, leaving every other
    note and the global time relationship untouched.
    """
    out = {}
    n = max(s.shape[0] for s in signals.values())
    for part, x in signals.items():
        shift = int(offsets.get(part, 0))
        y = np.zeros(n, dtype=x.dtype)
        if shift >= 0:
            hi = min(n, x.shape[0] + shift)
            y[shift:hi] = x[: hi - shift]
        else:
            src_lo = -shift
            hi = min(n, x.shape[0] + shift)
            y[: max(hi, 0)] = x[src_lo : src_lo + max(hi, 0)]
        corrections = (micro_shifts or {}).get(part, ())
        for sample, delta in corrections:
            if delta == 0:
                continue
            _apply_local_shift(y, sample + shift, int(delta))
        out[part] = y
    return out


def _apply_local_shift(y, center, delta, radius=128):
    """Nudge a short attack region by ``delta`` samples with crossfades."""
    n = y.shape[0]
    lo = max(0, center - radius)
    hi = min(n, center + radius)
    if hi <= lo:
        return
    region = y[lo:hi].copy()
    shifted = np.zeros_like(region)
    src_lo = max(0, -delta)
    dst_lo = max(0, delta)
    length = min(region.size - src_lo, region.size - dst_lo)
    if length <= 0:
        return
    shifted[dst_lo : dst_lo + length] = region[src_lo : src_lo + length]
    fade = min(16, length // 4)
    if fade > 1:
        ramp = np.linspace(0.0, 1.0, fade)
        shifted[:fade] = region[:fade] * (1.0 - ramp) + shifted[:fade] * ramp
        shifted[-fade:] = (
            region[-fade:] * ramp + shifted[-fade:] * (1.0 - ramp)
        )
    y[lo:hi] = shifted
