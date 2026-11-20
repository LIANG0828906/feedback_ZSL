from __future__ import annotations

import threading
from pathlib import Path

import numpy as np
import pytest

from volrender.budget import BudgetConfig
from volrender.coordinator import RenderCoordinator
from volrender.errors import BudgetExceededError, RenderBusyError, RenderCancelledError
from volrender.renderer import RayCaster
from volrender.transfer import TransferFunction
from volrender.view import CameraView
from volrender.volume import load_volume


@pytest.fixture()
def scene(volume_path: Path):
    volume = load_volume(volume_path)
    return RayCaster(volume, TransferFunction()), CameraView()


def test_repeat_render_is_byte_identical(scene) -> None:
    ray, view = scene
    budget = BudgetConfig(max_samples=10**12, max_memory_bytes=10**12, ray_batch=64)
    first = ray.render(view, width=64, height=64, step_size=1.0, budget=budget)
    second = ray.render(view, width=64, height=64, step_size=1.0, budget=budget)
    assert np.array_equal(first.pixels, second.pixels)
    assert np.array_equal(first.opacity, second.opacity)
    assert first.summary_dict() == second.summary_dict()
    assert first.content_hash == second.content_hash


def test_degradation_sequence_is_recorded(scene) -> None:
    ray, view = scene
    ample = BudgetConfig(max_samples=10**12, max_memory_bytes=10**12, ray_batch=64)
    base_estimate = ray.estimate_samples(
        view, width=64, height=64, step_size=1.0, early_term_alpha=0.98
    )
    increased = ray.estimate_samples(
        view, width=64, height=64, step_size=4.0, early_term_alpha=0.98
    )
    lowered = ray.estimate_samples(
        view, width=64, height=64, step_size=4.0, early_term_alpha=0.70
    )
    assert increased < base_estimate
    assert lowered < increased

    stage_one = ray.render(
        view,
        width=64,
        height=64,
        step_size=1.0,
        budget=BudgetConfig(
            max_samples=(base_estimate + increased) // 2,
            max_memory_bytes=10**12,
            ray_batch=64,
        ),
    )
    assert stage_one.degradation_stages == ["increase_step"]
    assert stage_one.step_size == 4.0

    checkpoints = []
    stage_two = ray.render(
        view,
        width=64,
        height=64,
        step_size=1.0,
        budget=BudgetConfig(
            max_samples=(increased + lowered) // 2,
            max_memory_bytes=10**12,
            ray_batch=64,
        ),
        checkpoint=lambda stage, details: checkpoints.append((stage, details)),
    )
    assert stage_two.degradation_stages == ["increase_step", "lower_early_term"]
    assert stage_two.early_term_alpha == 0.70
    assert [
        details["stage"]
        for stage, details in checkpoints
        if stage == "degradation"
    ] == ["increase_step", "lower_early_term"]

    reduced_memory = ray.estimate_memory(64, 64, 16) + 1
    stage_three = ray.render(
        view,
        width=64,
        height=64,
        step_size=1.0,
        budget=BudgetConfig(
            max_samples=(increased + lowered) // 2,
            max_memory_bytes=reduced_memory,
            ray_batch=4096,
            reduced_ray_batch=16,
        ),
    )
    assert stage_three.degradation_stages == [
        "increase_step",
        "lower_early_term",
        "reduce_batch",
    ]
    assert stage_three.ray_batch == 16
    assert stage_three.usage["memory_used_bytes"] <= reduced_memory
    assert np.array_equal(stage_three.pixels, stage_two.pixels)
    assert stage_three.content_hash == stage_two.content_hash


def test_budget_exhaustion_reports_all_stage_usage(scene) -> None:
    ray, view = scene
    with pytest.raises(BudgetExceededError) as raised:
        ray.render(
            view,
            width=32,
            height=32,
            step_size=1.0,
            budget=BudgetConfig(max_samples=1, max_memory_bytes=10**12),
        )
    error = raised.value
    assert error.code == "E_BUDGET_EXCEEDED"
    assert error.detail == "DEGRADATION_EXHAUSTED"
    assert [item["stage"] for item in error.usage["stage_usage"]] == [
        "increase_step",
        "lower_early_term",
    ]
    assert error.usage["final_estimate"]["samples"] > 1


def test_same_volume_view_is_busy_while_render_in_flight(scene) -> None:
    ray, view = scene
    coordinator = RenderCoordinator()
    entered = threading.Event()
    release = threading.Event()
    errors: list[BaseException] = []
    results = []

    def checkpoint(stage, details) -> None:
        if stage == "ray_batch" and not entered.is_set():
            entered.set()
            release.wait(timeout=5)

    def worker() -> None:
        try:
            results.append(
                coordinator.render(
                    ray,
                    view,
                    width=64,
                    height=64,
                    step_size=1.0,
                    budget=BudgetConfig(max_samples=10**12, max_memory_bytes=10**12, ray_batch=8),
                    checkpoint=checkpoint,
                )
            )
        except BaseException as exc:  # test thread transport
            errors.append(exc)

    thread = threading.Thread(target=worker, daemon=True)
    thread.start()
    assert entered.wait(timeout=5)
    with pytest.raises(RenderBusyError) as raised:
        coordinator.render(
            ray,
            view,
            width=16,
            height=16,
            step_size=1.0,
            budget=BudgetConfig(),
        )
    assert raised.value.code == "E_BUSY"
    release.set()
    thread.join(timeout=10)
    assert not thread.is_alive()
    assert not errors
    assert len(results) == 1
    assert coordinator.active_keys == []


def test_cancel_at_batch_checkpoint(scene) -> None:
    ray, view = scene
    coordinator = RenderCoordinator()
    entered = threading.Event()
    release = threading.Event()
    errors: list[BaseException] = []

    def checkpoint(stage, details) -> None:
        if stage == "ray_batch" and not entered.is_set():
            entered.set()
            release.wait(timeout=5)

    def worker() -> None:
        try:
            coordinator.render(
                ray,
                view,
                width=64,
                height=64,
                step_size=1.0,
                budget=BudgetConfig(max_samples=10**12, max_memory_bytes=10**12, ray_batch=8),
                checkpoint=checkpoint,
            )
        except BaseException as exc:
            errors.append(exc)

    thread = threading.Thread(target=worker, daemon=True)
    thread.start()
    assert entered.wait(timeout=5)
    assert coordinator.cancel(ray.volume.volume_sha256, view.hash)
    release.set()
    thread.join(timeout=10)
    assert not thread.is_alive()
    assert len(errors) == 1
    assert isinstance(errors[0], RenderCancelledError)
    assert errors[0].code == "E_CANCELLED"
    assert coordinator.active_keys == []


def test_cancel_at_degradation_checkpoint(scene) -> None:
    ray, view = scene
    cancel_event = threading.Event()
    cancel_event.set()
    with pytest.raises(RenderCancelledError) as raised:
        ray.render(
            view,
            width=32,
            height=32,
            step_size=1.0,
            budget=BudgetConfig(max_samples=1, max_memory_bytes=10**12),
            cancel_event=cancel_event,
        )
    assert raised.value.code == "E_CANCELLED"
    assert raised.value.usage["stage"] == "increase_step"

