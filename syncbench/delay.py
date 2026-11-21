"""Segmented cross-correlation delay estimation.

Delays are estimated by windowed cross-correlation around each reference
marker (sharp thumbtack peak, high per-marker SNR) and aggregated per
segment as the median, assigned to the mean marker position. Segments
without usable markers or with low correlation confidence are marked
invalid and never silently interpolated.
"""

from __future__ import annotations

import numpy as np


def _xcorr_lag_vals(ref, sig, max_lag):
    """Circular cross-correlation values for lags [-max_lag, max_lag].

    Peak at lag k means sig is ref delayed by k (same alignment origin).
    """
    n = len(ref)
    n_fft = 1 << int(np.ceil(np.log2(2 * n)))
    spec_r = np.fft.rfft(ref, n_fft)
    spec_s = np.fft.rfft(sig, n_fft)
    corr = np.fft.irfft(spec_s * np.conj(spec_r), n_fft)
    lags = np.concatenate((np.arange(0, max_lag + 1), np.arange(-max_lag, 0)))
    vals = np.concatenate((corr[: max_lag + 1], corr[n_fft - max_lag :]))
    return lags, vals


def _parabolic(y0, y1, y2):
    denom = y0 - 2.0 * y1 + y2
    return 0.5 * (y0 - y2) / denom if abs(denom) > 1e-12 else 0.0


def xcorr_delay(ref, sig, max_lag):
    """Whole-record delay of sig vs ref (subsample), with peak confidence."""
    lags, vals = _xcorr_lag_vals(ref, sig, max_lag)
    best = int(np.argmax(vals))
    lag = float(lags[best])
    conf = (vals[best] - float(np.mean(vals))) / (float(np.std(vals)) or 1e-12)
    if 0 < best < len(vals) - 1:
        lag += _parabolic(vals[best - 1], vals[best], vals[best + 1])
    return lag, conf


def marker_xcorr_delay(ref, sig, center, win, max_lag):
    """Delay of sig vs ref in a window around a reference marker position.

    Returns (delay, confidence) or None when the window leaves the record.
    """
    half = win // 2
    c = int(round(center))
    r0 = c - half
    ref_w = ref[r0 : r0 + win]
    sig_w = sig[r0 - max_lag : r0 + win + max_lag]
    if len(ref_w) < win or len(sig_w) < win + 2 * max_lag:
        return None
    n_fft = 1 << int(np.ceil(np.log2(2 * (win + max_lag))))
    spec_r = np.fft.rfft(ref_w, n_fft)
    spec_s = np.fft.rfft(sig_w, n_fft)
    corr = np.fft.irfft(spec_s * np.conj(spec_r), n_fft)
    vals = corr[: 2 * max_lag + 1]
    best = int(np.argmax(vals))
    conf = (vals[best] - float(np.mean(vals))) / (float(np.std(vals)) or 1e-12)
    frac = 0.0
    if 0 < best < len(vals) - 1:
        frac = _parabolic(vals[best - 1], vals[best], vals[best + 1])
    return (best + frac) - max_lag, conf


def estimate_segment_delays(
    ref, sig, seg_len, max_lag, min_conf=5.0, ref_markers=None, win=1024
):
    """Per-segment delay of sig vs ref from marker-windowed correlations.

    ref_markers: reference-channel marker arrival samples (required).
    Returns list of dicts: {segment, start, center, delay, confidence,
    valid, n_markers}. Invalid segments carry delay=None.
    """
    n = min(len(ref), len(sig))
    positions = [float(p) for p in (ref_markers or [])]
    segments = []
    seg_idx = 0
    for start in range(0, n - seg_len + 1, seg_len):
        end = start + seg_len
        delays = []
        confs = []
        used = []
        for pos in positions:
            if start <= pos < end:
                res = marker_xcorr_delay(ref, sig, pos, win, max_lag)
                if res is not None:
                    delays.append(res[0])
                    confs.append(res[1])
                    used.append(pos)
        if delays:
            conf = float(np.min(confs))
            segments.append(
                {
                    "segment": seg_idx,
                    "start": start,
                    "center": float(np.mean(used)),
                    "delay": float(np.median(delays)),
                    "confidence": round(conf, 4),
                    "valid": bool(conf >= min_conf),
                    "n_markers": len(delays),
                }
            )
        else:
            segments.append(
                {
                    "segment": seg_idx,
                    "start": start,
                    "center": start + seg_len / 2.0,
                    "delay": None,
                    "confidence": 0.0,
                    "valid": False,
                    "n_markers": 0,
                }
            )
        seg_idx += 1
    return segments
