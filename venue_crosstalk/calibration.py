"""Bounded cancellation-gain solver and residual crosstalk budget."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

from .config import AnalysisConfig
from .dsp import BandEstimate
from .errors import E_CALIB_INFEASIBLE
from .phase import PhaseContinuity
from .utils import sha256_json


@dataclass(frozen=True)
class ConstraintStep:
    order: int
    name: str
    limit: float | str
    gain_before: float
    gain_after: float
    achieved_suppression_db: float
    hit_target: bool
    note: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "order": self.order,
            "name": self.name,
            "limit": self.limit,
            "gain_before": self.gain_before,
            "gain_after": self.gain_after,
            "achieved_suppression_db": self.achieved_suppression_db,
            "hit_target": self.hit_target,
            "note": self.note,
        }


@dataclass(frozen=True)
class PairBandCalibration:
    pair_id: str
    band: BandEstimate
    target_suppression_db: float
    polarity_verdict: str
    polarity_setting: str
    cancel_gain: float
    residual_magnitude: float
    residual_energy: float
    achieved_suppression_db: float
    gap_db: float
    primary_suppression_db: float
    feasible: bool
    constraint_order: list[str]
    constraints: list[ConstraintStep]
    error_code: str | None
    reason: str
    content_hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "pair": self.pair_id,
            "band": self.band.band.name,
            "band_index": self.band.band.index,
            "band_low_hz": self.band.band.low_hz,
            "band_high_hz": self.band.band.high_hz,
            "coupling_magnitude": self.band.magnitude,
            "coupling_db": self.band.magnitude_db,
            "coupling_phase_deg": self.band.phase_deg,
            "coupling_phase_rad": self.band.phase_rad,
            "magnitude_uncertainty_db": self.band.magnitude_uncertainty_db,
            "phase_uncertainty_deg": self.band.phase_uncertainty_deg,
            "polarity_verdict": self.polarity_verdict,
            "polarity_setting": self.polarity_setting,
            "cancel_polarity_inverted": self.polarity_setting.endswith("inverted"),
            "detected_polarity_inverted": self.polarity_verdict == "inverted",
            "cancel_gain": self.cancel_gain,
            "cancel_gain_linear": self.cancel_gain,
            "cancel_gain_db": 20.0 * math.log10(max(self.cancel_gain, 1e-15)),
            "residual_magnitude": self.residual_magnitude,
            "residual_db": 20.0 * math.log10(max(self.residual_magnitude, 1e-15)),
            "residual_energy": self.residual_energy,
            "target_suppression_db": self.target_suppression_db,
            "achieved_suppression_db": self.achieved_suppression_db,
            "suppression_db": self.achieved_suppression_db,
            "gap_db": self.gap_db,
            "met": self.feasible,
            "primary_suppression_db": self.primary_suppression_db,
            "constraint_order": self.constraint_order,
            "constraints": [step.to_dict() for step in self.constraints],
            "feasible": self.feasible,
            "error_code": self.error_code,
            "reason": self.reason,
            "content_hash": self.content_hash,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "PairBandCalibration":
        from .config import BandSpec
        from .dsp import BandEstimate

        band = BandSpec(
            index=int(data["band_index"]),
            name=str(data["band"]),
            low_hz=float(data["band_low_hz"]),
            high_hz=float(data["band_high_hz"]),
        )
        phase = float(data.get("coupling_phase_rad", 0.0))
        magnitude = float(data["coupling_magnitude"])
        estimate = BandEstimate(
            pair_id=str(data["pair"]),
            band=band,
            magnitude=magnitude,
            magnitude_db=float(data.get("coupling_db", 0.0)),
            phase_rad=phase,
            phase_deg=float(data.get("coupling_phase_deg", 0.0)),
            magnitude_uncertainty_db=float(data.get("magnitude_uncertainty_db", 0.0)),
            phase_uncertainty_deg=float(data.get("phase_uncertainty_deg", 0.0)),
            magnitude_low=magnitude,
            magnitude_high=magnitude,
            phase_low_deg=float(data.get("coupling_phase_deg", 0.0)),
            phase_high_deg=float(data.get("coupling_phase_deg", 0.0)),
            coherence=0.0,
            sample_count=0,
            effective_samples=0.0,
            quantization_step=1.0 / 32768.0,
            transition_hz=0.0,
        )
        constraints = [
            ConstraintStep(
                order=int(item["order"]),
                name=str(item["name"]),
                limit=item["limit"],
                gain_before=float(item["gain_before"]),
                gain_after=float(item["gain_after"]),
                achieved_suppression_db=float(item["achieved_suppression_db"]),
                hit_target=bool(item["hit_target"]),
                note=str(item["note"]),
            )
            for item in data.get("constraints", [])
        ]
        return cls(
            pair_id=str(data["pair"]),
            band=estimate,
            target_suppression_db=float(data["target_suppression_db"]),
            polarity_verdict=str(data["polarity_verdict"]),
            polarity_setting=str(data["polarity_setting"]),
            cancel_gain=float(data["cancel_gain_linear"]),
            residual_magnitude=float(data["residual_magnitude"]),
            residual_energy=float(data["residual_energy"]),
            achieved_suppression_db=float(data["achieved_suppression_db"]),
            gap_db=float(data["gap_db"]),
            primary_suppression_db=float(data["primary_suppression_db"]),
            feasible=bool(data["feasible"]),
            constraint_order=list(data["constraint_order"]),
            constraints=constraints,
            error_code=None if data.get("error_code") is None else str(data["error_code"]),
            reason=str(data["reason"]),
            content_hash=str(data.get("content_hash", "")),
        )


def _suppression_db(coupling: float, residual: float) -> float:
    if coupling <= 1e-15:
        return 120.0
    return 20.0 * math.log10(max(coupling, 1e-15) / max(residual, 1e-15))


def _primary_suppression_db(gain: float, leakage: float) -> float:
    if leakage <= 0.0 or gain <= 0.0:
        return 0.0
    remaining = abs(1.0 - gain * leakage)
    if remaining <= 1e-15:
        return 120.0
    if remaining > 1.0:
        return 0.0
    return -20.0 * math.log10(remaining)


def calibrate_pair(
    estimate: BandEstimate,
    phase: PhaseContinuity,
    config: AnalysisConfig,
    *,
    primary_leakage: float | None = None,
    target_suppression_db: float | None = None,
) -> PairBandCalibration:
    """Solve gain and polarity in a fixed, auditable constraint order."""

    target = (
        config.target_suppression_db
        if target_suppression_db is None
        else float(target_suppression_db)
    )
    leakage = config.primary_leakage if primary_leakage is None else float(primary_leakage)
    magnitude = estimate.magnitude
    phase_rad = estimate.phase_rad
    cosine = math.cos(phase_rad)
    if abs(cosine) < 1e-12:
        ideal_gain = 0.0
        polarity_setting = "normal"
    elif cosine >= 0.0:
        ideal_gain = magnitude * cosine
        polarity_setting = "inverted"
    else:
        ideal_gain = -magnitude * cosine
        polarity_setting = "normal"
    if phase.verdict == "unknown":
        polarity_setting = f"unknown:{polarity_setting}"

    def current_residual(gain: float) -> float:
        sign = -1.0 if polarity_setting.endswith("inverted") else 1.0
        complex_residual = complex(
            magnitude * math.cos(phase_rad) + sign * gain,
            magnitude * math.sin(phase_rad),
        )
        # Keep a small, explicitly derived floor from estimator uncertainty and
        # quantization. It prevents reporting non-physical infinite suppression.
        magnitude_uncertainty = magnitude * (
            10.0 ** (estimate.magnitude_uncertainty_db / 20.0) - 1.0
        )
        phase_guard = (
            magnitude * math.sin(min(math.radians(estimate.phase_uncertainty_deg), math.pi / 4))
        )
        floor = math.sqrt(
            (0.05 * magnitude_uncertainty) ** 2
            + max(estimate.quantization_step / math.sqrt(12.0), 1e-12) ** 2
            + (0.02 * phase_guard) ** 2
        )
        return math.hypot(abs(complex_residual), floor)

    constraints: list[ConstraintStep] = []
    order = [
        "phase_polarity",
        "cancel_gain_limit",
        "primary_suppression_limit",
        "target_suppression",
    ]
    gain = ideal_gain
    constraints.append(
        ConstraintStep(
            order=1,
            name=order[0],
            limit=phase.verdict,
            gain_before=gain,
            gain_after=gain,
            achieved_suppression_db=_suppression_db(magnitude, current_residual(gain)),
            hit_target=_suppression_db(magnitude, current_residual(gain)) >= target,
            note=f"polarity setting={polarity_setting}; phase decision={phase.verdict}",
        )
    )

    before = gain
    gain = min(gain, config.max_cancel_gain)
    constraints.append(
        ConstraintStep(
            order=2,
            name=order[1],
            limit=config.max_cancel_gain,
            gain_before=before,
            gain_after=gain,
            achieved_suppression_db=_suppression_db(magnitude, current_residual(gain)),
            hit_target=_suppression_db(magnitude, current_residual(gain)) >= target,
            note="cancel gain clipped to declared maximum",
        )
    )

    before = gain
    if leakage <= 0:
        primary_limit_gain = math.inf
    else:
        floor_ratio = 10.0 ** (-config.max_primary_suppression_db / 20.0)
        primary_limit_gain = max(0.0, (1.0 - floor_ratio) / leakage)
    gain = min(gain, primary_limit_gain)
    constraints.append(
        ConstraintStep(
            order=3,
            name=order[2],
            limit=config.max_primary_suppression_db,
            gain_before=before,
            gain_after=gain,
            achieved_suppression_db=_suppression_db(magnitude, current_residual(gain)),
            hit_target=_suppression_db(magnitude, current_residual(gain)) >= target,
            note=(
                f"primary leakage={leakage}; max gain allowed by primary range="
                f"{primary_limit_gain:.10g}"
            ),
        )
    )

    residual = current_residual(gain)
    achieved = _suppression_db(magnitude, residual)
    feasible = achieved + 1e-12 >= target
    gap = max(0.0, target - achieved)
    constraints.append(
        ConstraintStep(
            order=4,
            name=order[3],
            limit=target,
            gain_before=gain,
            gain_after=gain,
            achieved_suppression_db=achieved,
            hit_target=feasible,
            note="target suppression evaluated against central estimate and guard floor",
        )
    )
    primary_suppression = _primary_suppression_db(gain, leakage)
    reason = (
        "target met"
        if feasible
        else f"target shortfall {gap:.3f} dB after all declared constraints"
    )
    if primary_suppression > config.max_primary_suppression_db + 1e-9:
        feasible = False
        gap = max(gap, primary_suppression - config.max_primary_suppression_db)
        reason = "primary-channel suppression range exceeded"
    error_code = None if feasible else E_CALIB_INFEASIBLE
    payload = {
        "pair": estimate.pair_id,
        "band": estimate.band.name,
        "coupling": estimate.magnitude,
        "phase": estimate.phase_rad,
        "target": target,
        "polarity": polarity_setting,
        "verdict": phase.verdict,
        "gain": gain,
        "residual": residual,
        "primary_leakage": leakage,
        "max_gain": config.max_cancel_gain,
        "primary_limit_db": config.max_primary_suppression_db,
        "constraints": [step.to_dict() for step in constraints],
    }
    return PairBandCalibration(
        pair_id=estimate.pair_id,
        band=estimate,
        target_suppression_db=target,
        polarity_verdict=phase.verdict,
        polarity_setting=polarity_setting,
        cancel_gain=gain,
        residual_magnitude=residual,
        residual_energy=residual * residual,
        achieved_suppression_db=achieved,
        gap_db=gap,
        primary_suppression_db=primary_suppression,
        feasible=feasible,
        constraint_order=order,
        constraints=constraints,
        error_code=error_code,
        reason=reason,
        content_hash=sha256_json(payload),
    )


@dataclass(frozen=True)
class ResidualBudget:
    band_name: str
    band_index: int
    target_suppression_db: float
    pair_budgets: list[dict[str, Any]]
    pickup_budgets: list[dict[str, Any]]
    shortfall_pairs: list[dict[str, Any]]
    shortfall_pickups: list[dict[str, Any]]
    content_hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "band": self.band_name,
            "band_index": self.band_index,
            "target_suppression_db": self.target_suppression_db,
            "pairs": self.pair_budgets,
            "pickups": self.pickup_budgets,
            "shortfall_pairs": self.shortfall_pairs,
            "shortfall_pickups": self.shortfall_pickups,
            "content_hash": self.content_hash,
        }


def build_residual_budget(
    band_name: str,
    band_index: int,
    target_suppression_db: float,
    calibrations: list[PairBandCalibration],
    pair_metadata: dict[str, tuple[str, str]] | None = None,
) -> ResidualBudget:
    pair_budgets: list[dict[str, Any]] = []
    pickup_energy: dict[str, float] = {}
    pickup_contributors: dict[str, list[dict[str, Any]]] = {}
    shortfall_pairs: list[dict[str, Any]] = []
    for item in calibrations:
        if pair_metadata is not None and item.pair_id in pair_metadata:
            source, pickup = pair_metadata[item.pair_id]
        else:
            source = item.pair_id.split(">", 1)[0]
            pickup = item.pair_id.split(">", 1)[1] if ">" in item.pair_id else ""
        entry = {
            "pair": item.pair_id,
            "source": source,
            "pickup": pickup,
            "coupling_magnitude": item.band.magnitude,
            "cancel_gain_linear": item.cancel_gain,
            "polarity_setting": item.polarity_setting,
            "residual_magnitude": item.residual_magnitude,
            "residual_energy": item.residual_energy,
            "target_suppression_db": target_suppression_db,
            "achieved_suppression_db": item.achieved_suppression_db,
            "gap_db": item.gap_db,
            "met": item.feasible,
        }
        pair_budgets.append(entry)
        pickup = entry["pickup"]
        pickup_energy[pickup] = pickup_energy.get(pickup, 0.0) + item.residual_energy
        pickup_contributors.setdefault(pickup, []).append(entry)
        if not item.feasible:
            shortfall_pairs.append(
                {
                    "pair": item.pair_id,
                    "band": band_name,
                    "gap_db": item.gap_db,
                    "achieved_suppression_db": item.achieved_suppression_db,
                }
            )
    pickup_budgets: list[dict[str, Any]] = []
    shortfall_pickups: list[dict[str, Any]] = []
    for pickup in sorted(pickup_energy):
        energy = pickup_energy[pickup]
        # The direct/main reference path is normalized to unit energy per band.
        suppression = -10.0 * math.log10(max(energy, 1e-24))
        met = suppression + 1e-12 >= target_suppression_db
        entry = {
            "pickup": pickup,
            "band": band_name,
            "residual_energy": energy,
            "residual_rms": math.sqrt(energy),
            "relative_suppression_db": suppression,
            "target_suppression_db": target_suppression_db,
            "gap_db": max(0.0, target_suppression_db - suppression),
            "met": met,
            "contributors": [item["pair"] for item in pickup_contributors[pickup]],
        }
        pickup_budgets.append(entry)
        if not met:
            shortfall_pickups.append(
                {
                    "pickup": pickup,
                    "band": band_name,
                    "gap_db": entry["gap_db"],
                    "relative_suppression_db": suppression,
                }
            )
    payload = {
        "band": band_name,
        "band_index": band_index,
        "target": target_suppression_db,
        "pairs": pair_budgets,
        "pickups": pickup_budgets,
    }
    return ResidualBudget(
        band_name=band_name,
        band_index=band_index,
        target_suppression_db=target_suppression_db,
        pair_budgets=pair_budgets,
        pickup_budgets=pickup_budgets,
        shortfall_pairs=shortfall_pairs,
        shortfall_pickups=shortfall_pickups,
        content_hash=sha256_json(payload),
    )
