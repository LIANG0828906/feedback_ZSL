from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional, Sequence, Tuple

import numpy as np

from .util import content_hash, wrap_deg


@dataclass(frozen=True)
class BandSpec:
    id: str
    low_hz: float
    high_hz: float
    transition_hz: float

    def to_dict(self) -> Dict[str, Any]:
        return {
            "id": self.id,
            "low_hz": self.low_hz,
            "high_hz": self.high_hz,
            "transition_hz": self.transition_hz,
        }

    @property
    def width_hz(self) -> float:
        return self.high_hz - self.low_hz

    @property
    def center_hz(self) -> float:
        return (self.low_hz + self.high_hz) / 2.0

    @property
    def hash(self) -> str:
        return content_hash(self.to_dict())


@dataclass(frozen=True)
class PhasePolicy:
    jump_threshold_deg: float = 120.0
    polarity_tolerance_deg: float = 35.0
    low_frequency_max_hz: float = 500.0
    group_delay_jump_samples: float = 2.5
    ambiguity_confidence: float = 0.6


@dataclass(frozen=True)
class CalibrationPolicy:
    gain_step_db: float = 0.05
    residual_floor: float = 1e-12


@dataclass(frozen=True)
class ScenarioConfig:
    bands: Tuple[BandSpec, ...]
    max_cancellation_gain_db: float
    target_suppression_db: float
    measurement_layout: str
    min_band_samples: int
    phase: PhasePolicy
    calibration: CalibrationPolicy
    estimator_version: str = "band-ls-v1"

    @property
    def band_ids(self) -> List[str]:
        return [band.id for band in self.bands]

    def to_dict(self) -> Dict[str, Any]:
        return {
            "schema_version": 1,
            "measurement_layout": self.measurement_layout,
            "max_cancellation_gain_db": self.max_cancellation_gain_db,
            "target_suppression_db": self.target_suppression_db,
            "min_band_samples": self.min_band_samples,
            "estimator_version": self.estimator_version,
            "bands": [band.to_dict() for band in self.bands],
            "phase": {
                "jump_threshold_deg": self.phase.jump_threshold_deg,
                "polarity_tolerance_deg": self.phase.polarity_tolerance_deg,
                "low_frequency_max_hz": self.phase.low_frequency_max_hz,
                "group_delay_jump_samples": self.phase.group_delay_jump_samples,
                "ambiguity_confidence": self.phase.ambiguity_confidence,
            },
            "calibration": {
                "gain_step_db": self.calibration.gain_step_db,
                "residual_floor": self.calibration.residual_floor,
            },
        }

    @property
    def hash(self) -> str:
        return content_hash(self.to_dict())


@dataclass(frozen=True)
class AlignmentResult:
    source: str
    receiver: str
    main_receiver: str
    path_label: str
    channel: int
    layout: str
    sample_rate: int
    component_hash: str
    arrival_samples: int
    geometry_delay_samples: int
    alignment_offset_samples: int
    distance_m: float
    speed_of_sound_m_s: float
    position_distance_m: Optional[float]
    distance_delta_m: Optional[float]
    distance_basis: str
    confidence: str
    confidence_score: float
    measured_minus_geometry_samples: int
    valid_samples: int
    source_rms: float
    component_rms: float
    aligned_component: np.ndarray = field(repr=False, compare=False)
    aligned_reference: np.ndarray = field(repr=False, compare=False)

    @property
    def pair_key(self) -> str:
        return f"{self.source}|{self.receiver}"

    @property
    def is_main(self) -> bool:
        return self.receiver == self.main_receiver

    def public_dict(self) -> Dict[str, Any]:
        return {
            "source": self.source,
            "receiver": self.receiver,
            "main_receiver": self.main_receiver,
            "is_main": self.is_main,
            "path": self.path_label,
            "channel": self.channel,
            "layout": self.layout,
            "sample_rate": self.sample_rate,
            "component_hash": self.component_hash,
            "arrival_samples": self.arrival_samples,
            "geometry_delay_samples": self.geometry_delay_samples,
            "alignment_offset_samples": self.alignment_offset_samples,
            "distance_m": self.distance_m,
            "speed_of_sound_m_s": self.speed_of_sound_m_s,
            "position_distance_m": self.position_distance_m,
            "distance_delta_m": self.distance_delta_m,
            "distance_basis": self.distance_basis,
            "confidence": self.confidence,
            "confidence_score": self.confidence_score,
            "measured_minus_geometry_samples": self.measured_minus_geometry_samples,
            "valid_samples": self.valid_samples,
            "source_rms": self.source_rms,
            "component_rms": self.component_rms,
        }

    @property
    def hash(self) -> str:
        return content_hash(self.public_dict())


