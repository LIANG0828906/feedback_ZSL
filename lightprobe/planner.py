from __future__ import annotations

from collections import defaultdict, deque

import numpy as np

from .bake_types import BakeConfig, InfluenceReport
from .directions import DirectionSet
from .geometry import ray_distance_for_occluder
from .grid import ProbeGrid
from .influence import record_reusable
from .record import ProbeRecord
from .scene import Scene
from .state import BakeState
def _full_report(
    probe_ids: tuple[str, ...],
    changed_factors: list[str],
    reason: str,
) -> InfluenceReport:
    return InfluenceReport(
        changed_factors=tuple(changed_factors),
        seed_probe_ids=probe_ids,
        affected_probe_ids=probe_ids,
        reasons={probe_id: reason for probe_id in probe_ids[:512]},
        conservative=True,
        conservative_reason=reason,
    )


def _closure(
    seeds: set[str],
    records: dict[str, ProbeRecord],
) -> set[str]:
    reverse: dict[str, set[str]] = defaultdict(set)
    for probe_id, record in records.items():
        dependencies = set(record.source_ids)
        if record.leak_score > 1.0e-12:
            dependencies |= set(record.comp_source_ids)
        for dependency in dependencies:
            if dependency != probe_id:
                reverse[dependency].add(probe_id)
    affected = set(seeds)
    queue = deque(seeds)
    while queue:
        current = queue.popleft()
        for dependent in reverse.get(current, ()):
            if dependent not in affected:
                affected.add(dependent)
                queue.append(dependent)
    return affected


