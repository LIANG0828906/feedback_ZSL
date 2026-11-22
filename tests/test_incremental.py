from __future__ import annotations

import copy

import numpy as np
import pytest

from filter_studio.errors import WorkbenchError
from filter_studio.filters import STAGE_FUNCTIONS
from filter_studio.cache import make_cache_key
from filter_studio.pipeline import PipelineSession
from filter_studio.stages import DEFAULT_PARAMS, STAGE_IDS


def _params_with(**overrides):
    params = copy.deepcopy(DEFAULT_PARAMS)
    for stage_id, values in overrides.items():
        params[stage_id].update(values)
    return params


def test_incremental_invalidation_boundary_and_full_equivalence(
    sample_bundle, tmp_path
):
    sample_dir, manifest = sample_bundle
    sample = manifest["samples"][2]
    session = PipelineSession(tmp_path / "incremental.snapshot")
    session.set_image(sample_dir / sample["file"])
    first = session.run()
    assert first.status == "complete"
    assert first.diagnostics.path_type == "full"
    assert first.diagnostics.recompute_count == 3
    assert first.diagnostics.cache_hit_count == 0
    denoise_before = session.cache.get("denoise")
    sharpen_before = session.cache.get("sharpen")

    affected = session.set_stage_params(
        "local_contrast", _params_with(local_contrast={"blend": 0.55})[
            "local_contrast"
        ]
    )
    assert affected == ("local_contrast",)
    assert session.cache.get("denoise") is denoise_before
    assert session.cache.get("sharpen") is sharpen_before
    assert session.cache.get("local_contrast") is None
    assert session.stage_statuses["local_contrast"] == "invalid"

    second = session.run()
    assert second.status == "complete"
    assert second.diagnostics.path_type == "incremental"
    assert second.diagnostics.recompute_count == 1
    assert second.diagnostics.cache_hit_count == 2
    stage_stats = {stage.stage: stage for stage in second.diagnostics.stages}
    assert stage_stats["denoise"].hits == 1
    assert stage_stats["sharpen"].hits == 1
    assert stage_stats["local_contrast"].recomputes == 1
    assert second.diagnostics.effective_cache_entries <= second.diagnostics.full_stage_count
    assert session.compare_with_full().equal

    affected = session.set_stage_params(
        "sharpen", _params_with(sharpen={"amount": 1.1})["sharpen"]
    )
    assert affected == ("sharpen", "local_contrast")
    assert session.cache.get("denoise") is denoise_before
    assert session.cache.get("sharpen") is None
    assert session.cache.get("local_contrast") is None

    third = session.run()
    assert third.status == "complete"
    assert third.diagnostics.recompute_count == 2
    assert third.diagnostics.cache_hit_count == 1
    report = session.compare_with_full()
    assert report.equal
    assert report.compared_stages == list(STAGE_IDS)


def test_compare_reports_e_diff_with_first_pixel_stage(
    sample_bundle, sample_by_mode, tmp_path
):
    sample_dir, _ = sample_bundle
    sample = sample_by_mode["L"]
    calls = {"local": 0}

    def flaky_runner(stage_id, array, params, checkpoint):
        output = STAGE_FUNCTIONS[stage_id](array, params, checkpoint)
        if stage_id == "local_contrast":
            calls["local"] += 1
            if calls["local"] == 1:
                output = output.copy()
                output[0, 0] = np.uint8((int(output[0, 0]) + 1) % 256)
        return output

    session = PipelineSession(
        tmp_path / "flaky.snapshot",
        stage_runner=flaky_runner,
    )
    session.set_image(sample_dir / sample["file"])
    run = session.run()
    assert run.status == "complete"
    report = session.compare_with_full()
    assert report.equal is False
    assert report.code == "E_DIFF"
    assert report.stage == "local_contrast"
    assert (report.x, report.y, report.channel) == (0, 0, 0)
    with pytest.raises(WorkbenchError) as caught:
        session.assert_consistent()
    assert caught.value.code == "E_DIFF"
    assert caught.value.stage == "local_contrast"


def test_compare_rejects_failure_or_partial_cache(sample_bundle, tmp_path):
    sample_dir, manifest = sample_bundle
    sample_by_mode = {sample["mode"]: sample for sample in manifest["samples"]}
    sample = sample_by_mode["L"]
    session = PipelineSession(tmp_path / "partial.snapshot")
    session.set_image(sample_dir / sample["file"])
    report = session.compare_with_full()
    assert report.equal is False
    assert report.code == "E_INCOMPLETE"


def test_cache_key_depends_on_input_stage_params_upstream_and_version():
    base = {
        "input_hash": "input-a",
        "stage_id": "sharpen",
        "params": {"radius": 1, "amount": 0.8, "threshold": 2},
        "upstream_key": "upstream-a",
        "stage_version": 1,
    }
    baseline = make_cache_key(**base)
    assert baseline == make_cache_key(**base)
    assert baseline != make_cache_key(**{**base, "input_hash": "input-b"})
    assert baseline != make_cache_key(**{**base, "stage_id": "local_contrast"})
    assert baseline != make_cache_key(
        **{**base, "params": {**base["params"], "amount": 0.9}}
    )
    assert baseline != make_cache_key(**{**base, "upstream_key": "upstream-b"})
    assert baseline != make_cache_key(**{**base, "stage_version": 2})

