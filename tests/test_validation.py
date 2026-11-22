from __future__ import annotations

import copy
import math

import pytest

from formation_workbench.errors import ErrorCode, WorkbenchError
from formation_workbench.validation import validate_scene


def _raises(payload):
    with pytest.raises(WorkbenchError) as captured:
        validate_scene(payload)
    return captured.value


def test_schema_missing_required_field(sample_payload):
    payload = copy.deepcopy(sample_payload)
    payload["map"].pop("costs")
    error = _raises(payload)
    assert error.code == ErrorCode.E_SCHEMA
    assert error.field == "map.costs"
    assert any(issue.field == "map.costs" for issue in error.issues)


def test_range_bad_cost_and_coordinate(sample_payload):
    payload = copy.deepcopy(sample_payload)
    payload["map"]["costs"][3][4] = 1.5
    error = _raises(payload)
    assert error.code == ErrorCode.E_RANGE
    assert error.coord == (4, 3)
    assert "costs" in (error.field or "")


def test_range_goal_outside_map(sample_payload):
    payload = copy.deepcopy(sample_payload)
    payload["members"][0]["goal"] = {"x": 999, "y": 3}
    error = _raises(payload)
    assert error.code == ErrorCode.E_RANGE
    assert error.field == "members[0].goal"
    assert error.coord == (999, 3)


def test_duplicate_member_identifier(sample_payload):
    payload = copy.deepcopy(sample_payload)
    payload["members"][1]["member_id"] = payload["members"][0]["member_id"]
    error = _raises(payload)
    assert error.code == ErrorCode.E_SCHEMA
    assert "duplicate member id" in error.message


def test_non_finite_slot_offset(sample_payload):
    payload = copy.deepcopy(sample_payload)
    payload["formation"]["slots"]["alpha"]["x"] = math.nan
    error = _raises(payload)
    assert error.code == ErrorCode.E_RANGE
    assert error.field == "formation.slots.alpha.x"


def test_missing_slots_prefers_schema_error(sample_payload):
    payload = copy.deepcopy(sample_payload)
    payload["formation"].pop("slots")
    error = _raises(payload)
    assert error.code == ErrorCode.E_SCHEMA
    assert error.field == "formation.slots"


def test_blockade_references_missing_terrain(sample_payload):
    payload = copy.deepcopy(sample_payload)
    payload["map"]["blockades"][0]["terrain"] = "does-not-exist"
    error = _raises(payload)
    assert error.code == ErrorCode.E_REF
    assert error.field == "map.blockades[0].terrain"


def test_blockade_missing_terrain_field_is_schema_error(sample_payload):
    payload = copy.deepcopy(sample_payload)
    payload["map"]["blockades"][0].pop("terrain")
    error = _raises(payload)
    assert error.code == ErrorCode.E_SCHEMA
    assert error.field == "map.blockades[0].terrain"


def test_slot_missing_axis_is_schema_error(sample_payload):
    payload = copy.deepcopy(sample_payload)
    payload["formation"]["slots"]["alpha"].pop("x")
    error = _raises(payload)
    assert error.code == ErrorCode.E_SCHEMA
    assert error.field == "formation.slots.alpha"


def test_member_references_missing_slot(sample_payload):
    payload = copy.deepcopy(sample_payload)
    payload["members"][0]["slot_id"] = "missing-slot"
    error = _raises(payload)
    assert error.code == ErrorCode.E_REF
    assert error.field == "members[0].slot_id"


def test_impassable_cell_has_terrain_marker(sample_payload):
    payload = copy.deepcopy(sample_payload)
    payload["map"]["terrain"][3][4] = "plain"
    payload["map"]["costs"][3][4] = -1
    error = _raises(payload)
    assert error.code == ErrorCode.E_SCHEMA
    assert error.coord == (4, 3)
