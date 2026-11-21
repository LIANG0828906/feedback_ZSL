from __future__ import annotations

import pytest

from cover_replay.engine import ReplayEngine
from cover_replay.errors import E_BUDGET_EXCEEDED, ReplayError

from .helpers import cover, load, make_scenario, shot


def test_memory_degradation_and_wait_queue(tmp_path):
    covers = [
        cover("C01", buffer_bytes=12000),
        cover("C02", buffer_bytes=6000),
        cover("C03", buffer_bytes=6000),
    ]
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=covers,
        shots=[shot()],
        config={
            "physical_steps": 1,
            "parallelism": 2,
            "memory_budget_bytes": 10000,
            "degradation_sequence": ["1.0", "0.5", "0.25"],
        },
    )
    scenario = load(scenario_dir)
    engine = ReplayEngine(
        scenario,
        tmp_path / "run",
        clean=True,
        compare_baseline=False,
    )
    frame = engine.step_once()
    events = frame["budget_usage"]["events"]
    degradation = [event for event in events if event["event"] == "degrade"]
    queue_events = [event for event in events if event["event"] == "queue"]
    assert degradation[0]["cover_id"] == "C01"
    assert degradation[0]["reserved_memory_bytes"] == 6000
    assert queue_events
    assert frame["budget_usage"]["peak_memory_bytes"] <= 10000
    assert frame["step_peak_memory_bytes"] <= 10000


def test_irreducible_buffer_rejects_step_without_publication(tmp_path):
    covers = [cover("C01", buffer_bytes=1000)]
    scenario_dir = make_scenario(
        tmp_path / "scenario",
        covers=covers,
        shots=[shot()],
        config={
            "physical_steps": 1,
            "memory_budget_bytes": 100,
            "degradation_sequence": ["1.0", "0.5"],
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
    assert caught.value.code == E_BUDGET_EXCEEDED
    assert caught.value.details["degradation_attempts"]
    assert not list((output / "snapshots").glob("step_*.json"))
    assert engine.next_step == 0
    assert (output / "failure.json").is_file()