@dataclass(frozen=True)
class CouplingEstimate:
    source: str
    receiver: str
    band_id: str
    low_hz: float
    high_hz: float
    strength_db: float
    strength_ci_db: Tuple[float, float]
    phase_deg: float
    phase_ci_deg: Tuple[float, float]
    coherence: float
    group_delay_samples: float
    valid_samples: int
    active_bins: int
    quantization_relative: float
    leakage_fraction: float
    transition_fraction: float
    estimator_version: str

    def to_dict(self) -> Dict[str, Any]:
        return {
            "source": self.source,
            "receiver": self.receiver,
            "band_id": self.band_id,
            "low_hz": self.low_hz,
            "high_hz": self.high_hz,
            "strength_db": self.strength_db,
            "strength_ci_db": list(self.strength_ci_db),
            "phase_deg": self.phase_deg,
            "phase_ci_deg": list(self.phase_ci_deg),
            "coherence": self.coherence,
            "group_delay_samples": self.group_delay_samples,
            "valid_samples": self.valid_samples,
            "active_bins": self.active_bins,
            "quantization_relative": self.quantization_relative,
            "leakage_fraction": self.leakage_fraction,
            "transition_fraction": self.transition_fraction,
            "estimator_version": self.estimator_version,
        }

    @property
    def pair_key(self) -> str:
        return f"{self.source}|{self.receiver}"

    @property
    def hash(self) -> str:
        return content_hash(self.to_dict())


@dataclass(frozen=True)
class BudgetEntry:
    source: str
    receiver: str
    band_id: str
    coupling_db: float
    coupling_amplitude: float
    phase_deg: float
    phase_cos_abs: float
    target_suppression_db: float
    required_gain_db: float
    max_gain_db: float
    achievable_suppression_db: float
    residual_amplitude: float
    residual_energy: float
    residual_crosstalk_db: float
    suppression_db: float
    status: str
    reason: str
    polarity_hint: int

    def to_dict(self) -> Dict[str, Any]:
        return {
            "source": self.source,
            "receiver": self.receiver,
            "band_id": self.band_id,
            "coupling_db": self.coupling_db,
            "coupling_amplitude": self.coupling_amplitude,
            "phase_deg": self.phase_deg,
            "phase_cos_abs": self.phase_cos_abs,
            "target_suppression_db": self.target_suppression_db,
            "required_gain_db": self.required_gain_db,
            "max_gain_db": self.max_gain_db,
            "achievable_suppression_db": self.achievable_suppression_db,
            "residual_amplitude": self.residual_amplitude,
            "residual_energy": self.residual_energy,
            "residual_crosstalk_db": self.residual_crosstalk_db,
            "suppression_db": self.suppression_db,
            "status": self.status,
            "reason": self.reason,
            "polarity_hint": self.polarity_hint,
        }

    @property
    def hash(self) -> str:
        return content_hash(self.to_dict())


