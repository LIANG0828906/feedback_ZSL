from __future__ import annotations

import math
from typing import Dict, Iterable, List, Optional

import numpy as np

from .models import CouplingEstimate, PhaseFinding, PhasePolicy
from .util import wrap_deg


def _circular_consistency(phases: Iterable[float]) -> tuple[float, float]:
    radians = np.radians(np.asarray(list(phases), dtype=np.float64))
    if radians.size == 0:
        return 0.0, 0.0
    vector = np.mean(np.exp(1j * radians))
    center = float(math.degrees(math.atan2(vector.imag, vector.real)))
    consistency = float(abs(vector))
    return center, consistency


def analyze_phase_continuity(
    estimates: Iterable[CouplingEstimate],
    policy: PhasePolicy,
    main_receivers: Optional[Dict[str, str]] = None,
) -> tuple[List[PhaseFinding], Dict[str, object]]:
    main_map = main_receivers or {}
    by_pair: Dict[str, List[CouplingEstimate]] = {}
    for estimate in estimates:
        if main_map.get(estimate.source, estimate.source) == estimate.receiver:
            continue
        by_pair.setdefault(estimate.pair_key, []).append(estimate)
    findings: List[PhaseFinding] = []
    pair_summary: Dict[str, Dict[str, object]] = {}
    ambiguous_pairs = set()
    for pair_key in sorted(by_pair):
        series = sorted(by_pair[pair_key], key=lambda item: item.low_hz)
        if not series:
            continue
        phases = [item.phase_deg for item in series]
        unwrapped = np.degrees(np.unwrap(np.radians(phases)))
        center, consistency = _circular_consistency(phases)
        phase_spread = float(np.max(unwrapped) - np.min(unwrapped))
        global_polarity = (
            abs(wrap_deg(center)) >= 135.0
            and consistency >= 0.75
            and phase_spread <= 60.0
        )
        if global_polarity:
            findings.append(
                PhaseFinding(
                    source=series[0].source,
                    receiver=series[0].receiver,
                    kind="polarity_reversed",
                    band_from=series[0].band_id,
                    band_to=series[-1].band_id,
                    phase_from_deg=series[0].phase_deg,
                    phase_to_deg=series[-1].phase_deg,
                    wrapped_delta_deg=None,
                    raw_delta_deg=None,
                    confidence=round(consistency, 6),
                    severity="warning",
                    candidate_causes=("true_polarity_reversal",),
                )
            )
        for index in range(len(series) - 1):
            left = series[index]
            right = series[index + 1]
            raw_delta = right.phase_deg - left.phase_deg
            wrapped_delta = wrap_deg(raw_delta)
            wrap_artifact = abs(raw_delta) > 300.0 and abs(wrapped_delta) < 90.0
            group_delay_jump = abs(
                right.group_delay_samples - left.group_delay_samples
            )
            both_low = (
                left.low_hz < policy.low_frequency_max_hz
                and right.low_hz < policy.low_frequency_max_hz
            )
            if (
                both_low
                and group_delay_jump > policy.group_delay_jump_samples
                and not wrap_artifact
            ):
                confidence = max(
                    0.2,
                    min(
                        0.95,
                        1.0
                        - policy.group_delay_jump_samples
                        / max(group_delay_jump, 1e-9),
                    ),
                )
                findings.append(
                    PhaseFinding(
                        source=left.source,
                        receiver=left.receiver,
                        kind="group_delay_discontinuity",
                        band_from=left.band_id,
                        band_to=right.band_id,
                        phase_from_deg=left.phase_deg,
                        phase_to_deg=right.phase_deg,
                        wrapped_delta_deg=wrapped_delta,
                        raw_delta_deg=raw_delta,
                        confidence=round(confidence, 6),
                        severity="warning",
                        candidate_causes=(
                            "geometry_delay_residual",
                            "band_transition_energy_leakage",
                        ),
                        group_delay_from_samples=left.group_delay_samples,
                        group_delay_to_samples=right.group_delay_samples,
                    )
                )
                if confidence < policy.ambiguity_confidence:
                    ambiguous_pairs.add(pair_key)
            if wrap_artifact:
                findings.append(
                    PhaseFinding(
                        source=left.source,
                        receiver=left.receiver,
                        kind="boundary_phase_wrap",
                        band_from=left.band_id,
                        band_to=right.band_id,
                        phase_from_deg=left.phase_deg,
                        phase_to_deg=right.phase_deg,
                        wrapped_delta_deg=wrapped_delta,
                        raw_delta_deg=raw_delta,
                        confidence=0.98,
                        severity="info",
                        candidate_causes=("analysis_boundary_wrapping",),
                    )
                )
                continue
            if abs(wrapped_delta) <= policy.jump_threshold_deg:
                continue
            polarity_like = (
                abs(abs(wrapped_delta) - 180.0) <= policy.polarity_tolerance_deg
            )
            leakage_evidence = max(
                left.leakage_fraction,
                right.leakage_fraction,
            )
            if polarity_like:
                confidence = 0.52
                if leakage_evidence > 0.15:
                    confidence = 0.64
                findings.append(
                    PhaseFinding(
                        source=left.source,
                        receiver=left.receiver,
                        kind="polarity_flip_candidate",
                        band_from=left.band_id,
                        band_to=right.band_id,
                        phase_from_deg=left.phase_deg,
                        phase_to_deg=right.phase_deg,
                        wrapped_delta_deg=wrapped_delta,
                        raw_delta_deg=raw_delta,
                        confidence=confidence,
                        severity="error",
                        candidate_causes=(
                            "true_polarity_reversal",
                            "crossover_boundary_phase_jump",
                            "band_transition_energy_leakage",
                        ),
                    )
                )
                ambiguous_pairs.add(pair_key)
            else:
                findings.append(
                    PhaseFinding(
                        source=left.source,
                        receiver=left.receiver,
                        kind="phase_jump",
                        band_from=left.band_id,
                        band_to=right.band_id,
                        phase_from_deg=left.phase_deg,
                        phase_to_deg=right.phase_deg,
                        wrapped_delta_deg=wrapped_delta,
                        raw_delta_deg=raw_delta,
                        confidence=0.82,
                        severity="warning",
                        candidate_causes=("band_coupling_phase_change",),
                    )
                )
        pair_summary[pair_key] = {
            "phase_center_deg": center,
            "circular_consistency": consistency,
            "phase_spread_deg": phase_spread,
            "polarity": -1 if global_polarity else 1,
            "ambiguous": pair_key in ambiguous_pairs,
        }
    summary = {
        "pair_count": len(pair_summary),
        "findings": len(findings),
        "ambiguous_pairs": sorted(ambiguous_pairs),
        "pairs": pair_summary,
    }
    return findings, summary
