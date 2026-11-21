"""Sync marker detection via normalized matched filtering."""

from __future__ import annotations

import numpy as np

from .generator import TRUTH_SOURCE, marker_template


def _normalized_matched_filter(x, tmpl):
    """Correlation of x with tmpl, normalized by local energy -> [-1, 1]."""
    n_t = len(tmpl)
    n_fft = 1 << int(np.ceil(np.log2(len(x) + n_t)))
    spec_x = np.fft.rfft(x, n_fft)
    spec_t = np.fft.rfft(tmpl[::-1], n_fft)
    corr = np.fft.irfft(spec_x * spec_t, n_fft)[n_t - 1 : len(x)]
    energy = np.concatenate(([0.0], np.cumsum(x * x)))
    local = energy[n_t:] - energy[:-n_t]
    local = np.maximum(local, 1e-18)
    norm = corr[: len(local)] / np.sqrt(local * np.sum(tmpl * tmpl))
    return norm


def detect_markers(signal, fs, period_s, snr_threshold=0.3, min_conf=0.5):
    """Detect periodic marker arrivals in one channel.

    Returns dict with:
      markers: list of {index, sample, confidence}
      unavailable: list of {index, reason, truth_source}
    Marker indices are assigned by rounding to the nominal period; drift
    well below half a period is tolerated. Missing or low-SNR markers are
    reported as unavailable and never interpolated silently.
    """
    tmpl = marker_template(fs)
    period = period_s * fs
    norm = _normalized_matched_filter(signal, tmpl)
    half = len(tmpl) // 2
    min_dist = int(period * 0.5)
    threshold = max(min_conf, snr_threshold * 0.5)
    cand = []
    above = np.flatnonzero(norm > threshold)
    if len(above):
        groups = np.split(above, np.flatnonzero(np.diff(above) > 1) + 1)
        for group in groups:
            peak = group[np.argmax(norm[group])]
            cand.append((peak, float(norm[peak])))
        cand.sort(key=lambda c: -c[1])
        kept = []
        for pos, conf in cand:
            if all(abs(pos - k[0]) >= min_dist for k in kept):
                kept.append((pos, conf))
        cand = sorted(kept)
    markers = []
    used_idx = set()
    for pos, conf in cand:
        idx = int(round(pos / period))
        if idx < 1 or idx in used_idx:
            continue
        used_idx.add(idx)
        if 1 <= pos < len(norm) - 1:
            y0, y1, y2 = norm[pos - 1], norm[pos], norm[pos + 1]
            denom = y0 - 2.0 * y1 + y2
            frac = 0.5 * (y0 - y2) / denom if abs(denom) > 1e-12 else 0.0
        else:
            frac = 0.0
        markers.append(
            {
                "index": idx,
                "sample": float(pos + half + frac),
                "confidence": round(conf, 6),
            }
        )
    markers.sort(key=lambda m: m["index"])
    unavailable = []
    if markers:
        present = {m["index"]: m for m in markers}
        for idx in range(1, markers[-1]["index"] + 1):
            if idx not in present:
                unavailable.append(
                    {
                        "index": idx,
                        "reason": "missing_or_low_snr",
                        "truth_source": TRUTH_SOURCE,
                    }
                )
            elif present[idx]["confidence"] < snr_threshold:
                unavailable.append(
                    {
                        "index": idx,
                        "reason": "low_snr",
                        "truth_source": TRUTH_SOURCE,
                    }
                )
    return {"markers": markers, "unavailable": unavailable}
