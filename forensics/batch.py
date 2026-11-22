"""Vectorised batch kernels used only by the interactive preview path.

These operate on a stack of all downscaled blocks at once, replacing the
per-block Python loop with a handful of NumPy calls.  They implement the same
statistical definitions as :mod:`forensics.analysis` (median residual variance,
edge-direction consistency, local contrast) so preview and precise statistics
share one mathematical definition; the per-block precise path remains the
reference implementation and :func:`compare_paths` cross-checks both.
"""

import numpy as np


def _window_stack(padded, window, height, width):
    n, c = padded.shape[0], padded.shape[1]
    s0, s1, s2, s3 = padded.strides
    return np.lib.stride_tricks.as_strided(
        padded,
        shape=(n, c, height, width, window, window),
        strides=(s0, s1, s2, s3, s2, s3),
        writeable=False,
    )


def batch_median(volume, window):
    radius = window // 2
    padded = np.pad(volume, ((0, 0), (0, 0), (radius, radius), (radius, radius)), mode="edge")
    n, c, height, width = volume.shape
    flat = _window_stack(padded, window, height, width).reshape(
        n, c, height, width, window * window
    )
    middle = (window * window) // 2
    return np.partition(flat, middle, axis=4)[:, :, :, :, middle]


def batch_uniform_mean(volume, window):
    radius = window // 2
    padded = np.pad(volume, ((0, 0), (0, 0), (radius, radius), (radius, radius)), mode="edge")
    n, c, height, width = volume.shape
    flat = _window_stack(padded, window, height, width).reshape(
        n, c, height, width, window * window
    )
    return flat.mean(axis=4)


def batch_analyze(blocks, params):
    """Analyse a stack ``(N, H, W, C)`` of blocks in one vectorised pass."""
    # Rearrange to (N, C, H, W) for channel-wise batched filtering.
    volume = np.moveaxis(blocks.astype(np.float64), 3, 1)
    n, c, height, width = volume.shape

    median = batch_median(volume, params.noise_window)
    residual = volume - median
    noise_per_channel = residual.reshape(n, c, -1).var(axis=2)

    gx = np.zeros_like(volume)
    gy = np.zeros_like(volume)
    gx[:, :, :, 1:-1] = volume[:, :, :, 2:] - volume[:, :, :, :-2]
    gy[:, :, 1:-1, :] = volume[:, :, 2:, :] - volume[:, :, :-2, :]
    strength = np.hypot(gx, gy)
    cutoff = 0.25 * strength.reshape(n, c, -1).max(axis=2)[:, :, None, None]
    edge_mask = strength >= np.maximum(cutoff, 1e-9)
    angle = np.arctan2(gy, gx)
    unit = np.exp(1j * angle)
    masked_sum = np.where(edge_mask, unit, 0.0).reshape(n, c, -1).sum(axis=2)
    counts = np.maximum(edge_mask.reshape(n, c, -1).sum(axis=2), 1)
    edge_per_channel = np.clip(np.abs(masked_sum / counts), 0.0, 1.0)

    local_mean = batch_uniform_mean(volume, params.contrast_window)
    local_sq = batch_uniform_mean(volume * volume, params.contrast_window)
    local_std = np.sqrt(np.clip(local_sq - local_mean ** 2, 0.0, None))
    contrast_per_channel = local_std.reshape(n, c, -1).mean(axis=2)

    return {
        "noise_var": noise_per_channel.max(axis=1),
        "edge_consistency": edge_per_channel.min(axis=1),
        "local_contrast": contrast_per_channel.max(axis=1),
        "residual_energy": (residual ** 2).reshape(n, -1).mean(axis=1),
    }
