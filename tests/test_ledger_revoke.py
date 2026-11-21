from __future__ import annotations

import pytest

from cover_replay.engine import ReplayEngine
from cover_replay.errors import E_CREDIT_CONFLICT, E_REVOKE_CLOSED, ReplayError

from .helpers import cover, load, make_scenario, revoke, shot


def test_overdraw_returns_credit_conflict_with_both_durabilities(tmp_path):
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=[cover(durability="10", threshold="10")],
        shots=[
            shot(client_seq=0, arrival_seq=1, momentum="6"),
            shot(client_seq=1, arrival_seq=2, momentum="6"),
        ],
        config={"physical_steps": 1},
    )
    scenario = load(scenario_dir)
    output = tmp_path / "run"
    engine = ReplayEngine(
        scenario,
        output,
        clean=True,
        compare_baseline=False,
    )
    with pytest.raises(ReplayError) as caught:
        engine.step_once()
    error = caught.value
    assert error.code == E_CREDIT_CONFLICT
    assert error.details["cover_id"] == "C01"
    assert error.details["shot_key"] == "P1:1"
    assert error.details["predicted_durability"] == "-2.000"
    assert error.details["authoritative_durability"] == "0.000"
    assert not list((output / "snapshots").glob("step_*.json"))


def test_damage_on_destroyed_cover_is_returned(tmp_path):
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=[cover(durability="5", threshold="5")],
        shots=[
            shot(client_seq=0, arrival_seq=1, momentum="5", step=0),
            shot(client_seq=1, arrival_seq=2, momentum="5", step=1),
        ],
        config={"physical_steps": 2},
    )
    scenario = load(scenario_dir)
    engine = ReplayEngine(
        scenario,
        tmp_path / "run",
        clean=True,
        compare_baseline=False,
    )
    first = engine.step_once()
    second = engine.step_once()
    assert first["covers"][0]["remaining_durability"] == "0.000"
    assert second["effective_shot_client_sequence"] == [1]
    assert engine.state.ledger["P1:1"].status == "returned"
    assert second["covers"][0]["accumulated_damage"] == "5.000"


def test_same_step_revoke_cancels_pending_credit(tmp_path):
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=[cover()],
        shots=[shot(momentum="5", step=0)],
        revokes=[revoke(client_seq=0, arrival_seq=2, step=0)],
        config={"physical_steps": 1},
    )
    scenario = load(scenario_dir)
    engine = ReplayEngine(
        scenario,
        tmp_path / "run",
        clean=True,
        compare_baseline=False,
    )
    frame = engine.step_once()
    assert frame["effective_shot_client_sequence"] == []
    assert frame["covers"][0]["accumulated_damage"] == "0.000"
    assert frame["revocation_events"][0]["action"] == "cancelled_pending"
    assert engine.state.ledger["P1:0"].status == "revoked"


def test_settled_revoke_generates_compensation_within_window(tmp_path):
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=[cover()],
        shots=[shot(momentum="5", step=0)],
        revokes=[revoke(client_seq=0, arrival_seq=10, step=1)],
        config={"physical_steps": 2, "revocation_window_steps": 2},
    )
    scenario = load(scenario_dir)
    engine = ReplayEngine(
        scenario,
        tmp_path / "run",
        clean=True,
        compare_baseline=False,
    )
    engine.step_once()
    frame = engine.step_once()
    record = frame["compensations"][0]
    assert record["restoration_units"] == 5000
    assert record["authoritative_unchanged_units"] == 5000
    assert engine.state.ledger["P1:0"].status == "compensated"
    assert frame["covers"][0]["accumulated_damage"] == "5.000"
    assert frame["covers"][0]["predicted_remaining"] == "100.000"


def test_revoke_outside_window_returns_closed_error(tmp_path):
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=[cover()],
        shots=[shot(momentum="5", step=0)],
        revokes=[revoke(client_seq=0, arrival_seq=10, step=2)],
        config={"physical_steps": 3, "revocation_window_steps": 0},
    )
    scenario = load(scenario_dir)
    engine = ReplayEngine(
        scenario,
        tmp_path / "run",
        clean=True,
        compare_baseline=False,
    )
    engine.step_once()
    engine.step_once()
    with pytest.raises(ReplayError) as caught:
        engine.step_once()
    assert caught.value.code == E_REVOKE_CLOSED
    assert caught.value.details["settled_step"] == 0
    assert engine.next_step == 2