@dataclass(frozen=True)
class PhaseFinding:
    source: str
    receiver: str
    kind: str
    band_from: str
    band_to: Optional[str]
    phase_from_deg: float
    phase_to_deg: Optional[float]
    wrapped_delta_deg: Optional[float]
    raw_delta_deg: Optional[float]
    confidence: float
    severity: str
    candidate_causes: Tuple[str, ...]
    group_delay_from_samples: Optional[float] = None
    group_delay_to_samples: Optional[float] = None

    def to_dict(self) -> Dict[str, Any]:
        return {
            "source": self.source,
            "receiver": self.receiver,
            "kind": self.kind,
            "band_from": self.band_from,
            "band_to": self.band_to,
            "phase_from_deg": self.phase_from_deg,
            "phase_to_deg": self.phase_to_deg,
            "wrapped_delta_deg": self.wrapped_delta_deg,
            "raw_delta_deg": self.raw_delta_deg,
            "confidence": self.confidence,
            "severity": self.severity,
            "candidate_causes": list(self.candidate_causes),
            "group_delay_from_samples": self.group_delay_from_samples,
            "group_delay_to_samples": self.group_delay_to_samples,
        }

    @property
    def pair_key(self) -> str:
        return f"{self.source}|{self.receiver}"

    @property
    def hash(self) -> str:
        return content_hash(self.to_dict())


@dataclass(frozen=True)
class CalibrationStep:
    order: int
    constraint: str
    band_id: str
    source: str
    receiver: str
    target_suppression_db: Optional[float]
    achieved_suppression_db: Optional[float]
    hit: bool
    detail: str

    def to_dict(self) -> Dict[str, Any]:
        return {
            "order": self.order,
            "constraint": self.constraint,
            "band_id": self.band_id,
            "source": self.source,
            "receiver": self.receiver,
            "target_suppression_db": self.target_suppression_db,
            "achieved_suppression_db": self.achieved_suppression_db,
            "hit": self.hit,
            "detail": self.detail,
        }


@dataclass(frozen=True)
class CalibrationSetting:
    band_id: str
    source: str
    receiver: str
    enabled: bool
    gain_db: float
    polarity: int
    target_suppression_db: float
    achieved_suppression_db: float
    phase_deg: float
    phase_cos_abs: float
    hit_target: bool
    main_gain_unchanged: bool
    within_gain_cap: bool
    feasible: bool
    reason: str

    def to_dict(self) -> Dict[str, Any]:
        return {
            "band_id": self.band_id,
            "source": self.source,
            "receiver": self.receiver,
            "enabled": self.enabled,
            "gain_db": self.gain_db,
            "polarity": self.polarity,
            "target_suppression_db": self.target_suppression_db,
            "achieved_suppression_db": self.achieved_suppression_db,
            "phase_deg": self.phase_deg,
            "phase_cos_abs": self.phase_cos_abs,
            "hit_target": self.hit_target,
            "main_gain_unchanged": self.main_gain_unchanged,
            "within_gain_cap": self.within_gain_cap,
            "feasible": self.feasible,
            "reason": self.reason,
        }


@dataclass(frozen=True)
class CalibrationResult:
    feasible: bool
    publishable: bool
    settings: Tuple[CalibrationSetting, ...]
    steps: Tuple[CalibrationStep, ...]
    conflicts: Tuple[Dict[str, Any], ...]
    constraint_order: Tuple[str, ...]
    error_code: Optional[str] = None

    def to_dict(self) -> Dict[str, Any]:
        return {
            "feasible": self.feasible,
            "publishable": self.publishable,
            "error_code": self.error_code,
            "constraint_order": list(self.constraint_order),
            "settings": [setting.to_dict() for setting in self.settings],
            "steps": [step.to_dict() for step in self.steps],
            "conflicts": list(self.conflicts),
        }

    @property
    def hash(self) -> str:
        return content_hash(self.to_dict())


@dataclass(frozen=True)
class RoutedBand:
    band: BandSpec
    matrix: Dict[str, Dict[str, Dict[str, Any]]]
    per_receiver_energy: Dict[str, float]

    def to_dict(self) -> Dict[str, Any]:
        return {
            "band": self.band.to_dict(),
            "matrix": self.matrix,
            "per_receiver_energy": self.per_receiver_energy,
        }

    @property
    def hash(self) -> str:
        return content_hash(self.to_dict())


