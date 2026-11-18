"""Small deterministic DSP helpers used by several stages."""

from __future__ import annotations

import numpy as np


def frac_delay(signal: np.ndarray, delay_samples: float) -> np.ndarray:
    """Delay ``signal`` by a (possibly fractional) number of samples.

Linear interpolation, zero padding at both ends. A positive delay moves the
    waveform to the right; a negative delay advances it. The implementation is
    pure NumPy and therefore bit-for-bit reproducible.
    """
    x = np.asarray(signal, dtype=np.float64)
    n = x.shape[0]
    if n == 0:
        return x.copy()
    d = float(delay_samples)
    base = int(np.floor(d))
    frac = d - base
    out = np.zeros(n, dtype=np.float64)
    if frac == 0.0:
        source_indices = np.arange(n) - base
        valid = (source_indices >= 0) & (source_indices < n)
        out[valid] = x[source_indices[valid]]
        return out
    # Output i reconstructs x at continuous position (i - d):
    # x(i-d) = (1-frac)*x(i-base) + frac*x(i-base-1).
    for output_index in range(n):
        near = output_index - base
        far = near - 1
        value = 0.0
        if 0 <= near < n:
            value += (1.0 - frac) * x[near]
        if 0 <= far < n:
            value += frac * x[far]
        out[output_index] = value
    return out


def wrap_deg(angle_deg: float) -> float:
    """Wrap an angle in degrees to the half-open interval [-180, 180)."""
    return (float(angle_deg) + 180.0) % 360.0 - 180.0


def circular_error_deg(measured: float, truth: float) -> float:
    """Absolute circular angular error in degrees, range [0, 180]."""
    return abs(wrap_deg(measured - truth))


def canonical_json_bytes(payload: dict) -> bytes:
    """Canonical byte serialization used for manifest digest/snapshots."""
    import json

    return json.dumps(
        payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")
