"""Loudness/true-peak compliance stage with delay-compensated limiting."""

from __future__ import annotations

import numpy as np

from .dsp import design_oversample_filter, lookahead_limiter, true_peak_db
from .loudness import integrated_loudness


def _force_below_ceiling(current, sr, limit_db, kernel, stats, iterations,
                         max_iterations=12):
    """Iterate the look-ahead limiter until TP is truly under the ceiling."""
    ceiling = limit_db - 0.2
    limit_linear = 10.0 ** (limit_db / 20.0)
    for _ in range(max_iterations):
        # Fast linear-peak pre-check: if even the un-oversampled peak is
        # already below a conservative margin, no limiting (and no expensive
        # 4x FFT) is required.
        linear_peak = float(np.max(np.abs(current)))
        if linear_peak <= limit_linear * 0.85:
            tp_check = true_peak_db(current, kernel)
            iterations.append(
                {
                    "ceiling_db": round(ceiling, 6),
                    "true_peak_db": round(tp_check, 6),
                    "triggered_samples": 0,
                    "max_attenuation_db": 0.0,
                }
            )
            break
        limited, meta = lookahead_limiter(current, sr, ceiling)
        # One precise oversampled measurement per actual limiting pass.
        tp = true_peak_db(limited, kernel)
        stats["triggered"] += meta["triggered_samples"]
        stats["max_atten"] = max(
            stats["max_atten"], meta["max_attenuation_db"]
        )
        iterations.append(
            {
                "ceiling_db": round(ceiling, 6),
                "true_peak_db": round(tp, 6),
                "triggered_samples": meta["triggered_samples"],
                "max_attenuation_db": round(meta["max_attenuation_db"], 6),
            }
        )
        current = limited
        if tp <= limit_db:
            break
        ceiling -= max(0.15, tp - limit_db + 0.2)
    return current


def comply(master, sr, target_lufs, true_peak_limit_db,
           loudness_tolerance_lu=0.5, max_iterations=12):
    """Return (master, compliance report). Sample count never changes."""
    original_samples = master.shape[0]
    kernel = design_oversample_filter()
    iterations = []
    stats = {"triggered": 0, "max_atten": 0.0}
    current = master
    lufs_before = integrated_loudness(current, sr)

    # Stage 1: guarantee the true-peak ceiling first.
    current = _force_below_ceiling(
        current, sr, true_peak_limit_db, kernel, stats, iterations,
        max_iterations,
    )
    lufs_after_limiter = integrated_loudness(current, sr)

    # Stage 2: alternate loudness trims and ceiling re-assertion so both the
    # LUFS target and the dBTP ceiling hold simultaneously.
    trim_db = 0.0
    for _ in range(max_iterations):
        lufs_now = integrated_loudness(current, sr)
        if not np.isfinite(lufs_now):
            break
        delta = target_lufs - lufs_now
        if abs(delta) <= loudness_tolerance_lu:
            break
        # delta<0 means the target is quieter -> linear gain < 1 (attenuate).
        current = current * (10.0 ** (delta / 20.0))
        trim_db += delta
        current = _force_below_ceiling(
            current, sr, true_peak_limit_db, kernel, stats, iterations,
            max_iterations,
        )

    # Unconditional final guarantee against any residual overshoot.
    current = _force_below_ceiling(
        current, sr, true_peak_limit_db, kernel, stats, iterations,
        max_iterations,
    )

    lufs_final = integrated_loudness(current, sr)
    tp_final = true_peak_db(current, kernel)
    assert current.shape[0] == original_samples
    report = {
        "target_lufs": target_lufs,
        "true_peak_limit_db": true_peak_limit_db,
        "loudness_before_db": _safe(lufs_before),
        "loudness_after_limiter_db": _safe(lufs_after_limiter),
        "loudness_final_lufs": _safe(lufs_final),
        "loudness_error_lu": _safe(lufs_final - target_lufs)
        if np.isfinite(lufs_final)
        else None,
        "true_peak_final_db": _safe(tp_final),
        "static_trim_db": round(trim_db, 6),
        "limiter_triggered_samples": int(stats["triggered"]),
        "limiter_max_attenuation_db": round(stats["max_atten"], 6),
        "limiter_iterations": iterations,
        "tp_compliant": bool(tp_final <= true_peak_limit_db),
        "loudness_compliant": bool(
            np.isfinite(lufs_final)
            and abs(lufs_final - target_lufs) <= loudness_tolerance_lu
        ),
    }
    return current, report


def _safe(v):
    if not np.isfinite(v):
        return None
    return round(float(v), 6)
