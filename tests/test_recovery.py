from __future__ import annotations

import copy
import multiprocessing
import os

import numpy as np
import pytest

from filter_studio.errors import WorkbenchError
from filter_studio.filters import STAGE_FUNCTIONS
from filter_studio.pipeline import PipelineSession, ResourceLimits, RunControl
from filter_studio.stages import DEFAULT_PARAMS


def _hard_exit_after_first_stage(
    snapshot_path: str,
    image_path: str,
    params: dict,
) -> None:
    from filter_studio.pipeline import PipelineSession

    session = PipelineSession(snapshot_path, params=params)
    session.set_image(image_path)

    def listener(event):
        if event.get("type") == "stage_done" and event.get("stage") == "denoise":
            os._exit(37)

    session.run(listener=listener)


def test_cancel_persists_completed_prefix_and_resumes_consistently(
    sample_bundle, sample_by_mode, tmp_path
):
    sample_dir, _ = sample_bundle
    sample = sample_by_mode["RGB"]
    snapshot = tmp_path / "cancel.snapshot"
    session = PipelineSession(snapshot)
    session.set_image(sample_dir / sample["file"])
    control = RunControl()

    def listener(event):
        if event.get("type") == "stage_done" and event.get("stage") == "denoise":
            control.cancel()

    cancelled = session.run(control=control, listener=listener)
    assert cancelled.status == "cancelled"
    assert cancelled.error["code"] == "E_CANCELLED"
    assert list(session.cache.entries) == ["denoise"]
    assert snapshot.is_file()
    manifest = session.snapshot.peek_manifest()
    assert manifest["chain_complete"] is False
    assert manifest["completed_stages"] == ["denoise"]

    resumed = PipelineSession(snapshot)
    restore = resumed.set_image(sample_dir / sample["file"])
    assert restore.status == "partial"
    assert restore.loaded_stages == ("denoise",)
    assert resumed.last_run is None
    run = resumed.run()
    assert run.status == "complete"
    assert run.diagnostics.recompute_count == 2
    assert run.diagnostics.cache_hit_count == 1
    assert any("未完成快照" in note for note in run.diagnostics.notes)

    reference = PipelineSession(tmp_path / "reference.snapshot")
    reference.set_image(sample_dir / sample["file"])
    reference.run()
    assert np.array_equal(run.output, reference.cache.get("local_contrast").output)
    assert resumed.compare_with_full().equal


def test_hard_process_exit_recovers_only_persisted_stage_prefix(
    sample_bundle, sample_by_mode, tmp_path
):
    sample_dir, _ = sample_bundle
    sample = sample_by_mode["LA"]
    snapshot = tmp_path / "hard-exit.snapshot"
    context = multiprocessing.get_context("spawn")
    process = context.Process(
        target=_hard_exit_after_first_stage,
        args=(
            str(snapshot),
            str(sample_dir / sample["file"]),
            copy.deepcopy(DEFAULT_PARAMS),
        ),
    )
    process.start()
    process.join(timeout=30)
    assert process.exitcode == 37

    manifest = PipelineSession(snapshot).snapshot.peek_manifest()
    assert manifest is not None
    assert manifest["chain_complete"] is False
    assert manifest["completed_stages"] == ["denoise"]

    recovered = PipelineSession(snapshot)
    restore = recovered.set_image(sample_dir / sample["file"])
    assert restore.status == "partial"
    assert restore.snapshot_complete is False
    assert len(recovered.cache) == 1
    assert recovered.last_run is None
    resumed = recovered.run()
    assert resumed.status == "complete"
    assert resumed.diagnostics.recompute_count == 2
    assert resumed.diagnostics.cache_hit_count == 1
    assert any("不对强制终止前的完整报告作任何声明" in n for n in resumed.diagnostics.notes)
    assert recovered.compare_with_full().equal


def test_stage_failure_does_not_pollute_upstream_and_can_retry(
    sample_bundle, sample_by_mode, tmp_path
):
    sample_dir, _ = sample_bundle
    sample = sample_by_mode["L"]
    state = {"fail": True}

    def flaky_runner(stage_id, array, params, checkpoint):
        if stage_id == "sharpen" and state["fail"]:
            state["fail"] = False
            raise RuntimeError("injected sharpen failure")
        return STAGE_FUNCTIONS[stage_id](array, params, checkpoint)

    session = PipelineSession(
        tmp_path / "failure.snapshot",
        stage_runner=flaky_runner,
    )
    session.set_image(sample_dir / sample["file"])
    failed = session.run()
    assert failed.status == "failed"
    assert failed.error["code"] == "E_STAGE"
    assert failed.error["stage"] == "sharpen"
    assert list(session.cache.entries) == ["denoise"]
    denoise_key = session.cache.get("denoise").cache_key
    assert session.stage_statuses["sharpen"] == "failed"
    assert session.stage_statuses["local_contrast"] == "blocked"
    assert session.snapshot.peek_manifest()["completed_stages"] == ["denoise"]

    retried = session.retry()
    assert retried.status == "complete"
    assert retried.diagnostics.recompute_count == 2
    assert retried.diagnostics.cache_hit_count == 1
    assert session.cache.get("denoise").cache_key == denoise_key
    assert session.compare_with_full().equal


def test_corrupt_input_returns_e_decode(tmp_path):
    bad = tmp_path / "bad.png"
    bad.write_bytes(b"\x89PNG\r\n\x1a\nbroken")
    session = PipelineSession(tmp_path / "decode.snapshot")
    with pytest.raises(WorkbenchError) as caught:
        session.set_image(bad)
    assert caught.value.code == "E_DECODE"


def test_memory_limit_is_enforced(sample_bundle, sample_by_mode, tmp_path):
    sample_dir, _ = sample_bundle
    session = PipelineSession(
        tmp_path / "memory.snapshot",
        limits=ResourceLimits(max_buffer_bytes=4096),
    )
    with pytest.raises(WorkbenchError) as caught:
        session.set_image(sample_dir / sample_by_mode["L"]["file"])
    assert caught.value.code == "E_MEMORY"

