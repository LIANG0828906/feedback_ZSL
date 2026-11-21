"""Drift source diagnosis: clock skew vs event jump vs unknown."""

from __future__ import annotations

import numpy as np

from .errors import E_DRIFT_UNKNOWN
from .fit import fit_drift

CLOCK_SKEW = "CLOCK_SKEW"
EVENT_JUMP = "EVENT_JUMP"


def _mad_sigma(values):
    med = np.median(values)
    return 1.4826 * float(np.median(np.abs(values - med))) + 1e-12


def _best_single_jump(t, d):
    """Search the best single-jump piecewise-linear explanation.

    A jump may contaminate one transition segment (a segment containing
    markers from both sides of the jump), so the search optionally
    excludes the segment on either side of the candidate cut.
    Returns dict(cut, excluded, rms, jump_size, fit_a, fit_b).
    """
    n = len(d)
    best = None
    for cut in range(1, n):
        for excluded in (-1, cut - 1, cut):
            pre = [i for i in range(cut) if i != excluded]
            post = [i for i in range(cut, n) if i != excluded]
            if len(pre) < 2 or len(post) < 2:
                continue
            fit_a = fit_drift(t[pre], d[pre])
            fit_b = fit_drift(t[post], d[post])
            resid = np.concatenate([fit_a["residuals"], fit_b["residuals"]])
            rms = float(np.sqrt(np.mean(resid**2)))
            # tie-break towards the later cut: a contaminated transition
            # segment belongs to the pre-jump side, so the jump takes
            # effect at the first fully post-jump segment
            if (
                best is None
                or rms < best["rms"] - 1e-9
                or (abs(rms - best["rms"]) <= 1e-9 and cut > best["cut"])
            ):
                boundary = t[cut]
                jump = (fit_b["offset"] + fit_b["slope"] * boundary) - (
                    fit_a["offset"] + fit_a["slope"] * boundary
                )
                best = {
                    "cut": cut,
                    "excluded": excluded,
                    "rms": rms,
                    "jump_size": float(jump),
                    "fit_a": fit_a,
                    "fit_b": fit_b,
                }
    return best


def diagnose(centers, delays, residual_threshold, indices=None):
    """Classify the drift pattern of one channel.

    indices: original segment numbers for the (valid) samples, so that
    evidence segments refer to the true segmentation grid.
    Returns a dict with conclusion (CLOCK_SKEW | EVENT_JUMP |
    E_DRIFT_UNKNOWN), evidence segments, confidence intervals and
    residual statistics. Unknown patterns are never defaulted to skew.
    """
    t = np.asarray(centers, dtype=np.float64)
    d = np.asarray(delays, dtype=np.float64)
    if indices is None:
        indices = list(range(len(d)))
    indices = [int(i) for i in indices]
    fit = fit_drift(t, d)
    stats = {
        "residual_rms": fit["residual_rms"],
        "residual_max": float(np.max(np.abs(fit["residuals"]))) if len(d) else 0.0,
        "n_segments": int(len(d)),
    }
    base = {
        "fit": fit,
        "residual_stats": stats,
        "evidence_segments": [],
        "candidate_hypotheses": [],
    }
    if len(d) < 4:
        return {
            **base,
            "conclusion": E_DRIFT_UNKNOWN,
            "candidate_hypotheses": ["insufficient_segments"],
        }
    if stats["residual_max"] <= residual_threshold:
        return {
            **base,
            "conclusion": CLOCK_SKEW,
            "ppm": fit["ppm"],
            "ppm_ci": fit["ppm_ci"],
            "offset": fit["offset"],
            "offset_ci": fit["offset_ci"],
            "evidence_segments": indices,
        }
    diffs = np.diff(d)
    sigma = _mad_sigma(diffs)
    step_thresh = max(residual_threshold, 6.0 * sigma)
    best = _best_single_jump(t, d)
    if (
        best is not None
        and best["rms"] <= residual_threshold
        and abs(best["jump_size"]) > step_thresh
    ):
        cut = best["cut"]
        evidence = [indices[cut - 1], indices[cut]]
        return {
            **base,
            "conclusion": EVENT_JUMP,
            "jump_segment": indices[cut],
            "jump_size": best["jump_size"],
            "jump_size_ci": 2.0 * best["rms"] * float(np.sqrt(2.0)),
            "transition_segment": (
                indices[best["excluded"]] if best["excluded"] >= 0 else None
            ),
            "ppm_before": best["fit_a"]["ppm"],
            "ppm_after": best["fit_b"]["ppm"],
            "piecewise_residual_rms": best["rms"],
            "evidence_segments": evidence,
        }
    step_idx = [i + 1 for i in range(len(diffs)) if abs(diffs[i]) > step_thresh]
    hypotheses = []
    if len(step_idx) > 1:
        hypotheses.append("multiple_event_jumps")
    if stats["residual_rms"] > residual_threshold:
        hypotheses.append("nonlinear_clock_or_noise")
    if not hypotheses:
        hypotheses.append("unmodelled_drift")
    return {
        **base,
        "conclusion": E_DRIFT_UNKNOWN,
        "candidate_hypotheses": hypotheses,
        "step_segments": [indices[i] for i in step_idx],
    }
