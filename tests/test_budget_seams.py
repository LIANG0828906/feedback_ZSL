from __future__ import annotations

import copy

import pytest

from volrender.config import parse_config
from volrender.errors import AppError, ErrorCode
from volrender.render import render_frame
from volrender.seams import check_seams


def test_budget_degrades_only_to_declared_stage(sample_bundle):
    loaded = sample_bundle["loaded"]
    artifact = render_frame(loaded.scene, loaded.config, "float64")
    degraded = artifact.summary["degraded_tile_indices"]
    assert degraded == [1, 4]
    assert artifact.summary["parallelism"]["max_workers"] == loaded.config.max_workers
    for result in artifact.tiles:
        assert result.samples_used == result.estimated_samples
        assert result.samples_used <= loaded.config.sample_budget
    attempts = [
        event for event in artifact.trace if event["event"] == "budget_attempt"
    ]
    tile_one = [
        event for event in attempts if event["tile_index"] == 1
    ]
    assert tile_one[0]["stage"] == "full"
    assert tile_one[0]["accepted"] is False
    assert tile_one[1]["stage"] == "balanced"
    assert tile_one[1]["accepted"] is True
    submits = [
        event["seq"] for event in artifact.trace if event["event"] == "submit"
    ]
    assert submits == sorted(submits)


def test_over_budget_rejects_before_any_tile_completes(sample_bundle):
    loaded = sample_bundle["loaded"]
    data = copy.deepcopy(sample_bundle["config_data"])
    data["sample_budget"] = 100
    config = parse_config(data)
    with pytest.raises(AppError) as caught:
        render_frame(loaded.scene, config, "float64")
    error = caught.value
    assert error.code == ErrorCode.E_BUDGET_EXCEEDED
    assert error.details["output_written"] is False
    assert len(error.details["rejected_tiles"]) == 6
    assert not any(
        event["event"] == "complete" for event in error.details["trace"]
    )
    assert any(
        event["event"] == "wait_queue"
        for event in error.details["trace"]
    )


def test_seam_reference_is_exact(sample_bundle):
    loaded = sample_bundle["loaded"]
    artifact = render_frame(loaded.scene, loaded.config, "float64")
    check = check_seams(artifact)
    assert check.ok
    assert check.max_color_delta == 0.0
    assert check.max_transmittance_delta == 0.0
    assert check.checked_pixels > 0


def test_seam_violation_reports_side_values(sample_bundle):
    loaded = sample_bundle["loaded"]
    artifact = render_frame(loaded.scene, loaded.config, "float64")
    artifact.color = artifact.color.copy()
    artifact.color[0, 15, 0] += 0.25
    with pytest.raises(AppError) as caught:
        check_seams(artifact)
    error = caught.value
    assert error.code == ErrorCode.E_SEAM_UNRESOLVED
    assert error.details["x"] == 15
    assert error.details["y"] == 0
    assert error.details["neighbor_side_values"]["left"] is not None
    assert error.details["neighbor_side_values"]["right"] is not None
    assert error.details["tiled_color"] != error.details["reference_color"]