def plan_incremental(
    old: BakeState | None,
    *,
    scene: Scene,
    grid: ProbeGrid,
    directions: DirectionSet,
    audit_directions: DirectionSet,
    config: BakeConfig,
    strategy: str,
) -> InfluenceReport:
    all_ids = grid.probe_ids
    if old is None:
        return _full_report(all_ids, ["initial_bake"], "no_prior_committed_state")

    changed: list[str] = []
    if scene.scene_id != old.scene.scene_id:
        return _full_report(all_ids, ["scene_id"], "scene_identity_changed")
    if grid.signature != old.grid.signature:
        return _full_report(
            all_ids,
            ["probe_spacing", "bounds"],
            "probe_grid_rebuilt_spacing_or_bounds_changed",
        )
    if tuple(sorted(old.records)) != tuple(sorted(all_ids)):
        return _full_report(all_ids, ["probe_grid"], "probe_id_set_changed")
    if old.config.sh_order != config.sh_order:
        return _full_report(all_ids, ["sh_order"], "sh_order_changed")
    if (
        old.config.direction_count != config.direction_count
        or old.config.direction_seed != config.direction_seed
    ):
        return _full_report(all_ids, ["direction_set"], "direction_set_changed")
    if old.config.audit_direction_count != config.audit_direction_count:
        return _full_report(all_ids, ["audit_direction_set"], "audit_direction_set_changed")
    if old.scene.sky.content_hash() != scene.sky.content_hash():
        return _full_report(all_ids, ["sky_light"], "sky_light_parameters_changed")
    if old.strategy.name != strategy:
        return _full_report(all_ids, ["sampling_strategy"], "sampling_strategy_changed")
    if old.config.interpolation_neighbors != config.interpolation_neighbors:
        return _full_report(
            all_ids,
            ["interpolation_neighbors"],
            "interpolation_neighborhood_changed",
        )

    old_object_ids = old.scene.occluder_ids
    new_object_ids = scene.occluder_ids
    if old_object_ids != new_object_ids:
        return _full_report(
            all_ids,
            ["occluder_set"],
            "occluder_id_set_or_order_changed_conservative_rebuild",
        )

    old_geometry = {
        object_id: old.scene.geometry_hash(object_id) for object_id in old_object_ids
    }
    new_geometry = {object_id: scene.geometry_hash(object_id) for object_id in new_object_ids}
    geometry_changed = {
        object_id
        for object_id in old_object_ids
        if old_geometry[object_id] != new_geometry[object_id]
    }
    old_materials = {
        name: material.content_hash() for name, material in old.scene.materials.items()
    }
    new_materials = {name: material.content_hash() for name, material in scene.materials.items()}
    changed_materials = {
        name
        for name in set(old_materials) | set(new_materials)
        if old_materials.get(name) != new_materials.get(name)
    }
    material_affected_objects = {
        object_id
        for object_id in old_object_ids
        if scene.occluder(object_id) is not None
        and scene.occluder(object_id).material in changed_materials
    }
    dirty_objects = geometry_changed | material_affected_objects
    if geometry_changed:
        changed.append("occluder_geometry")
    if changed_materials:
        changed.append("material_albedo")
    if old.config.compensation_strength != config.compensation_strength:
        changed.append("compensation_strength")

    old_records = dict(old.records)
    old_indices = {object_id: index for index, object_id in enumerate(old_object_ids)}
    new_indices = {object_id: index for index, object_id in enumerate(new_object_ids)}
    seed_reasons: dict[str, str] = {}

    if dirty_objects:
        distance_matrix = np.stack(
            [np.asarray(old_records[probe_id].first_hit_distances) for probe_id in all_ids]
        )
        hit_matrix = np.stack(
            [np.asarray(old_records[probe_id].first_hit_ids) for probe_id in all_ids]
        )
        geometry_objects = sorted(geometry_changed)
        for object_id in sorted(dirty_objects):
            old_index = old_indices[object_id]
            hit_this = np.any(hit_matrix == old_index, axis=1)
            if object_id in geometry_objects:
                new_occluder = scene.occluder(object_id)
                if new_occluder is None:
                    # The id set is stable, but keep this branch defensive.
                    object_dirty = hit_this
                else:
                    distance = ray_distance_for_occluder(
                        new_occluder, grid.coordinates, audit_directions.vectors
                    )
                    new_closer = distance < distance_matrix - 1.0e-7
                    object_dirty = hit_this | np.any(new_closer, axis=1)
            else:
                object_dirty = hit_this
            for probe_id in np.asarray(all_ids, dtype=object)[object_dirty]:
                seed_reasons.setdefault(str(probe_id), f"occluder_or_material:{object_id}")

    if old.config.compensation_strength != config.compensation_strength:
        for probe_id in all_ids:
            record = old_records[probe_id]
            if record.leak_score > 1.0e-12:
                seed_reasons.setdefault(probe_id, "compensation_strength:leak_source")

    for probe_id, record in old_records.items():
        if not record.verify_content_hash():
            seed_reasons.setdefault(probe_id, "content_hash_mismatch")

    affected = _closure(set(seed_reasons), old_records)
    reasons = dict(seed_reasons)

    # The double check is intentionally repeated after closure: the content hash
    # and factor version must both prove reuse, never just an ID match.
    changed_any = True
    while changed_any:
        changed_any = False
        for probe_id in all_ids:
            if probe_id in affected:
                continue
            record = old_records[probe_id]
            reusable, reason = record_reusable(
                record,
                scene=scene,
                grid=grid,
                directions=directions,
                audit_directions=audit_directions,
                config=config,
                strategy=strategy,
                records=old_records,
            )
            if not reusable:
                affected.add(probe_id)
                reasons[probe_id] = reason
                expanded = _closure({probe_id}, old_records)
                if expanded - affected:
                    affected |= expanded
                changed_any = True

    conservative = False
    conservative_reason = None
    if not changed and not affected:
        reasons = {}
    return InfluenceReport(
        changed_factors=tuple(changed),
        seed_probe_ids=tuple(sorted(seed_reasons)),
        affected_probe_ids=tuple(probe_id for probe_id in all_ids if probe_id in affected),
        reasons={key: reasons[key] for key in sorted(reasons)},
        conservative=conservative,
        conservative_reason=conservative_reason,
    )


def factor_change_summary(
    old: BakeState | None, new: BakeState
) -> dict[str, str]:
    if old is None:
        return {"state": "initial"}
    result: dict[str, str] = {}
    for key in sorted(set(old.factor_versions) | set(new.factor_versions)):
        if old.factor_versions.get(key) != new.factor_versions.get(key):
            result[key] = "changed"
    return result or {"state": "unchanged"}

