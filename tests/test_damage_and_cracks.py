from __future__ import annotations

import pytest

from cover_replay.engine import ReplayEngine, compute_quantization_impact, run_offline
from cover_replay.errors import E_CRACK_UNSTABLE, ReplayError

from .helpers import cover, load, make_scenario, shot


def test_fixed_point_primary_damage_and_quantization_bound(tmp_path):
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=[cover(hardness="1.0", durability="100", threshold="50")],
        shots=[shot(momentum="5.0049")],
        config={"physical_steps": 1, "fixed_point_decimals": 3},
    )
    scenario = load(scenario_dir)
    engine = ReplayEngine(
        scenario,
        tmp_path / "run",
        clean=True,
        compare_baseline=False,
    )
    frame = engine.step_once()
    assert frame["covers"][0]["accumulated_damage"] == "5.005"
    assert frame["covers"][0]["remaining_durability"] == "94.995"
    assert frame["covers"][0]["quantization_error_bound"] == "0.0005"
    assert frame["effective_shot_client_sequence"] == [0]
    assert frame["budget_usage"]["peak_memory_bytes"] <= 65536


def test_decimal_change_recomputes_and_reports_threshold_crossing(tmp_path):
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=[cover(durability="100", threshold="10.010")],
        shots=[
            shot(client_seq=0, arrival_seq=1, momentum="5.0049"),
            shot(client_seq=1, arrival_seq=2, momentum="5.0049"),
        ],
        config={"physical_steps": 1, "fixed_point_decimals": 3},
    )
    scenario = load(scenario_dir)
    reference = run_offline(scenario, decimals=3)
    selected = run_offline(scenario, decimals=2)
    assert reference[0]["covers"][0]["cracked"] is True
    assert selected[0]["covers"][0]["cracked"] is False

    impact = compute_quantization_impact(scenario, 3, 2)
    assert impact["affected_cover_count"] == 1
    item = impact["affected_covers"][0]
    assert item["cover_id"] == "C01"
    assert item["reason"] == "selected_precision_avoided_threshold"
    assert item["reference_threshold"] == "10.010"
    assert item["selected_threshold"] == "10.01"


def test_crack_propagates_chain_in_same_step(tmp_path):
    covers = [
        cover(
            "C01",
            durability="100",
            threshold="10",
            neighbors=[{"cover_id": "C02", "distance": "0"}],
        ),
        cover("C02", durability="100", threshold="10", neighbors=[]),
    ]
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=covers,
        shots=[shot(momentum="10.1")],
        config={"physical_steps": 1, "crack_propagation_ratio": "1.0"},
    )
    scenario = load(scenario_dir)
    engine = ReplayEngine(
        scenario,
        tmp_path / "run",
        clean=True,
        compare_baseline=False,
    )
    frame = engine.step_once()
    events = frame["crack_events"]
    assert [(event["cover_id"], event["path"]) for event in events] == [
        ("C01", ["C01"]),
        ("C02", ["C01", "C02"]),
    ]
    by_cover = {item["cover_id"]: item for item in frame["covers"]}
    assert by_cover["C02"]["cracked"] is True
    assert by_cover["C02"]["accumulated_damage"] == "10.000"


def test_unstable_chain_aborts_without_snapshot(tmp_path):
    covers = [
        cover(
            "C01",
            durability="100",
            threshold="1",
            neighbors=[{"cover_id": "C02", "distance": "0"}],
        ),
        cover(
            "C02",
            durability="100",
            threshold="1",
            neighbors=[{"cover_id": "C01", "distance": "0"}],
        ),
    ]
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=covers,
        shots=[shot(momentum="1")],
        config={
            "physical_steps": 1,
            "crack_max_iterations": 2,
            "crack_propagation_ratio": "1.0",
        },
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
    assert error.code == E_CRACK_UNSTABLE
    assert error.details["unstable_covers"]
    assert error.details["unstable_paths"]
    assert not list((output / "snapshots").glob("step_*.json"))
    assert engine.next_step == 0


def test_attenuation_uses_distance_and_radius(tmp_path):
    covers = [
        cover(
            "C01",
            durability="100",
            threshold="10",
            neighbors=[
                {"cover_id": "C02", "distance": "10"},
                {"cover_id": "C03", "distance": "20"},
            ],
        ),
        cover("C02", durability="100", threshold="50"),
        cover("C03", durability="100", threshold="50"),
    ]
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=covers,
        shots=[shot(momentum="10")],
        config={"physical_steps": 1, "crack_propagation_ratio": "1.0"},
    )
    scenario = load(scenario_dir)
    engine = ReplayEngine(
        scenario,
        tmp_path / "run",
        clean=True,
        compare_baseline=False,
    )
    frame = engine.step_once()
    by_cover = {item["cover_id"]: item for item in frame["covers"]}
    assert by_cover["C02"]["accumulated_damage"] == "5.000"
    assert by_cover["C03"]["accumulated_damage"] == "0.000"


def test_propagation_never_exceeds_neighbor_durability(tmp_path):
    covers = [
        cover(
            "C01",
            durability="100",
            threshold="1",
            neighbors=[{"cover_id": "C02", "distance": "0"}],
        ),
        cover("C02", durability="5", threshold="5"),
    ]
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=covers,
        shots=[shot(momentum="10")],
        config={"physical_steps": 1, "crack_propagation_ratio": "1.0"},
    )
    scenario = load(scenario_dir)
    engine = ReplayEngine(
        scenario,
        tmp_path / "run",
        clean=True,
        compare_baseline=False,
    )
    frame = engine.step_once()
    by_cover = {item["cover_id"]: item for item in frame["covers"]}
    assert by_cover["C02"]["accumulated_damage"] == "5.000"
    assert by_cover["C02"]["remaining_durability"] == "0.000"

