"""First-order clock drift model fitting: delay(t) = offset + slope * t."""

from __future__ import annotations

import numpy as np


def fit_drift(centers, delays, weights=None):
    """Least-squares fit of delay = offset + slope * center.

    Returns dict with offset (samples), slope (samples/sample), ppm,
    per-segment residuals, residual RMS and 2-sigma confidence intervals.
    """
    t = np.asarray(centers, dtype=np.float64)
    d = np.asarray(delays, dtype=np.float64)
    if len(t) < 2:
        slope = 0.0
        offset = float(d[0]) if len(d) else 0.0
        resid = d - (offset + slope * t)
        return {
            "offset": offset,
            "slope": slope,
            "ppm": 0.0,
            "residuals": resid.tolist(),
            "residual_rms": float(np.sqrt(np.mean(resid**2))) if len(resid) else 0.0,
            "offset_ci": 0.0,
            "ppm_ci": 0.0,
        }
    w = np.ones_like(t) if weights is None else np.asarray(weights, dtype=np.float64)
    t_scale = float(np.max(np.abs(t))) or 1.0
    tn = t / t_scale
    a = np.vstack([w, w * tn]).T
    ata = a.T @ a
    atd = a.T @ (w * d)
    coef = np.linalg.solve(ata, atd)
    offset, slope_n = float(coef[0]), float(coef[1])
    slope = slope_n / t_scale
    resid = d - (offset + slope * t)
    dof = max(len(t) - 2, 1)
    sigma2 = float(np.sum(w * resid**2) / dof)
    cov = sigma2 * np.linalg.inv(ata)
    offset_ci = 2.0 * float(np.sqrt(max(cov[0, 0], 0.0)))
    ppm_ci = 2.0 * float(np.sqrt(max(cov[1, 1], 0.0))) / t_scale * 1e6
    return {
        "offset": offset,
        "slope": slope,
        "ppm": slope * 1e6,
        "residuals": resid.tolist(),
        "residual_rms": float(np.sqrt(np.mean(resid**2))),
        "offset_ci": offset_ci,
        "ppm_ci": ppm_ci,
    }


def jump_candidates(residuals, threshold):
    """Segment indices whose fit residual exceeds the threshold."""
    return [i for i, r in enumerate(residuals) if abs(r) > threshold]


def compare_fits(old, new):
    """Parameter difference after a configuration change (e.g. seg length)."""
    return {
        "delta_offset": new["offset"] - old["offset"],
        "delta_ppm": new["ppm"] - old["ppm"],
        "delta_residual_rms": new["residual_rms"] - old["residual_rms"],
    }
