from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Dict, List, Optional, Sequence, Tuple

from .config import changed_band_ids, neighboring_bands
from .models import ScenarioConfig


NODE_ORDER = (
    "alignment",
    "coupling",
    "route_matrix",
    "budget",
    "phase",
    "calibration",
)


@dataclass(frozen=True)
class RecomputePlan:
    initial: bool
    changed_inputs: Tuple[str, ...]
    affected: Dict[str, Tuple[str, ...]]
    forced_bands: Tuple[str, ...]
    forced_reasons: Dict[str, str]
    cache_hits: Dict[str, Tuple[str, ...]]
    recomputed: Dict[str, Tuple[str, ...]]

    def to_dict(self) -> Dict[str, Any]:
        nodes = {}
        for node in NODE_ORDER:
            nodes[node] = {
                "affected_bands": list(self.affected.get(node, ())),
                "cache_hits": list(self.cache_hits.get(node, ())),
                "recomputed_bands": list(self.recomputed.get(node, ())),
            }
        return {
            "initial": self.initial,
            "changed_inputs": list(self.changed_inputs),
            "forced_bands": list(self.forced_bands),
            "forced_reasons": self.forced_reasons,
            "nodes": nodes,
        }


def _bands_contiguous(config: ScenarioConfig) -> bool:
    for left, right in zip(config.bands, config.bands[1:]):
        if abs(left.high_hz - right.low_hz) > 1e-9:
            return False
    return True


def _config_value(config: Dict[str, Any], path: Sequence[str], default: Any) -> Any:
    value: Any = config
    for key in path:
        if not isinstance(value, dict) or key not in value:
            return default
        value = value[key]
    return value


def build_plan(
    previous_state: Optional[Any],
    previous_config: Optional[Dict[str, Any]],
    config: ScenarioConfig,
    alignment_changed: bool,
) -> RecomputePlan:
    all_bands = tuple(config.band_ids)
    if previous_state is None or previous_config is None:
        affected = {node: all_bands for node in NODE_ORDER}
        return RecomputePlan(
            initial=True,
            changed_inputs=("initial",),
            affected=affected,
            forced_bands=all_bands,
            forced_reasons={band: "initial computation" for band in all_bands},
            cache_hits={node: () for node in NODE_ORDER},
            recomputed=affected,
        )

    changed_inputs: List[str] = []
    if alignment_changed:
        changed_inputs.append("alignment")
    old_target = float(previous_config.get("target_suppression_db"))
    new_target = config.target_suppression_db
    if old_target != new_target:
        changed_inputs.append("target_suppression_db")
    old_max_gain = float(previous_config.get("max_cancellation_gain_db"))
    if old_max_gain != config.max_cancellation_gain_db:
        changed_inputs.append("max_cancellation_gain_db")
    old_layout = str(previous_config.get("measurement_layout"))
    if old_layout != config.measurement_layout:
        changed_inputs.append("geometric_alignment_basis")
    old_estimator = str(previous_config.get("estimator_version", "band-ls-v1"))
    if old_estimator != config.estimator_version:
        changed_inputs.append("estimator_version")
    old_phase = previous_config.get("phase", {})
    new_phase = config.to_dict()["phase"]
    if old_phase != new_phase:
        changed_inputs.append("phase_policy")
    old_calibration = previous_config.get("calibration", {})
    new_calibration = config.to_dict()["calibration"]
    if old_calibration != new_calibration:
        changed_inputs.append("calibration_policy")

    old_bands = previous_config.get("bands", [])
    changed_ids, structural_ids = changed_band_ids(old_bands, config.bands)
    structural = bool(structural_ids) or not _bands_contiguous(config)
    if changed_ids:
        changed_inputs.append("band_boundaries")
    if structural:
        changed_inputs.append("band_set_structure")

    forced: set[str] = set()
    forced_reasons: Dict[str, str] = {}
    if structural:
        forced.update(all_bands)
        for band in all_bands:
            forced_reasons[band] = "band set or contiguous coverage changed; cache safety unproven"
    if "estimator_version" in changed_inputs:
        forced.update(all_bands)
        for band in all_bands:
            forced_reasons[band] = "estimator version changed; cache safety unproven"
    if alignment_changed:
        forced.update(all_bands)
        for band in all_bands:
            forced_reasons.setdefault(band, "alignment basis or component content changed")

    affected_coupling = set(all_bands if (structural or alignment_changed or "estimator_version" in changed_inputs) else changed_ids)
    affected_route = set(affected_coupling)
    budget_global = bool(
        set(changed_inputs)
        & {
            "target_suppression_db",
            "max_cancellation_gain_db",
            "alignment",
            "band_boundaries",
            "band_set_structure",
        }
    )
    affected_budget = set(all_bands if budget_global else affected_route)
    phase_global = bool(
        set(changed_inputs)
        & {
            "alignment",
            "band_boundaries",
            "band_set_structure",
            "phase_policy",
            "estimator_version",
        }
    )
    affected_phase = set(all_bands if phase_global else ())
    if affected_phase:
        affected_phase.update(neighboring_bands(all_bands, affected_phase))
    calibration_global = bool(
        set(changed_inputs)
        & {
            "target_suppression_db",
            "max_cancellation_gain_db",
            "alignment",
            "band_boundaries",
            "band_set_structure",
            "phase_policy",
            "calibration_policy",
            "estimator_version",
        }
    )
    affected_calibration = set(
        all_bands if calibration_global else (affected_budget | affected_phase)
    )

    affected: Dict[str, Tuple[str, ...]] = {
        "alignment": all_bands if alignment_changed else (),
        "coupling": tuple(band for band in all_bands if band in affected_coupling),
        "route_matrix": tuple(band for band in all_bands if band in affected_route),
        "budget": tuple(band for band in all_bands if band in affected_budget),
        "phase": tuple(band for band in all_bands if band in affected_phase),
        "calibration": tuple(
            band for band in all_bands if band in affected_calibration
        ),
    }
    cache_hits = {
        node: tuple(
            band
            for band in all_bands
            if band not in affected[node] and band not in forced
        )
        for node in NODE_ORDER
    }
    recomputed = {
        node: tuple(
            band
            for band in all_bands
            if band in affected[node] or band in forced
        )
        for node in NODE_ORDER
    }
    if not changed_inputs:
        changed_inputs.append("none")
    return RecomputePlan(
        initial=False,
        changed_inputs=tuple(changed_inputs),
        affected=affected,
        forced_bands=tuple(band for band in all_bands if band in forced),
        forced_reasons=forced_reasons,
        cache_hits=cache_hits,
        recomputed=recomputed,
    )
