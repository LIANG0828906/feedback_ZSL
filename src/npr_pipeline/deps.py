from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .strokes import LAYER_NAMES


def batch_dependencies(
    layer: int,
    zone_id: str,
    factors: dict[str, str],
) -> dict[str, str]:
    result = {
        "geometry": factors[f"geometry.{zone_id}"],
        "grid": factors["grid"],
    }
    if layer == 0:
        result["outline"] = factors["outline"]
    elif layer == 1:
        result["shadow"] = factors["shadow"]
        result["outline_geometry"] = factors["outline_geometry"]
    elif layer == 2:
        result["smear"] = factors["smear"]
        result["shadow_density"] = factors["shadow_density"]
        result["outline_geometry"] = factors["outline_geometry"]
    else:
        raise ValueError(f"unknown layer: {layer}")
    return result


@dataclass
class InvalidationPlan:
    changed_factors: tuple[str, ...]
    layers_to_generate: dict[int, set[str]]
    affected_old_batches: set[str] = field(default_factory=set)
    reasons: dict[str, list[str]] = field(default_factory=dict)

    def add_reason(self, key: str, reason: str) -> None:
        values = self.reasons.setdefault(key, [])
        if reason not in values:
            values.append(reason)

    def reason_for(self, layer: int, zone_id: str) -> list[str]:
        values = list(self.reasons.get(f"layer:{layer}", []))
        values.extend(self.reasons.get(f"zone:{zone_id}", []))
        return sorted(set(values))

    def will_generate(self, layer: int, zone_id: str) -> bool:
        return zone_id in self.layers_to_generate.get(layer, set())


def compute_invalidation(
    old_manifest: dict[str, Any] | None,
    factors: dict[str, str],
    zones: tuple[str, ...],
) -> InvalidationPlan:
    all_zone_set = set(zones)
    if old_manifest is None:
        layers = {layer: set(all_zone_set) for layer in LAYER_NAMES}
        plan = InvalidationPlan(
            changed_factors=tuple(sorted(factors)),
            layers_to_generate=layers,
        )
        for layer in LAYER_NAMES:
            plan.add_reason(f"layer:{layer}", "no_committed_version")
        return plan

    old_factors = {
        str(key): str(value)
        for key, value in (old_manifest.get("factors") or {}).items()
    }
    changed: set[str] = set()
    for key in sorted(set(old_factors) | set(factors)):
        if old_factors.get(key) != factors.get(key):
            changed.add(key)
    plan = InvalidationPlan(
        changed_factors=tuple(sorted(changed)),
        layers_to_generate={layer: set() for layer in LAYER_NAMES},
    )

    def mark_layer(layer: int, zones_to_mark: set[str], reason: str) -> None:
        plan.layers_to_generate[layer].update(zones_to_mark)
        plan.add_reason(f"layer:{layer}", reason)

    if "outline" in changed:
        mark_layer(0, all_zone_set, "outline_parameters_changed")
    if "outline_geometry" in changed:
        mark_layer(0, all_zone_set, "outline_geometry_changed")
        mark_layer(1, all_zone_set, "outline_culling_closure")
        mark_layer(2, all_zone_set, "shadow_density_closure")
    if "shadow" in changed:
        mark_layer(1, all_zone_set, "shadow_parameters_changed")
    if "shadow_density" in changed:
        mark_layer(2, all_zone_set, "shadow_density_changed")
    if "smear" in changed:
        mark_layer(2, all_zone_set, "smear_parameters_changed")
    if "grid" in changed:
        for layer in LAYER_NAMES:
            mark_layer(layer, all_zone_set, "sampling_grid_changed")

    for key in sorted(changed):
        if not key.startswith("geometry."):
            continue
        zone_id = key.split(".", 1)[1]
        if zone_id in all_zone_set:
            for layer in LAYER_NAMES:
                mark_layer(layer, {zone_id}, "partition_geometry_changed")
                plan.add_reason(f"zone:{zone_id}", "partition_geometry_changed")
        else:
            for layer in LAYER_NAMES:
                mark_layer(layer, all_zone_set, "zone_set_changed")

    old_batches = old_manifest.get("batches") or []
    for record in old_batches:
        if not isinstance(record, dict):
            continue
        batch_id = str(record.get("batch", ""))
        layer = int(record.get("layer", -1))
        zone_id = str(record.get("zone_id", ""))
        if layer not in LAYER_NAMES or zone_id not in all_zone_set:
            plan.affected_old_batches.add(batch_id)
            continue
        expected = batch_dependencies(layer, zone_id, factors)
        stored = record.get("factor_versions") or {}
        if stored != expected:
            plan.affected_old_batches.add(batch_id)
            plan.layers_to_generate[layer].add(zone_id)
            plan.add_reason(
                f"layer:{layer}",
                "dependency_version_changed_conservative",
            )

    return plan


__all__ = [
    "InvalidationPlan",
    "batch_dependencies",
    "compute_invalidation",
]
