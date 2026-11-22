from __future__ import annotations

from dataclasses import replace

import pytest

from formation_workbench.errors import ErrorCode, WorkbenchError
from formation_workbench.executor import BoundedExecutor
from formation_workbench.models import SearchConfig
from formation_workbench.search import plan_member


def test_member_search_is_order_independent(sample_scene):
    order_one = ["alpha", "bravo", "charlie", "delta"]
    order_two = ["delta", "charlie", "bravo", "alpha"]
    first = {member: plan_member(sample_scene, member) for member in order_one}
    second = {member: plan_member(sample_scene, member) for member in order_two}
    for member in order_one:
        assert first[member].cells == second[member].cells
        assert first[member].cost == second[member].cost
        assert first[member].dependencies.cells == first[member].cells


def test_parallel_search_matches_serial(sample_scene):
    serial = {member: plan_member(sample_scene, member) for member in sample_scene.member_ids}
    executor = BoundedExecutor(3, 1)
    outcomes = executor.run_ordered(
        sample_scene.member_ids,
        lambda member: plan_member(sample_scene, member),
    )
    assert [outcome.item for outcome in outcomes] == list(sample_scene.member_ids)
    for outcome in outcomes:
        assert outcome.error is None
        assert outcome.value.cells == serial[outcome.item].cells
        assert outcome.value.cost == serial[outcome.item].cost


def test_heuristic_overestimate_is_rejected(sample_scene):
    weighted_scene = replace(
        sample_scene,
        search=SearchConfig(heuristic_weight=1500),
    )
    with pytest.raises(WorkbenchError) as captured:
        plan_member(weighted_scene, "alpha")
    error = captured.value
    assert error.code == ErrorCode.E_HEUR
    assert error.member_id == "alpha"
    assert error.coord is not None
    assert "overestimates" in error.message


def test_path_dependencies_include_terrain_and_blockades(sample_scene):
    path = plan_member(sample_scene, "alpha")
    assert path.dependencies.cells
    assert "plain" in path.dependencies.terrain_classes or "road" in path.dependencies.terrain_classes
    for coord in path.dependencies.cells:
        assert sample_scene.map.in_bounds(coord)
    assert path.dependencies.map_hash == sample_scene.map.content_hash()
