from __future__ import annotations

from dataclasses import replace
from pathlib import Path

from streamforge import Pipeline
from streamforge.errors import ErrorCode
from streamforge.pipeline import load_artifacts_from_output
from streamforge.playback import (
    BandwidthSample,
    PlaybackConfig,
    load_bandwidth,
    simulate_playback,
)
from tests.conftest import read_json


def _prepare(config_factory, tmp_path: Path):
    config, budget = config_factory()
    output = tmp_path / "out"
    run = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run()
    slices = load_artifacts_from_output(output, run.project)
    playback = PlaybackConfig.from_dict(
        read_json(run.project.config_path.parent / "playback.json")
    )
    bandwidth = load_bandwidth(run.project.config_path.parent / "bandwidth.json")
    return run, slices, playback, bandwidth


def test_switches_only_at_shared_slice_boundaries(config_factory, tmp_path: Path) -> None:
    run, slices, playback, bandwidth = _prepare(config_factory, tmp_path)
    result = simulate_playback(
        tiers=run.project.tiers,
        slices=slices,
        bandwidth=bandwidth,
        playback=playback,
        budget=run.project.budget,
    )
    assert not result.errors
    switches = [event for event in result.events if event["event"] == "switch"]
    assert switches
    for event in switches:
        index = event["index"]
        target = next(
            item
            for item in slices[event["to_tier"]]
            if item.index == index
        )
        assert target.start_ns == run.timeline.boundaries[index].start_ns
        assert target.audio_start_ns == target.start_ns
        assert target.video_start_ns == target.start_ns
    assert result.min_buffer_bytes >= 0
    assert result.max_buffer_bytes <= run.project.budget.decode_buffer_bytes


def test_misaligned_switch_is_rejected_and_old_tier_retained(
    config_factory, tmp_path: Path
) -> None:
    run, slices, _, _ = _prepare(config_factory, tmp_path)
    slices["high"][0] = replace(
        slices["high"][0],
        video_start_ns=slices["high"][0].video_start_ns + 1,
    )
    slices["high"] = slices["high"][:1]
    result = simulate_playback(
        tiers=run.project.tiers,
        slices=slices,
        bandwidth=[BandwidthSample(time_ms=0, kbps=5000)],
        playback=PlaybackConfig(
            initial_tier="low",
            initial_buffer_bytes=700000,
            bandwidth_safety_ratio=0.8,
        ),
        budget=run.project.budget,
    )
    rejected = [
        event for event in result.events if event["event"] == "switch_rejected"
    ]
    assert rejected
    assert rejected[0]["error"]["code"] == ErrorCode.SWITCH_MISALIGN
    assert rejected[0]["to_tier"] == "high"
    assert result.final_tier != "high"


def test_decoder_buffer_overflow_rejects_switch(
    config_factory, tmp_path: Path
) -> None:
    run, slices, _, _ = _prepare(config_factory, tmp_path)
    result = simulate_playback(
        tiers=run.project.tiers,
        slices=slices,
        bandwidth=[
            BandwidthSample(time_ms=0, kbps=5000)
        ],
        playback=PlaybackConfig(
            initial_tier="low",
            initial_buffer_bytes=run.project.budget.decode_buffer_bytes,
            bandwidth_safety_ratio=0.8,
        ),
        budget=run.project.budget,
    )
    rejected = [
        event for event in result.events if event["event"] == "switch_rejected"
    ]
    assert rejected
    assert rejected[0]["error"]["code"] == ErrorCode.SWITCH_MISALIGN
    assert "overflow" in rejected[0]["error"]["message"]