@dataclass(frozen=True)
class BudgetBand:
    band_id: str
    entries: Tuple[BudgetEntry, ...]
    per_receiver_residual_energy: Dict[str, float]
    per_receiver_suppression_db: Dict[str, float]
    short_bands: Tuple[str, ...]

    def to_dict(self) -> Dict[str, Any]:
        return {
            "band_id": self.band_id,
            "entries": [entry.to_dict() for entry in self.entries],
            "per_receiver_residual_energy": self.per_receiver_residual_energy,
            "per_receiver_suppression_db": self.per_receiver_suppression_db,
            "short_bands": list(self.short_bands),
        }

    @property
    def hash(self) -> str:
        return content_hash(self.to_dict())


@dataclass(frozen=True)
class AnalysisState:
    schema_version: int
    config: Dict[str, Any]
    manifest_digest: str
    geometry_digest: str
    alignments: Tuple[Dict[str, Any], ...]
    coupling: Tuple[Dict[str, Any], ...]
    route_matrix: Tuple[Dict[str, Any], ...]
    budget: Tuple[Dict[str, Any], ...]
    phase_findings: Tuple[Dict[str, Any], ...]
    phase_summary: Dict[str, Any]
    calibration: Dict[str, Any]
    errors: Tuple[Dict[str, Any], ...]
    incremental: Dict[str, Any]
    coupling_diff: Dict[str, Any]
    node_hashes: Dict[str, str]
    trace_hash: str
    status: str
    content_hash_value: str = ""

    def to_dict(self) -> Dict[str, Any]:
        payload = {
            "schema_version": self.schema_version,
            "config": self.config,
            "manifest_digest": self.manifest_digest,
            "geometry_digest": self.geometry_digest,
            "alignments": list(self.alignments),
            "coupling": list(self.coupling),
            "route_matrix": list(self.route_matrix),
            "budget": list(self.budget),
            "phase_findings": list(self.phase_findings),
            "phase_summary": self.phase_summary,
            "calibration": self.calibration,
            "errors": list(self.errors),
            "incremental": self.incremental,
            "coupling_diff": self.coupling_diff,
            "node_hashes": self.node_hashes,
            "trace_hash": self.trace_hash,
            "status": self.status,
        }
        return payload

    def with_hash(self) -> "AnalysisState":
        return AnalysisState(
            schema_version=self.schema_version,
            config=self.config,
            manifest_digest=self.manifest_digest,
            geometry_digest=self.geometry_digest,
            alignments=self.alignments,
            coupling=self.coupling,
            route_matrix=self.route_matrix,
            budget=self.budget,
            phase_findings=self.phase_findings,
            phase_summary=self.phase_summary,
            calibration=self.calibration,
            errors=self.errors,
            incremental=self.incremental,
            coupling_diff=self.coupling_diff,
            node_hashes=self.node_hashes,
            trace_hash=self.trace_hash,
            status=self.status,
            content_hash_value=content_hash(self.to_dict()),
        )


def phase_difference_summary(
    previous: Sequence[Dict[str, Any]], current: Sequence[Dict[str, Any]]
) -> Dict[str, Any]:
    old = {
        (item["source"], item["receiver"], item["band_id"]): item for item in previous
    }
    rows = []
    max_abs_delta = -1.0
    max_band: Optional[str] = None
    max_pair: Optional[str] = None
    for item in current:
        key = (item["source"], item["receiver"], item["band_id"])
        before = old.get(key)
        if before is None:
            continue
        strength_delta = float(item["strength_db"]) - float(before["strength_db"])
        phase_delta = wrap_deg(float(item["phase_deg"]) - float(before["phase_deg"]))
        magnitude = max(abs(strength_delta), abs(phase_delta) / 10.0)
        if magnitude > max_abs_delta:
            max_abs_delta = magnitude
            max_band = item["band_id"]
            max_pair = f"{item['source']}|{item['receiver']}"
        rows.append(
            {
                "source": item["source"],
                "receiver": item["receiver"],
                "band_id": item["band_id"],
                "strength_delta_db": strength_delta,
                "phase_delta_deg": phase_delta,
            }
        )
    return {
        "compared": len(rows),
        "max_deviation_band": max_band,
        "max_deviation_pair": max_pair,
        "max_deviation_score": max(0.0, max_abs_delta),
        "rows": rows,
    }
