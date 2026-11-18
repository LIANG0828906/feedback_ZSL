"""Loudness targeting, true-peak limiting and mastery metrics."""

from __future__ import annotations

import math
from typing import Any

import numpy as np

from .dsp import (
    apply_lookahead_limiter,
    db_to_linear,
    integrated_loudness_lufs,
    linear_to_db,
    peak,
    true_peak_dbtp,
)


def master_bus(
    bus: np.ndarray,
    sample_rate: int,
    target_lufs: float,
    true_peak_limit: float,
) -> tuple[np.ndarray, dict[str, Any]]:
    """Find a deterministic gain/limiter plan satisfying both delivery limits."""

    source = np.asarray(bus, dtype=np.float64)
    if source.ndim != 2 or source.shape[1] != 2:
        raise ValueError("bus must be an (N, 2) stereo array")
    initial_loudness = integrated_loudness_lufs(source, sample_rate)
    gain_db = float(target_lufs) - initial_loudness
    ceiling = float(true_peak_limit) - 0.20
    search_peak_limit = float(true_peak_limit) - 0.10
    best: tuple[
        np.ndarray,
        dict[str, Any],
        float,
        float,
        float,
        float,
    ] | None = None
    best_score = math.inf

    for _ in range(10):
        gained = source * db_to_linear(gain_db)
        limited, limiter_info = apply_lookahead_limiter(
            gained, ceiling_dbtp=ceiling, sample_rate=sample_rate
        )
        loudness = integrated_loudness_lufs(limited, sample_rate)
        true_peak = true_peak_dbtp(limited)
        deviation = loudness - float(target_lufs)
        peak_ok = true_peak <= search_peak_limit + 1.0e-9
        score = abs(deviation)
        if peak_ok and score < best_score:
            best_score = score
            best = (limited, limiter_info, gain_db, ceiling, loudness, true_peak)
        if peak_ok and abs(deviation) <= 0.04:
            break

        # Loudness responds monotonically to pre-gain. The true-peak ceiling is
        # tightened only when the oversampled detector finds residual overshoot.
        if not peak_ok:
            ceiling -= max(0.05, true_peak - float(true_peak_limit) + 0.04)
        gain_db -= deviation
        gain_db = min(40.0, max(-40.0, gain_db))

    if best is None:
        # A final conservative pass guarantees delivery even if the iterative
        # search could not find a simultaneously compliant candidate.
        ceiling = float(true_peak_limit) - 0.50
        gained = source * db_to_linear(gain_db)
        limited, limiter_info = apply_lookahead_limiter(
            gained, ceiling_dbtp=ceiling, sample_rate=sample_rate
        )
        loudness = integrated_loudness_lufs(limited, sample_rate)
        true_peak = true_peak_dbtp(limited)
        best = (limited, limiter_info, gain_db, ceiling, loudness, true_peak)

    output, limiter_info, chosen_gain, chosen_ceiling, loudness, true_peak = best
    deviation = loudness - float(target_lufs)
    report = {
        "target_lufs": float(target_lufs),
        "integrated_lufs": float(loudness),
        "deviation_lu": float(deviation),
        "true_peak_limit_dbtp": float(true_peak_limit),
        "true_peak_dbtp": float(true_peak),
        "loudness_compliant": bool(abs(deviation) <= 0.5),
        "true_peak_compliant": bool(
            true_peak <= float(true_peak_limit) + 1.0e-9
        ),
        "pre_gain_db": float(chosen_gain),
        "initial_lufs": float(initial_loudness),
        "ceiling_dbtp": float(chosen_ceiling),
        "limiter": limiter_info,
    }
    report["compliant"] = bool(
        report["loudness_compliant"] and report["true_peak_compliant"]
    )
    return output, report


def channel_peaks(bus: np.ndarray) -> dict[str, float]:
    return {
        "left_dbfs": float(linear_to_db(peak(bus[:, 0]))),
        "right_dbfs": float(linear_to_db(peak(bus[:, 1]))),
    }

