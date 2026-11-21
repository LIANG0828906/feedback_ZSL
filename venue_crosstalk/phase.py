"""Cross-band phase continuity and polarity-disambiguation logic."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

from .config import PolarityConfig
from .dsp import BandEstimate
from .errors import E_POLARITY_UNKNOWN
from .utils import sha256_json


def wrap_degrees(value: float) -> float:
    return (value + 180.0) % 360.0 - 180.0


def _distance_from_inversion_deg(phase_deg: float) -> float:
    return abs(abs(wrap_degrees(phase_deg)) - 180.0)


@dataclass(frozen=True)
class PhaseContinuity:
    pair_id: str
    verdict: str
    code: str | None
    inversion_bands: list[str]
    inversion_interval_hz: list[float] | None
    boundary_wraps: list[dict[str, Any]]
    phase_jumps: list[dict[str, Any]]
    group_delay_discontinuities: list[dict[str, Any]]
    candidates: list[dict[str, Any]]
    evidence: list[str]
    content_hash: str

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "PhaseContinuity":
        return cls(
            pair_id=str(data["pair"]),
            verdict=str(data["verdict"]),
            code=None if data.get("code") is None else str(data["code"]),
            inversion_bands=list(data.get("inversion_bands", [])),
            inversion_interval_hz=data.get("inversion_interval_hz"),
            boundary_wraps=list(data.get("boundary_wraps", [])),
            phase_jumps=list(data.get("phase_jumps", [])),
            group_delay_discontinuities=list(
                data.get("group_delay_discontinuities", [])
            ),
            candidates=list(data.get("candidates", [])),
            evidence=list(data.get("evidence", [])),
            content_hash=str(data.get("content_hash", "")),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "pair": self.pair_id,
            "verdict": self.verdict,
            "code": self.code,
            "inversion_bands": self.inversion_bands,
            "inversion_interval_hz": self.inversion_interval_hz,
            "boundary_wraps": self.boundary_wraps,
            "phase_jumps": self.phase_jumps,
            "group_delay_discontinuities": self.group_delay_discontinuities,
            "candidates": self.candidates,
            "evidence": self.evidence,
            "content_hash": self.content_hash,
        }


def analyze_phase_continuity(
    estimates: list[BandEstimate],
    config: PolarityConfig,
) -> PhaseContinuity:
    if not estimates:
        raise ValueError("phase continuity requires at least one band estimate")
    ordered = sorted(estimates, key=lambda item: item.band.low_hz)
    pair_id = ordered[0].pair_id
    phases = [item.phase_deg for item in ordered]
    uncertainties = [item.phase_uncertainty_deg for item in ordered]
    inversion_tolerance = config.inversion_tolerance_deg
    inversion_flags = [
        _distance_from_inversion_deg(phase) <= inversion_tolerance for phase in phases
    ]

    boundary_wraps: list[dict[str, Any]] = []
    phase_jumps: list[dict[str, Any]] = []
    group_delays: list[dict[str, Any]] = []
    for index in range(len(ordered) - 1):
        left = ordered[index]
        right = ordered[index + 1]
        raw_delta = phases[index + 1] - phases[index]
        wrapped_delta = wrap_degrees(raw_delta)
        entry = {
            "bands": [left.band.name, right.band.name],
            "band_indices": [left.band.index, right.band.index],
            "interval_hz": [left.band.high_hz, right.band.low_hz],
            "raw_delta_deg": raw_delta,
            "wrapped_delta_deg": wrapped_delta,
        }
        if (
            abs(raw_delta) >= config.wrap_threshold_deg
            and abs(wrapped_delta) <= config.jump_threshold_deg
        ):
            entry["classification"] = "phase_wrap"
            boundary_wraps.append(entry)
        elif abs(wrapped_delta) > config.jump_threshold_deg:
            entry["classification"] = "phase_jump"
            phase_jumps.append(entry)

        if (
            left.band.high_hz <= config.low_band_max_hz
            or right.band.low_hz <= config.low_band_max_hz
        ):
            delta_hz = right.band.center_hz - left.band.center_hz
            if delta_hz > 0:
                group_delay_ms = math.degrees(math.radians(wrapped_delta)) / (
                    360.0 * delta_hz
                ) * 1000.0
                group_delays.append(
                    {
                        "bands": [left.band.name, right.band.name],
                        "group_delay_ms": group_delay_ms,
                        "delta_hz": delta_hz,
                    }
                )

    group_delay_discontinuities: list[dict[str, Any]] = []
    for left, right in zip(group_delays, group_delays[1:]):
        difference = right["group_delay_ms"] - left["group_delay_ms"]
        if abs(difference) > config.group_delay_jump_ms:
            group_delay_discontinuities.append(
                {
                    "bands": [left["bands"][0], left["bands"][1], right["bands"][1]],
                    "group_delay_left_ms": left["group_delay_ms"],
                    "group_delay_right_ms": right["group_delay_ms"],
                    "jump_ms": difference,
                    "threshold_ms": config.group_delay_jump_ms,
                }
            )

    groups: list[list[int]] = []
    current: list[int] = []
    for index, is_inverted in enumerate(inversion_flags):
        if is_inverted:
            current.append(index)
        elif current:
            groups.append(current)
            current = []
    if current:
        groups.append(current)

    candidates: list[dict[str, Any]] = []
    evidence: list[str] = []
    jump_boundaries = {
        (jump["band_indices"][0], jump["band_indices"][1]) for jump in phase_jumps
    }
    for group in groups:
        if len(group) == len(ordered):
            continue
        enters_group = (
            group[0] - 1,
            group[0],
        ) in jump_boundaries
        leaves_group = (
            group[-1],
            group[-1] + 1,
        ) in jump_boundaries
        if not (enters_group or leaves_group):
            evidence_hint = (
                "bands near +/-180 deg form a continuous phase progression through "
                "a phase wrap and are not treated as a polarity inversion"
            )
            if evidence_hint not in evidence:
                evidence.append(evidence_hint)
            continue
        start_band = ordered[group[0]].band
        end_band = ordered[group[-1]].band
        candidates.append(
            {
                "pair": pair_id,
                "band_names": [ordered[index].band.name for index in group],
                "interval_hz": [start_band.low_hz, end_band.high_hz],
                "evidence": "consecutive bands have phase within tolerance of +/-180 deg",
            }
        )

    verdict = "normal"
    code: str | None = None
    inversion_bands: list[str] = []
    inversion_interval_hz: list[float] | None = None
    if boundary_wraps:
        evidence.append(
            f"{len(boundary_wraps)} boundary phase wrap(s) detected and excluded from "
            "polarity inversion"
        )
    if phase_jumps:
        evidence.append(f"{len(phase_jumps)} cross-band phase jump(s) detected")
    if group_delay_discontinuities:
        evidence.append(
            f"{len(group_delay_discontinuities)} low-band group-delay discontinuity "
            "candidate(s) detected"
        )

    all_bands_inverted = bool(inversion_flags) and all(inversion_flags)
    if all_bands_inverted:
        inversion_bands = [item.band.name for item in ordered]
        inversion_interval_hz = [ordered[0].band.low_hz, ordered[-1].band.high_hz]
        high_uncertainty = any(item > 80.0 for item in uncertainties)
        non_wrap_jumps = [
            jump
            for jump in phase_jumps
            if abs(jump["wrapped_delta_deg"]) > max(
                config.jump_threshold_deg,
                max(uncertainties),
            )
        ]
        if high_uncertainty or non_wrap_jumps:
            verdict = "unknown"
            code = E_POLARITY_UNKNOWN
            evidence.append(
                "all bands are near +/-180 deg but uncertain or discontinuous phase "
                "prevents a reliable inversion decision"
            )
        else:
            verdict = "inverted"
            evidence.append(
                "phase stays near +/-180 deg across all bands; phase wraps at +/-180 "
                "are numerical representations, not separate inversions"
            )
    elif candidates:
        verdict = "unknown"
        code = E_POLARITY_UNKNOWN
        evidence.append(
            "only a subset of bands is near +/-180 deg; a localized clock/phase offset "
            "cannot be distinguished from a partial polarity reversal"
        )
    elif phase_jumps:
        verdict = "unknown"
        code = E_POLARITY_UNKNOWN
        evidence.append(
            "cross-band phase jumps exist without a full-band inversion signature"
        )
    elif group_delay_discontinuities:
        verdict = "unknown"
        code = E_POLARITY_UNKNOWN
        evidence.append(
            "low-band group delay is discontinuous, so polarity is not uniquely determined"
        )
    else:
        evidence.append(
            "phase is continuous through crossover boundaries after unwrapping; "
            "detected wraps are boundary artifacts"
        )

    payload = {
        "pair": pair_id,
        "verdict": verdict,
        "code": code,
        "inversion_bands": inversion_bands,
        "inversion_interval_hz": inversion_interval_hz,
        "boundary_wraps": boundary_wraps,
        "phase_jumps": phase_jumps,
        "group_delay_discontinuities": group_delay_discontinuities,
        "candidates": candidates,
        "evidence": evidence,
    }
    return PhaseContinuity(
        pair_id=pair_id,
        verdict=verdict,
        code=code,
        inversion_bands=inversion_bands,
        inversion_interval_hz=inversion_interval_hz,
        boundary_wraps=boundary_wraps,
        phase_jumps=phase_jumps,
        group_delay_discontinuities=group_delay_discontinuities,
        candidates=candidates,
        evidence=evidence,
        content_hash=sha256_json(payload),
    )
