"""ITU-R BS.1770-4 integrated loudness (stereo) and section metrics."""

from __future__ import annotations

import numpy as np

from .dsp import biquad_apply, k_weighting_coeffs, true_peak_db

# Stereo weighting: L=R=1.0, no surround channels.
CHANNEL_WEIGHTS = np.array([1.0, 1.0])
ABSOLUTE_GATE_LUFS = -70.0
RELATIVE_GATE_OFFSET_LU = -10.0


def k_weighted(signal, sr):
    b, a = k_weighting_coeffs(sr)
    if signal.ndim == 1:
        return biquad_apply(signal, b, a)
    return np.column_stack(
        [biquad_apply(signal[:, ch], b, a) for ch in range(signal.shape[1])]
    )


def _block_means(weighted, sr, block_seconds, hop_seconds):
    block = int(round(block_seconds * sr))
    hop = int(round(hop_seconds * sr))
    n = weighted.shape[0]
    means = []
    if n < block:
        padded = np.zeros((block, weighted.shape[1]))
        padded[:n] = weighted
        weighted = padded
        n = block
    for start in range(0, n - block + 1, hop):
        frame = weighted[start : start + block]
        mean_square = np.mean(frame**2, axis=0)
        means.append(float(np.sum(CHANNEL_WEIGHTS * mean_square)))
    return np.array(means)


def integrated_loudness(signal, sr):
    """BS.1770-4 gated integrated loudness in LUFS (LKFS)."""
    weighted = k_weighted(signal, sr)
    blocks_400 = _block_means(weighted, sr, 0.4, 0.1)
    blocks_100 = _block_means(weighted, sr, 0.1, 0.1)
    blocks = np.concatenate([blocks_400, blocks_100])
    blocks = blocks[blocks > 0.0]
    if blocks.size == 0:
        return -np.inf
    # Absolute gate at -70 LKFS.
    abs_thresh = 10.0 ** (ABSOLUTE_GATE_LUFS / 10.0)
    gated = blocks[blocks > abs_thresh]
    if gated.size == 0:
        return ABSOLUTE_GATE_LUFS
    ungated_loudness = -0.691 + 10.0 * np.log10(np.mean(gated))
    rel_thresh = 10.0 ** ((ungated_loudness + RELATIVE_GATE_OFFSET_LU) / 10.0)
    rel_gated = gated[gated > rel_thresh]
    if rel_gated.size == 0:
        return ungated_loudness
    return -0.691 + 10.0 * np.log10(np.mean(rel_gated))


def section_metrics(master, sr, sections, oversample_kernel=None):
    """Per-section integrated loudness and true peak for the report."""
    out = []
    n = master.shape[0]
    for sec in sections:
        start = min(int(round(sec.start * sr)), n)
        end = min(int(round(sec.end * sr)), n)
        frame = master[start:end]
        if frame.shape[0] < int(0.4 * sr):
            lufs = -np.inf
        else:
            lufs = integrated_loudness(frame, sr)
        tp = true_peak_db(frame, oversample_kernel)
        out.append(
            {
                "name": sec.name,
                "start_sample": start,
                "end_sample": end,
                "integrated_lufs": _safe_round(lufs),
                "true_peak_db": _safe_round(tp),
            }
        )
    return out


def _safe_round(value, digits=6):
    if not np.isfinite(value):
        return None
    return round(float(value), digits)
