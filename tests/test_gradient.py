from __future__ import annotations

import pytest
import torch

from diffrender.compare import assert_reports_equal
from diffrender.errors import AppError
from diffrender.gradient import GradientGuard, GuardConfig
from diffrender.optimize import OptimizationRunner, RunConfig
from diffrender.params import build_layout, raw_from_scene


def test_gradient_guard_freezes_only_failed_scalar(dataset):
    task = dataset.task("single")
    layout = build_layout(task.scene)
    raw = raw_from_scene(task.scene, layout).requires_grad_(True)
    raw.grad = torch.ones_like(raw)
    guard = GradientGuard(
        layout,
        GuardConfig(
            clip_enabled=True,
            clip_norm=0.5,
            explode_threshold=100.0,
            failure_patience=1,
        ),
    )
    report = guard.process(
        raw,
        iteration=0,
        gradient_spikes={"sphere_0.radius": 1.0e8},
    )
    radius_index = layout.label_to_index["sphere_0.radius"]
    albedo_index = layout.label_to_index["sphere_0.albedo.r"]
    assert "sphere_0.radius" in report.newly_failed
    assert float(raw.grad[radius_index].item()) == 0.0
    assert abs(float(raw.grad[albedo_index].item())) > 0.0
    assert report.clipping_triggered
    with pytest.raises(AppError) as caught:
        guard.raise_if_failed(report)
    assert caught.value.code == "E_GRAD"


def test_clip_enabled_and_disabled_trajectories_are_reproducible(dataset):
    task = dataset.task("single")
    clipped_config = RunConfig(
        iterations=3,
        learning_rate=0.006,
        mode="exact",
        checkpoint_every=0,
        clip_enabled=True,
        clip_norm=5.0,
    )
    unclipped_config = RunConfig(
        iterations=3,
        learning_rate=0.006,
        mode="exact",
        checkpoint_every=0,
        clip_enabled=False,
        clip_norm=5.0,
    )
    clipped_a = OptimizationRunner(task, clipped_config)
    clipped_a.run_to(3)
    clipped_b = OptimizationRunner(task, clipped_config)
    clipped_b.run_to(3)
    assert_reports_equal(
        clipped_a.report().to_dict(),
        clipped_b.report().to_dict(),
        compare_gradient_history=True,
        label="clipped trajectory",
    )
    unclipped_a = OptimizationRunner(task, unclipped_config)
    unclipped_a.run_to(3)
    unclipped_b = OptimizationRunner(task, unclipped_config)
    unclipped_b.run_to(3)
    assert_reports_equal(
        unclipped_a.report().to_dict(),
        unclipped_b.report().to_dict(),
        compare_gradient_history=True,
        label="unclipped trajectory",
    )
    assert clipped_a.config_fingerprint != unclipped_a.config_fingerprint


def test_injected_failure_freezes_parameter_and_checkpoint_recovers(dataset, tmp_path):
    task = dataset.task("single")
    config = RunConfig(
        iterations=4,
        learning_rate=0.006,
        mode="exact",
        checkpoint_every=1,
        failure_patience=1,
        explode_threshold=100.0,
    )
    checkpoint_dir = tmp_path / "checkpoints"
    initial = OptimizationRunner(task, config, checkpoint_dir=checkpoint_dir)
    initial.run_to(2)
    before = initial.report().final_parameters

    failing = OptimizationRunner(
        task,
        config,
        checkpoint_dir=checkpoint_dir,
        gradient_spikes={2: {"sphere_0.radius": 1.0e18}},
    )
    assert failing.load_checkpoint()["valid"]
    with pytest.raises(AppError) as caught:
        failing.run_to(3)
    assert caught.value.code == "E_GRAD"
    after = failing.parameter_errors()
    assert after["sphere_0.radius"]["estimate"] == before["sphere_0.radius"]
    assert after["sphere_0.center.x"]["estimate"] != before["sphere_0.center.x"]

    recovered = OptimizationRunner(task, config, checkpoint_dir=checkpoint_dir)
    assert recovered.load_checkpoint()["valid"]
    recovered.run_to(4)
    reference = OptimizationRunner(task, config)
    reference.run_to(4)
    assert_reports_equal(
        reference.report().to_dict(),
        recovered.report().to_dict(),
        compare_gradient_history=True,
        label="recovered run",
    )

