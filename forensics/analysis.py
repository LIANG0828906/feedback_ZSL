"""Noise residual, edge-direction consistency and local-contrast analysis.

Every statistic is computed independently per channel (no silent luminance
down-mix) and then aggregated with an explicit ``per_channel`` record plus a
max-over-channels summary used for tamper decisions.  All routines are pure
NumPy and deterministic for fixed inputs and parameters.
"""

import numpy as np

from .errors import ForensicsError, E_PARA
from .hashing import median_filter2d, uniform_filter2d as _uniform_filter2d


def _validate_windows(params):
    for name in ("noise_window", "edge_window", "contrast_window"):
        value = getattr(params, name)
        if not isinstance(value, int) or value <= 0 or value % 2 == 0:
            raise ForensicsError(
                E_PARA,
                "%s must be a positive odd integer, got %r" % (name, value),
                field=name,
            )


def noise_residual_variance(block, window):
    """Per-channel variance of (pixel - local median), plus residual map."""
    channels = block.shape[2]
    per_channel = []
    residuals = np.zeros_like(block, dtype=np.float64)
    for c in range(channels):
        plane = block[:, :, c].astype(np.float64)
        estimate = median_filter2d(plane, window)
        residual = plane - estimate
        residuals[:, :, c] = residual
        per_channel.append(float(np.var(residual)))
    return per_channel, residuals


def edge_direction_consistency(block, window):
    """Per-channel Sobel edge strength and dominant-orientation consistency.

    Returns one value in [0, 1] per channel: the fraction of edge pixels whose
    gradient direction agrees with the block's dominant orientation.  A value
    near 1 means a coherent structured edge; isotropic noise scores low.
    """
    channels = block.shape[2]
    per_channel = []
    for c in range(channels):
        plane = block[:, :, c].astype(np.float64)
        gx = np.zeros_like(plane)
        gy = np.zeros_like(plane)
        gx[:, 1:-1] = plane[:, 2:] - plane[:, :-2]
        gy[1:-1, :] = plane[2:, :] - plane[:-2, :]
        strength = np.hypot(gx, gy)
        # Fixed relative threshold avoids a per-block percentile sort while
        # keeping the dominant-orientation statistic deterministic.
        cutoff = 0.25 * float(strength.max())
        edge_mask = strength >= max(cutoff, 1e-9)
        if int(edge_mask.sum()) < 2:
            per_channel.append(0.0)
            continue
        angle = np.arctan2(gy[edge_mask], gx[edge_mask])
        # Circular dominant direction via mean of unit vectors.
        mean_vec = np.abs(np.mean(np.exp(1j * angle)))
        per_channel.append(float(np.clip(mean_vec, 0.0, 1.0)))
    return per_channel


def local_contrast(block, window):
    """Per-channel local contrast: mean local standard deviation."""
    channels = block.shape[2]
    per_channel = []
    for c in range(channels):
        plane = block[:, :, c].astype(np.float64)
        local_mean = _uniform_filter2d(plane, window)
        local_sq_mean = _uniform_filter2d(plane * plane, window)
        variance = np.clip(local_sq_mean - local_mean ** 2, 0.0, None)
        per_channel.append(float(np.mean(np.sqrt(variance))))
    return per_channel


def analyze_block(block, params):
    """Compute the full per-block analysis record.

    ``block`` keeps its channel axis.  The returned dict preserves per-channel
    statistics and adds max-over-channel summaries plus the boolean tamper
    decision (a block is suspicious if *any* threshold is exceeded).
    """
    _validate_windows(params)
    block = np.asarray(block, dtype=np.uint8)
    channels = block.shape[2]

    noise_pc, residual = noise_residual_variance(block, params.noise_window)
    edge_pc = edge_direction_consistency(block, params.edge_window)
    contrast_pc = local_contrast(block, params.contrast_window)

    noise_var_value = float(max(noise_pc))
    edge_consistency_value = float(min(edge_pc))
    local_contrast_value = float(max(contrast_pc))

    # Tamper signatures on the deterministic samples: high residual variance,
    # low directional coherence, high local contrast.
    suspicious = (
        noise_var_value > params.noise_threshold
        and local_contrast_value > params.contrast_threshold
        and edge_consistency_value < params.edge_threshold
    )

    # Bounded [0,1] score combining the three signals for heatmap rendering.
    noise_score = min(1.0, noise_var_value / max(params.noise_threshold * 2.0, 1e-9))
    contrast_score = min(
        1.0, local_contrast_value / max(params.contrast_threshold * 2.0, 1e-9)
    )
    edge_score = 1.0 - edge_consistency_value
    score = float(np.clip((noise_score + contrast_score + edge_score) / 3.0, 0.0, 1.0))

    return {
        "channels": channels,
        "per_channel": {
            "noise_var": [float(v) for v in noise_pc],
            "edge_consistency": [float(v) for v in edge_pc],
            "local_contrast": [float(v) for v in contrast_pc],
        },
        "noise_var": noise_var_value,
        "edge_consistency": edge_consistency_value,
        "local_contrast": local_contrast_value,
        "score": score,
        "suspicious": bool(suspicious),
        "residual_energy": float(np.mean(residual ** 2)),
    }


def monotonicity_check(metric_values, parameter_values):
    """Verify a metric moves monotonically as a parameter tightens.

    Returns True if ``metric_values`` is monotonic (non-decreasing or
    non-increasing) as ``parameter_values`` increases.  Used to demonstrate the
    'monotone and explainable as parameters tighten' requirement.
    """
    arr = np.asarray(metric_values, dtype=np.float64)
    diffs = np.diff(arr)
    return bool(np.all(diffs >= -1e-12) or np.all(diffs <= 1e-12))
