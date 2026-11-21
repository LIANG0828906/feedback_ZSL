from __future__ import annotations

import json

import pytest

from cover_replay.errors import E_REPLAY_INVALID, ReplayError
from cover_replay.loader import load_scenario

from .helpers import cover, make_scenario, shot, write_json


def test_missing_shot_field_reports_record_and_path(tmp_path):
    scenario_dir = make_scenario(tmp_path / "scenario", covers=[cover()], shots=[shot()])
    match_path = scenario_dir / "match.json"
    data = json.loads(match_path.read_text(encoding="utf-8"))
    del data["shots"][0]["momentum"]
    write_json(match_path, data)

    with pytest.raises(ReplayError) as caught:
        load_scenario(scenario_dir)

    error = caught.value
    assert error.code == E_REPLAY_INVALID
    assert error.record_index == 0
    assert error.field_path == "shots[0].momentum"


@pytest.mark.parametrize(
    ("mutation", "expected_path"),
    [
        (lambda item: item.update(incidence_angle="-1"), "shots[0].incidence_angle"),
        (lambda item: item.update(momentum="-0.1"), "shots[0].momentum"),
        (lambda item: item.update(effective_step=99), "shots[0].effective_step"),
        (
            lambda item: item.update(hit_position={"x": "99", "y": "5"}),
            "shots[0].hit_position",
        ),
    ],
)
def test_shot_out_of_range_has_precise_location(tmp_path, mutation, expected_path):
    scenario_dir = make_scenario(tmp_path / "scenario", covers=[cover()], shots=[shot()])
    match_path = scenario_dir / "match.json"
    data = json.loads(match_path.read_text(encoding="utf-8"))
    mutation(data["shots"][0])
    write_json(match_path, data)

    with pytest.raises(ReplayError) as caught:
        load_scenario(scenario_dir)
    assert caught.value.code == E_REPLAY_INVALID
    assert caught.value.record_index == 0
    assert caught.value.field_path == expected_path


def test_cover_reference_and_threshold_are_validated(tmp_path):
    bad_neighbor = cover(neighbors=[{"cover_id": "C99", "distance": "1"}])
    scenario_dir = make_scenario(tmp_path / "scenario", covers=[bad_neighbor], shots=[shot()])
    with pytest.raises(ReplayError) as caught:
        load_scenario(scenario_dir)
    assert caught.value.field_path == "covers[0].neighbors[0].cover_id"

    scenario_dir = make_scenario(
        tmp_path / "scenario-2",
        covers=[cover(threshold="101")],
        shots=[shot()],
    )
    with pytest.raises(ReplayError) as caught:
        load_scenario(scenario_dir)
    assert caught.value.field_path == "covers[0].segment_damage_threshold"


def test_duplicate_client_sequence_is_rejected(tmp_path):
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=[cover()],
        shots=[shot(arrival_seq=1), shot(arrival_seq=2)],
    )
    with pytest.raises(ReplayError) as caught:
        load_scenario(scenario_dir)
    assert caught.value.record_index == 1
    assert caught.value.field_path == "shots[1].client_seq"


def test_unsupported_match_schema_version_has_field_path(tmp_path):
    scenario_dir = make_scenario(tmp_path / "scenario", covers=[cover()], shots=[shot()])
    match_path = scenario_dir / "match.json"
    data = json.loads(match_path.read_text(encoding="utf-8"))
    data["schema_version"] = 99
    write_json(match_path, data)
    with pytest.raises(ReplayError) as caught:
        load_scenario(scenario_dir)
    assert caught.value.code == E_REPLAY_INVALID
    assert caught.value.field_path == "match.schema_version"

