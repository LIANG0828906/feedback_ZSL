from __future__ import annotations

import threading

import numpy as np
import pytest

from dustrender.chunks import build_chunk_table
from dustrender.config import RenderConfig
from dustrender.errors import E_BUDGET_EXCEEDED, E_BUSY, DustRenderError
from dustrender.scene import Scene
from dustrender.scheduler import RenderScheduler
from dustrender.seam import compare_seams


def _seam_config() -> RenderConfig:
    return RenderConfig.from_dict(
        {
            "chunk_width": 12,
            "chunk_height": 9,
            "parallelism": 3,
            "task_budget": 100000,
            "budget_policy": "degrade",
            "base_step": 0.12,
            "min_step": 0.04,
            "max_step": 0.8,
            "density_adapt_gain": 1.25,
            "phase": 0.5,
            "shadow_samples": 2,
            "shadow_distance": 8.0,
            "precision": "float",
            "compare_precision": True,
            "seam_validation": True,
            "seam_tolerance": 2e-5,
        }
    )


def test_chunk_table_is_row_major_and_uses_submission_ids() -> None:
    chunks = build_chunk_table(25, 19, 12, 9)
    assert [chunk.id for chunk in chunks] == [
        "c0000",
        "c0001",
        "c0002",
        "c0003",
        "c0004",
        "c0005",
        "c0006",
        "c0007",
        "c0008",
    ]
    assert chunks[-1].bounds == (24, 18, 25, 19)


def test_render_is_deterministic_and_seam_free(smoke_path) -> None:
    scene = Scene.load(smoke_path)
    config = _seam_config()
    first = RenderScheduler().render(scene, config)
    second = RenderScheduler().render(scene, config)
    assert first.error is None
    assert first.seam["status"] == "passed"
    assert first.seam["violation_count"] == 0
    assert first.image.tobytes() == second.image.tobytes()
    assert first.summary()["summary_sha256"] == second.summary()["summary_sha256"]
    assert first.precision_compare is not None
    assert first.precision_compare["pixels_over_upper_bound"] == 0
    assert first.precision_compare["max_abs"] <= first.precision_compare["upper_bound"]


def test_budget_degrade_keeps_every_task_within_budget(nested_path) -> None:
    scene = Scene.load(nested_path)
    config = RenderConfig.from_dict(
        {
            "chunk_width": 16,
            "chunk_height": 16,
            "parallelism": 2,
            "task_budget": 12000,
            "budget_policy": "degrade",
            "base_step": 0.08,
            "min_step": 0.02,
            "max_step": 2.0,
            "shadow_samples": 3,
            "seam_validation": False,
            "compare_precision": False,
        }
    )
    run = RenderScheduler().render(scene, config)
    assert run.error is None
    assert run.degraded
    assert run.effective_step > config.base_step
    assert all(result.samples <= config.task_budget for result in run.results.values())


def test_budget_reject_preserves_completed_chunks(triple_path) -> None:
    scene = Scene.load(triple_path)
    config = RenderConfig.from_dict(
        {
            "chunk_width": 8,
            "chunk_height": 8,
            "parallelism": 4,
            "task_budget": 3000,
            "budget_policy": "reject",
            "base_step": 0.06,
            "max_step": 1.0,
            "shadow_samples": 2,
            "seam_validation": False,
            "compare_precision": False,
        }
    )
    run = RenderScheduler().render(scene, config)
    assert run.error is not None
    assert run.error.code == E_BUDGET_EXCEEDED
    assert run.results
    assert run.rejected
    assert "completed_chunks" in run.error.details


def test_duplicate_whole_render_returns_busy(smoke_path) -> None:
    scene = Scene.load(smoke_path)
    config = _seam_config()
    scheduler = RenderScheduler()
    entered = threading.Event()
    release = threading.Event()

    def callback(results, statuses, step) -> None:
        if not entered.is_set():
            entered.set()
            release.wait(5)

    thread = threading.Thread(
        target=lambda: scheduler.render(scene, config, on_progress=callback),
        daemon=True,
    )
    thread.start()
    assert entered.wait(5)
    try:
        with pytest.raises(DustRenderError) as caught:
            scheduler.render(scene, config)
        assert caught.value.code == E_BUSY
    finally:
        release.set()
        thread.join(10)
    assert not thread.is_alive()


def test_chunked_pixels_match_single_chunk_truth(smoke_path) -> None:
    scene = Scene.load(smoke_path)
    run = RenderScheduler().render(scene, _seam_config())
    truth_chunk = run.chunks[0].__class__(0, "truth", 0, 0, 0, 0, scene.width, scene.height)
    from dustrender.renderer import render_chunk

    truth = render_chunk(scene, run.config, truth_chunk, run.effective_step)
    assert np.array_equal(run.image, truth.rgb)
    assert np.array_equal(run.transmittance, truth.transmittance)


def test_seam_error_localizes_pixel_and_chunk_pair(smoke_path) -> None:
    scene = Scene.load(smoke_path)
    config = _seam_config()
    run = RenderScheduler().render(scene, config)
    truth_chunk = run.chunks[0].__class__(0, "truth", 0, 0, 0, 0, scene.width, scene.height)
    from dustrender.renderer import render_chunk

    truth = render_chunk(scene, config, truth_chunk, run.effective_step)
    broken = run.results["c0000"]
    broken.rgb[0, 0, 0] += 1.0
    doc, error = compare_seams(scene, run.chunks, run.results, truth, config.seam_tolerance)
    assert doc["status"] == "failed"
    assert error is not None
    assert error.code == "E_SEAM_UNRESOLVED"
    assert error.details["violations"][0]["chunk"] == "c0000"
