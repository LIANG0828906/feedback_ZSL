"""Deterministic local polynomial resampling.

Catmull-Rom interpolation is a local cubic polynomial.  Every output sample is
derived from four neighbouring source samples, so seeking to an arbitrary slice
boundary produces exactly the same samples as streaming the whole timeline.
"""

from __future__ import annotations

import numpy as np


def catmull_rom(y0: np.ndarray, y1: np.ndarray, y2: np.ndarray, y3: np.ndarray, t: np.ndarray) -> np.ndarray:
    t = t[..., None] if t.ndim == 1 and y0.ndim == 2 else t
    return y1 + 0.5 * t * (
        (y2 - y0)
        + t * ((2.0 * y0 - 5.0 * y1 + 4.0 * y2 - y3) + t * (3.0 * (y1 - y2) + y3 - y0))
    )


def resample_output_segment(
    source: np.ndarray,
    source_rate: int,
    output_rate: int,
    start_sample: int,
    sample_count: int,
    delay_samples: int = 0,
) -> tuple[np.ndarray, np.ndarray]:
    """Resample an output-time segment from a source track.

    Returns ``(samples, valid)``.  ``valid`` marks positions covered by the
    source; positions before time zero or after the final source sample are
    zero-filled.  The interpolation itself uses clamped neighbours for stable
    edge behaviour.
    """

    if sample_count <= 0:
        return np.zeros((0, source.shape[1]), dtype=np.float64), np.zeros(0, dtype=bool)
    if source.shape[0] == 0:
        return np.zeros((sample_count, source.shape[1]), dtype=np.float64), np.zeros(
            sample_count, dtype=bool
        )
    positions = (
        np.arange(start_sample, start_sample + sample_count, dtype=np.float64) - delay_samples
    ) * (float(source_rate) / float(output_rate))
    valid = (positions >= 0.0) & (positions <= float(source.shape[0] - 1))
    safe_positions = np.clip(positions, 0.0, float(max(0, source.shape[0] - 1)))
    base = np.floor(safe_positions).astype(np.int64)
    fraction = safe_positions - base
    last = source.shape[0] - 1
    i0 = np.clip(base - 1, 0, last)
    i1 = np.clip(base, 0, last)
    i2 = np.clip(base + 1, 0, last)
    i3 = np.clip(base + 2, 0, last)
    values = catmull_rom(source[i0], source[i1], source[i2], source[i3], fraction)
    values[~valid] = 0.0
    return values, valid


def to_output_channels(source: np.ndarray, output_channels: int) -> np.ndarray:
    if output_channels == 1:
        if source.shape[1] == 1:
            return source
        return source.mean(axis=1, keepdims=True)
    if source.shape[1] == 2:
        return source
    return np.repeat(source, 2, axis=1)


def quantize_pcm(values: np.ndarray) -> np.ndarray:
    scaled = np.rint(np.clip(values, -1.0, 1.0) * 32767.0)
    return np.clip(scaled, -32768, 32767).astype("<i2")
