from __future__ import annotations

from dataclasses import replace

import pytest

from formation_workbench.errors import ErrorCode, WorkbenchError
from formation_workbench.search import plan_member
from formation_workbench.smoothing import (
    smooth_all,
    smooth_member,
    traversed_cells,
    waypoint_count_with_tolerance,
)


def test_smoothing_keeps_passability_and_dual_readings(sample_scene):
    raw = {member: plan_member(sample_scene, member) for member in sample_scene.member_ids}
    smoothed = smooth_all(sample_scene, raw)
    for member_id, path in smoothed.items():
        assert path.cost >= path.baseline_cost
        for waypoint in path.waypoints:
            assert waypoint.grid[0] * 1000 + 500 == waypoint.world[0]
            assert waypoint.grid[1] * 1000 + 500 == waypoint.world[1]
        for cell in traversed_cells(path):
            assert sample_scene.map.is_passable(cell)


def test_tighter_slot_tolerance_never_reduces_waypoints(sample_scene):
    raw = {member: plan_member(sample_scene, member) for member in sample_scene.member_ids}
    looser = waypoint_count_with_tolerance(sample_scene, raw, 2200)
    tighter = waypoint_count_with_tolerance(sample_scene, raw, 800)
    strictest = waypoint_count_with_tolerance(sample_scene, raw, 100)
    assert tighter >= looser
    assert strictest >= tighter


def test_smoothing_parameter_bounds(sample_scene):
    raw = plan_member(sample_scene, "alpha")
    parent = smooth_member(sample_scene, raw, None)
    with pytest.raises(WorkbenchError) as captured:
        smooth_member(
            sample_scene,
            raw,
            parent if parent.member_id != raw.member_id else None,
            slot_tolerance=sample_scene.formation.hard_slot_tolerance + 1,
        )
    assert captured.value.code == ErrorCode.E_PARA


def test_formation_violation_returns_pair_and_deviation(sample_scene):
    raw = {member: plan_member(sample_scene, member) for member in sample_scene.member_ids}
    alpha = smooth_member(sample_scene, raw["alpha"], None)
    bad_slots = dict(sample_scene.formation.slots)
    bad_slots["bravo"] = (20000, 2000)
    bad_formation = replace(sample_scene.formation, slots=bad_slots)
    bad_scene = replace(sample_scene, formation=bad_formation)
    with pytest.raises(WorkbenchError) as captured:
        smooth_member(bad_scene, raw["bravo"], alpha)
    error = captured.value
    assert error.code == ErrorCode.E_FORM
    assert error.pair == ("bravo", "alpha")
    assert error.deviation is not None and error.deviation > 0
