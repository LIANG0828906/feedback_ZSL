from __future__ import annotations

import copy

import pytest

from diffrender.compare import assert_reports_equal, compare_reports
from diffrender.errors import AppError
from diffrender.optimize import OptimizationRunner, RunConfig
from diffrender.paths import run_three_paths


def test_three_paths_are_elementwise_identical(dataset, tmp_path):
    task = dataset.task("single")
    config = RunConfig(
        iterations=4,
        learning_rate=0.006,
        mode="exact",
        checkpoint_every=2,
    )
    result = run_three_paths(task, config, tmp_path / "paths")
    assert result["ok"]
    assert all(value["equal"] for value in result["comparisons"].values())
    assert (
        result["realtime"]["final_parameters"]
        == result["resumed"]["final_parameters"]
        == result["oneshot"]["final_parameters"]
    )
    assert (
        result["realtime"]["loss_history"]
        == result["resumed"]["loss_history"]
        == result["oneshot"]["loss_history"]
    )
    assert (
        result["realtime"]["submission_sequence"]
        == result["resumed"]["submission_sequence"]
        == result["oneshot"]["submission_sequence"]
    )


def test_three_paths_are_identical_in_tiled_mode(dataset, tmp_path):
    task = dataset.task("single")
    config = RunConfig(
        iterations=2,
        learning_rate=0.006,
        mode="tiled",
        tile_size=16,
        max_parallel_blocks=2,
        checkpoint_every=1,
    )
    result = run_three_paths(task, config, tmp_path / "tiled_paths")
    assert result["ok"]


def test_three_paths_are_identical_in_downsample_mode(dataset, tmp_path):
    task = dataset.task("single")
    config = RunConfig(
        iterations=2,
        learning_rate=0.006,
        mode="downsample",
        requested_downsample_factor=2,
        checkpoint_every=1,
    )
    result = run_three_paths(task, config, tmp_path / "downsample_paths")
    assert result["ok"]
    assert result["realtime"]["strategy"]["downsample_factor"] == 2


def test_compare_localizes_first_difference(dataset):
    task = dataset.task("single")
    runner = OptimizationRunner(
        task,
        RunConfig(
            iterations=2,
            learning_rate=0.006,
            mode="exact",
            checkpoint_every=0,
        ),
    )
    runner.run_to(2)
    reference = runner.report().to_dict()
    candidate = copy.deepcopy(reference)
    candidate["final_parameters"]["sphere_0.radius"] += 1.0e-9
    candidate["loss_history"][0] += 1.0e-9
    report = compare_reports(reference, candidate)
    assert not report["equal"]
    assert report["first_difference"]["category"] == "parameter"
    assert (
        report["first_difference"]["parameter_identifier"]
        == "sphere_0.radius"
    )
    with pytest.raises(AppError) as caught:
        assert_reports_equal(reference, candidate)
    assert caught.value.code == "E_DIFF"
    assert caught.value.field_path == "final_parameters.sphere_0.radius"

