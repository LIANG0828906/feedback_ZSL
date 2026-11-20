from __future__ import annotations

import dataclasses

import numpy as np
import pytest

from probe_baker.bake import BakeEngine
from probe_baker.errors import ErrorCode, ProbeError
from probe_baker.scene import BoxOccluder, Sky
from probe_baker.strategy import STRATEGY_DENSE


def _advance_occluder(scene):
    updated = []
    for occluder in scene.occluders:
        if occluder.occluder_id == "service_counter" and isinstance(occluder, BoxOccluder):
            updated.append(
                BoxOccluder(
                    occluder.occluder_id,
                    (occluder.lower[0] + 0.4, occluder.lower[1], occluder.lower[2] - 0.3),
                    (occluder.upper[0] + 0.4, occluder.upper[1], occluder.upper[2] - 0.3),
                )
            )
        else:
            updated.append(occluder)
    return dataclasses.replace(scene, occluders=tuple(updated))


def test_local_occluder_change_reuses_unaffected_hashes(room_scene, small_config) -> None:
    engine = BakeEngine()
    baseline = engine.bake(room_scene, small_config)
    changed_scene = _advance_occluder(room_scene)
    changed = engine.bake(changed_scene, small_config, previous=baseline.state)
    assert changed.plan is not None
    assert any("occluders" in reason for reason in changed.plan.reasons)
    assert changed.plan.recomputed.size < changed.state.grid.count
    assert changed.plan.resampled.size < changed.state.grid.count
    assert changed.plan.dependency_closure.size >= changed.plan.recomputed.size
    unchanged = [
        index
        for index in range(changed.state.grid.count)
        if index not in set(changed.plan.recomputed.tolist())
    ]
    assert unchanged
    assert (
        changed.state.artifacts.probe_hashes[unchanged[0]]
        == baseline.state.artifacts.probe_hashes[unchanged[0]]
    )
    assert np.array_equal(
        changed.state.artifacts.coefficients[unchanged[0]],
        baseline.state.artifacts.coefficients[unchanged[0]],
    )


def test_compensation_change_is_partial_and_hash_stable(room_scene, small_config) -> None:
    engine = BakeEngine()
    baseline = engine.bake(room_scene, small_config)
    changed_config = dataclasses.replace(
        small_config, compensation_strength=small_config.compensation_strength + 0.2
    )
    changed = engine.bake(room_scene, changed_config, previous=baseline.state)
    assert changed.plan is not None
    assert "compensation_strength" in changed.plan.reasons
    assert changed.plan.recomputed.size < changed.state.grid.count
    reused = [
        index
        for index in range(changed.state.grid.count)
        if index not in set(changed.plan.recomputed.tolist())
    ]
    assert reused
    assert (
        changed.state.artifacts.probe_hashes[reused[0]]
        == baseline.state.artifacts.probe_hashes[reused[0]]
    )


def test_sky_change_is_global(room_scene, small_config) -> None:
    engine = BakeEngine()
    baseline = engine.bake(room_scene, small_config)
    sky = Sky(
        room_scene.sky.zenith,
        room_scene.sky.horizon,
        room_scene.sky.ground,
        room_scene.sky.intensity * 1.2,
    )
    changed_scene = dataclasses.replace(room_scene, sky=sky)
    changed = engine.bake(changed_scene, small_config, previous=baseline.state)
    assert changed.plan is not None
    assert "sky" in changed.plan.reasons
    assert changed.plan.recomputed.size == changed.state.grid.count


def test_dynamic_mask_change_is_local(room_scene, small_config) -> None:
    engine = BakeEngine()
    baseline = engine.bake(room_scene, small_config)
    masked = dataclasses.replace(
        room_scene, dynamic_mask=frozenset({"street_lamp_head"})
    )
    changed = engine.bake(masked, small_config, previous=baseline.state)
    assert changed.plan is not None
    assert any("street_lamp_head" in reason for reason in changed.plan.reasons)
    assert changed.plan.resampled.size > 0
    assert changed.plan.resampled.size < changed.state.grid.count
    assert changed.plan.recomputed.size < changed.state.grid.count


def test_sh_and_grid_change_rebuild_all(room_scene, small_config) -> None:
    engine = BakeEngine()
    baseline = engine.bake(room_scene, small_config)
    higher = dataclasses.replace(small_config, sh_order=3)
    changed = engine.bake(room_scene, higher, previous=baseline.state)
    assert changed.plan is not None
    assert "sh_order" in changed.plan.reasons
    assert changed.plan.recomputed.size == changed.state.grid.count
    assert changed.state.artifacts.coefficients.shape[1] == 16

    resized = dataclasses.replace(small_config, spacing=1.4)
    grid_change = engine.bake(room_scene, resized, previous=changed.state)
    assert grid_change.plan is not None
    assert "grid_structure" in grid_change.plan.reasons
    assert grid_change.grid_result.old_probe_count == changed.state.grid.count
    assert grid_change.grid_result.new_probe_count != changed.grid_result.old_probe_count
    assert grid_change.grid_result.structure_hash != changed.state.grid.structure_hash

    bounds_change = dataclasses.replace(
        room_scene, bounds_max=(room_scene.bounds_max[0] + 1.0, *room_scene.bounds_max[1:])
    )
    bounds_result = engine.bake(bounds_change, small_config, previous=grid_change.state)
    assert bounds_result.plan is not None
    assert "grid_structure" in bounds_result.plan.reasons
    assert bounds_result.grid_result.new_probe_count > grid_change.grid_result.new_probe_count


def test_nonfinite_direction_aborts_without_state(room_scene, small_config) -> None:
    directions = tuple(
        tuple(value) for value in np.asarray(
            [[0.0, 0.0, 1.0]] * 32, dtype=np.float64
        )
    )
    bad_directions = list(directions)
    bad_directions[0] = (float("nan"), 0.0, 1.0)
    config = dataclasses.replace(
        small_config, directions=tuple(bad_directions)
    )
    with pytest.raises(ProbeError) as exc:
        BakeEngine().bake(room_scene, config)
    assert exc.value.code == ErrorCode.SH_INVALID

