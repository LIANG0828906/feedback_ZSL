"""Deterministic fractional-delay resampling for drift compensation."""

from __future__ import annotations

import numpy as np

MODE_LINEAR = "linear"
MODE_SINC = "sinc"
SINC_TAPS = 16  # taps each side


def _positions(n_out, offset, slope):
    n = np.arange(n_out, dtype=np.float64)
    return offset + (1.0 + slope) * n


def output_length(n_in, offset, slope):
    """Largest output length whose read positions stay inside the input."""
    rate = 1.0 + slope
    if rate <= 0:
        raise ValueError("non-positive resample rate")
    last = (n_in - 1.0 - offset) / rate
    return max(int(np.floor(last)) + 1, 0)


def resample_linear(x, offset, slope, n_out=None):
    x = np.asarray(x, dtype=np.float64)
    if n_out is None:
        n_out = output_length(len(x), offset, slope)
    pos = _positions(n_out, offset, slope)
    return np.interp(pos, np.arange(len(x), dtype=np.float64), x)


def resample_sinc(x, offset, slope, n_out=None, taps=SINC_TAPS):
    """Windowed-sinc (Hann) fractional resampling, deterministic."""
    x = np.asarray(x, dtype=np.float64)
    pad = taps + 2
    xp = np.concatenate((np.zeros(pad), x, np.zeros(pad)))
    if n_out is None:
        n_out = output_length(len(x), offset, slope)
    pos = _positions(n_out, offset, slope) + pad
    base = np.floor(pos).astype(np.int64)
    frac = pos - base
    k = np.arange(-taps + 1, taps + 1, dtype=np.float64)
    arg = frac[:, None] - k[None, :]
    kernel = np.sinc(arg)
    win_arg = (k[None, :] - frac[:, None]) / (taps + 0.5)
    window = np.where(
        np.abs(win_arg) <= 1.0, 0.5 * (1.0 + np.cos(np.pi * win_arg)), 0.0
    )
    kernel = kernel * window
    idx = base[:, None] + k.astype(np.int64)[None, :]
    out = np.sum(xp[idx] * kernel, axis=1)
    return out


def resample(x, offset, slope, mode=MODE_LINEAR, n_out=None):
    if mode == MODE_LINEAR:
        return resample_linear(x, offset, slope, n_out)
    if mode == MODE_SINC:
        return resample_sinc(x, offset, slope, n_out)
    raise ValueError(f"unknown resample mode: {mode}")
