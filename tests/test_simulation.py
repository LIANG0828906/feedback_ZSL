from __future__ import annotations

from dataclasses import replace

import pytest

from formation_workbench.errors import ErrorCode, WorkbenchError
from formation_workbench.fixed import point_segment_distance
from formation_workbench.search import plan_member
from formation_workbench.simulation import simulate_member, validate_member_trace
from formation_workbench.smoothing import Waypoint, smooth_all, smooth_member


def _prepared(sample_scene):
    raw = {member: plan_member(sample_scene, member) for member in sample_scene.member_ids}
    return smooth_all(sample_scene, raw)


def test_tick_replay_limits_turn_and_stays_on_polyline(sample_scene):
    paths = _prepared(sample_scene)
    trace = simulate_member(sample_scene, "alpha", paths)
    assert trace.completed_tick < sample_scene.simulation.max_ticks
    previous_segment = 0
    previous_heading = trace.ticks[0].heading
    for record in trace.ticks[1:]:
        assert record.segment_index in {previous_segment, previous_segment + 1}
        previous_segment = record.segment_index
        delta = abs((record.heading - previous_heading + 180000) % 360000 - 180000)
        assert delta <= sample_scene.member("alpha").turn_rate
        previous_heading = record.heading
        path = paths["alpha"]
        if record.segment_index < len(path.points) - 1:
            distance = point_segment_distance(
                record.position,
                path.points[record.segment_index],
                path.points[record.segment_index + 1],
            )
            assert distance <= 2
    assert trace.ticks[trace.completed_tick].completed
    assert trace.ticks[-1].position == paths["alpha"].points[-1]


def test_invalid_step_returns_para(sample_scene):
    paths = _prepared(sample_scene)
    member = sample_scene.member("alpha")
    bad_member = replace(member, step=0)
    members = tuple(
        bad_member if item.member_id == member.member_id else item
        for item in sample_scene.members
    )
    bad_scene = replace(sample_scene, members=members)
    with pytest.raises(WorkbenchError) as captured:
        simulate_member(bad_scene, "alpha", paths)
    assert captured.value.code == ErrorCode.E_PARA
    assert captured.value.member_id == "alpha"


def test_formation_collapse_returns_adv(sample_scene):
    paths = _prepared(sample_scene)
    bravo = paths["bravo"]
    shifted_points = tuple((point[0] + 5000, point[1]) for point in bravo.points)
    shifted_waypoints = tuple(
        Waypoint(
            index=item.index,
            grid=(item.grid[0] + 5, item.grid[1]),
            world=(item.world[0] + 5000, item.world[1]),
        )
        for item in bravo.waypoints
    )
    bad_path = replace(bravo, points=shifted_points, waypoints=shifted_waypoints)
    shifted_paths = dict(paths)
    shifted_paths["bravo"] = bad_path
    tight_scene = replace(
        sample_scene,
        formation=replace(sample_scene.formation, hard_slot_tolerance=4200),
    )
    with pytest.raises(WorkbenchError) as captured:
        simulate_member(tight_scene, "bravo", shifted_paths)
    error = captured.value
    assert error.code == ErrorCode.E_ADV
    assert error.member_id == "bravo"
    assert error.tick == 0
    assert error.pair == ("bravo", "alpha")
    assert error.deviation is not None and error.deviation > 0


def test_soft_formation_excursion_slows_and_records_reason(sample_scene):
    paths = _prepared(sample_scene)
    bravo = paths["bravo"]
    shifted_points = tuple((point[0] - 4000, point[1]) for point in bravo.points)
    shifted_waypoints = tuple(
        Waypoint(
            index=item.index,
            grid=(item.grid[0] - 4, item.grid[1]),
            world=(item.world[0] - 4000, item.world[1]),
        )
        for item in bravo.waypoints
    )
    shifted_paths = dict(paths)
    shifted_paths["bravo"] = replace(
        bravo, points=shifted_points, waypoints=shifted_waypoints
    )
    with pytest.raises(WorkbenchError) as captured:
        simulate_member(
            replace(
                sample_scene,
                formation=replace(
                    sample_scene.formation, hard_slot_tolerance=4200
                ),
            ),
            "bravo",
            shifted_paths,
        )
    error = captured.value
    assert error.code == ErrorCode.E_ADV
    partial_ticks = error.details.get("partial_ticks", [])
    assert any(
        record["slowdown_reason"] is not None
        and "formation_slowdown" in record["slowdown_reason"]
        for record in partial_ticks
    ) or (
        error.details.get("attempt_slowdown_reason") is not None
        and "formation_slowdown"
        in str(error.details.get("attempt_slowdown_reason"))
    )


def test_stationary_member_path_is_supported(sample_scene):
    alpha = sample_scene.member("alpha")
    stationary = replace(alpha, goal=alpha.start)
    members = tuple(
        stationary if item.member_id == alpha.member_id else item
        for item in sample_scene.members
    )
    stationary_scene = replace(sample_scene, members=members)
    raw = plan_member(stationary_scene, "alpha")
    assert raw.cells == (alpha.start,)
    path = smooth_member(stationary_scene, raw, None)
    assert len(path.points) == 1
    trace = simulate_member(stationary_scene, "alpha", {"alpha": path})
    assert trace.completed_tick == 0
    assert trace.ticks[0].completed


def test_trace_validator_detects_segment_jump(sample_scene):
    paths = _prepared(sample_scene)
    trace = simulate_member(sample_scene, "alpha", paths)
    bad_tick = replace(trace.ticks[2], segment_index=2)
    bad_trace = replace(
        trace,
        ticks=trace.ticks[:2] + (bad_tick,) + trace.ticks[3:],
    )
    with pytest.raises(WorkbenchError) as captured:
        validate_member_trace(sample_scene, "alpha", paths["alpha"], bad_trace)
    assert captured.value.code == ErrorCode.E_ADV
    assert captured.value.item == "segment_index"
    assert captured.value.tick == 2
