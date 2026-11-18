"""Intra-group relative delay / polarity estimation and phase correction."""

from __future__ import annotations

import numpy as np
from scipy import signal as sp_signal

from .dsp import frac_delay

# Only small residual sub-sample/few-sample timing errors are expected after
# integer transient alignment; a bounded search avoids locking onto the next
# period of the rhythmic waveform.
# Integer transient alignment leaves only a sub-sample/few-sample residual;
# a tight search on the 20-120 Hz band avoids broadband click artifacts.
MAX_DELAY_SAMPLES = 2


def _low_band(x: np.ndarray, sr: int) -> np.ndarray:
    """Zero-phase 20-120 Hz band-pass used for phase analysis."""
    nyq = sr / 2.0
    low = max(20.0 / nyq, 1e-4)
    high = min(120.0 / nyq, 0.999)
    sos = sp_signal.butter(4, [low, high], btype="bandpass", output="sos")
    return sp_signal.sosfiltfilt(sos, x)


def low_band_energy(x: np.ndarray, sr: int, strong_section: tuple[int, int]) -> float:
    lo, hi = strong_section
    band = _low_band(x[lo:hi], sr)
    return float(np.mean(band * band))


def estimate_relative(reference: np.ndarray, member: np.ndarray, sr: int,
                      strong_section: tuple[int, int],
                      center_lag: int = 0) -> dict:
    """Estimate member-vs-reference delay (samples, signed) and polarity.

    The returned ``relative_delay_samples`` is positive when the member
    arrives after the reference; the internal cross-correlation lag has the
    opposite sign and is converted before returning.
    """
    lo, hi = strong_section
    ref_band = _low_band(reference[lo:hi], sr)
    mem_band = _low_band(member[lo:hi], sr)
    # Zero-padded correlation (no circular wrap) over the bounded lag range.
    lags = np.arange(center_lag - MAX_DELAY_SAMPLES, center_lag + MAX_DELAY_SAMPLES + 1)
    correlation = np.empty(lags.shape[0], dtype=np.float64)
    for index, lag in enumerate(lags):
        if lag >= 0:
            correlation[index] = np.dot(
                ref_band[lag:], mem_band[: mem_band.shape[0] - lag]
            )
        else:
            correlation[index] = np.dot(
                ref_band[:lag], mem_band[-lag:]
            )
    # Envelope drift can bias the raw correlation magnitude toward the search
    # boundary. Penalize distance from the alignment-derived center lag so a
    # genuinely aligned pair (residual delay zero) wins, while a real
    # sub-sample residual can still move the estimate by one sample.
    center_index = int(np.where(lags == center_lag)[0][0])
    center_magnitude = abs(correlation[center_index]) + 1e-20
    scores = np.abs(correlation) / (
        center_magnitude * (1.0 + 0.02 * np.abs(lags - center_lag))
    )
    best = int(np.argmax(scores))
    lag = int(lags[best])
    polarity = 1 if correlation[best] >= 0 else -1
    # Parabolic interpolation around the best integer lag -> sub-sample delay.
    fractional = float(lag)
    if 0 < best < len(correlation) - 1:
        y0, y1, y2 = scores[best - 1:best + 2]
        denom = y0 - 2.0 * y1 + y2
        if abs(denom) > 1e-18:
            fractional += 0.5 * (y0 - y2) / denom
    # Positive member delay produces a negative cross-correlation lag.
    relative_delay = float(fractional)
    return {
        "relative_delay_samples": relative_delay,
        "integer_lag_samples": lag,
        "polarity": int(polarity),
        "correlation_strength": float(abs(correlation[best])),
    }


def correct_member(member: np.ndarray, estimate: dict) -> np.ndarray:
    """Apply polarity flip and compensating fractional delay.

    If the member arrives ``d`` samples late relative to the reference it is
    advanced by ``d`` (negative delay). Total transient displacement is bounded
    by the fractional remainder (<= 1 sample) because the integer-lag component
    only restores the timing the member was injected with.
    """
    corrected = frac_delay(member, -estimate["relative_delay_samples"])
    if estimate["polarity"] < 0:
        corrected = -corrected
    return corrected


def phase_correct_groups(tracks: list[dict], aligned: list[np.ndarray],
                         sr: int, strong_section_s: tuple[float, float]
                         , alignment_records: list[dict] | None = None
                         ) -> tuple[list[np.ndarray], list[dict], dict]:
    """Correct every declared group against its first member (reference)."""
    strong = (
        int(round(strong_section_s[0] * sr)),
        int(round(strong_section_s[1] * sr)),
    )
    groups: dict[str, list[int]] = {}
    for index, track in enumerate(tracks):
        name = track.get("phase_group")
        if name:
            groups.setdefault(name, []).append(index)

    corrected_signals = list(aligned)
    pair_reports: list[dict] = []
    gain_records: list[dict] = []
    for group_name, members in groups.items():
        ref_index = members[0]
        before_sum = np.sum(
            [aligned[i][strong[0]:strong[1]] for i in members], axis=0
        )
        for member_index in members[1:]:
            center_lag = 0
            if alignment_records is not None:
                ref_shift = alignment_records[ref_index]["integer_shift_samples"]
                mem_shift = alignment_records[member_index]["integer_shift_samples"]
                # Coarse cross-correlation lag derived from integer shifts.
                center_lag = int(mem_shift - ref_shift)
            estimate = estimate_relative(
                aligned[ref_index], aligned[member_index], sr, strong,
                center_lag=center_lag,
            )
            pair_reports.append(
                {
                    "group": group_name,
                    "reference_file": tracks[ref_index]["file"],
                    "member_file": tracks[member_index]["file"],
                    **estimate,
                }
            )
            corrected_signals[member_index] = correct_member(
                aligned[member_index], estimate
            )
        after_sum = np.sum(
            [corrected_signals[i][strong[0]:strong[1]] for i in members],
            axis=0,
        )
        energy_before = low_band_energy(before_sum, sr, (0, strong[1] - strong[0]))
        energy_after = low_band_energy(after_sum, sr, (0, strong[1] - strong[0]))
        gain_db = 10.0 * np.log10((energy_after + 1e-20) / (energy_before + 1e-20))
        gain_records.append(
            {
                "group": group_name,
                "low_band_hz": [20, 120],
                "energy_before": energy_before,
                "energy_after": energy_after,
                "low_band_gain_db": float(gain_db),
                "meets_6db": bool(gain_db >= 6.0),
            }
        )
    return corrected_signals, pair_reports, {"groups": gain_records}
