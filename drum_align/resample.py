"""Curve-driven linear resampling into the reference camera timeline."""

from __future__ import annotations

import numpy as np

from .drift import DriftCurve
from .signal import pearson_correlation


def _output_channels(samples: np.ndarray, out_channels: int) -> np.ndarray:
    source_channels = samples.shape[1]
    if source_channels == out_channels:
        return samples.astype(np.float64, copy=False)
    if out_channels == 1:
        return samples.astype(np.float64).mean(axis=1, keepdims=True)
    if out_channels == 2 and source_channels == 1:
        mono = samples[:, 0].astype(np.float64)
        return np.column_stack((mono, mono))
    raise ValueError(
        f"cannot map {source_channels} source channels to {out_channels} outputs"
    )


def sample_at_reference_positions(
    samples: np.ndarray,
    positions: np.ndarray,
    curve: DriftCurve,
    *,
    out_channels: int,
) -> np.ndarray:
    """Sample a track at reference-axis positions using the drift curve.

    ``positions`` are reference sample coordinates. The curve gives the auxiliary
    source coordinate as ``position + offset(position)``. Endpoint offsets are
    held constant by ``np.interp``.
    """
    source = _output_channels(samples, out_channels)
    ref = np.asarray(positions, dtype=np.float64)
    if ref.size == 0:
        return np.zeros((0, out_channels), dtype=np.float64)
    offsets = curve.offsets_at(ref)
    source_positions = ref + offsets
    lower = np.floor(source_positions).astype(np.int64)
    fraction = (source_positions - lower).astype(np.float64)
    valid = (lower >= 0) & (lower < source.shape[0])
    safe_lower = np.where(valid, lower, 0)
    padded = np.vstack((source, np.zeros((1, out_channels), dtype=np.float64)))
    upper = safe_lower + 1
    result = (
        padded[safe_lower] * (1.0 - fraction)[:, None]
        + padded[upper] * fraction[:, None]
    )
    result[~valid] = 0.0
    return result


def phase_windows(
    *,
    bar_starts: list[int],
    reference_mono: np.ndarray,
    aux_samples: np.ndarray,
    curve: DriftCurve,
    window_samples: int,
    pre_roll_samples: int,
    threshold: float,
) -> tuple[bool, list[dict[str, object]]]:
    """Check polarity/consistency around all score anchors."""
    windows: list[dict[str, object]] = []
    flagged = False
    total = reference_mono.size
    for bar in bar_starts:
        start = max(0, int(bar) - int(pre_roll_samples))
        end = min(total, start + int(window_samples))
        if end <= start:
            continue
        positions = np.arange(start, end, dtype=np.float64)
        aux = sample_at_reference_positions(
            aux_samples, positions, curve, out_channels=1
        )[:, 0]
        reference = reference_mono[start:end]
        correlation = round(pearson_correlation(reference, aux), 6)
        is_flagged = correlation < threshold
        flagged = flagged or is_flagged
        windows.append(
            {
                "start_sample": int(start),
                "end_sample": int(end),
                "correlation": correlation,
                "flagged": bool(is_flagged),
            }
        )
    return flagged, windows
