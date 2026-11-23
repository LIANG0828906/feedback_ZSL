from __future__ import annotations

from dataclasses import replace
from pathlib import Path

import numpy as np
import pytest

from acoustic_workbench.codes import E_DIFF
from acoustic_workbench.config import AnalysisConfig
from acoustic_workbench.errors import WorkbenchError
from acoustic_workbench.models import RenderConfig
from acoustic_workbench.pipeline import AcousticPipeline
from acoustic_workbench.verify import (
    compare_run_results,
    run_injection_suite,
    run_scale_report,
    run_three_path_verification,
)


def test_three_paths_are_sample_exact(short_sample, tmp_path: Path) -> None:
    config = AnalysisConfig(max_parallelism=2)
    render = RenderConfig(convolution_window=1024, render_block_size=512)
    verification = run_three_path_verification(
        short_sample,
        config,
        render,
        tmp_path / "paths",
        interrupt_after=4,
    )
    assert verification.report["ok"] is True
    assert verification.report["partial_committed_frames"] == 4
    assert np.array_equal(
        verification.realtime.render_result.output,
        verification.resumed.render_result.output,
    )
    assert np.array_equal(
        verification.realtime.render_result.output,
        verification.recomputed.render_result.output,
    )
    assert (
        verification.realtime_vs_resume["output_sha256"]
        == verification.realtime_vs_recompute["output_sha256"]
    )


def test_long_sample_three_paths_are_sample_exact(repository, tmp_path: Path) -> None:
    sample = repository.load("long_many_000")
    verification = run_three_path_verification(
        sample,
        AnalysisConfig(max_parallelism=3, thread_buffer_mb=32),
        RenderConfig(convolution_window=512, render_block_size=256),
        tmp_path / "long_paths",
        interrupt_after=17,
    )
    assert verification.report["ok"] is True
    assert verification.report["total_frames"] == 50
    assert verification.report["partial_committed_frames"] == 17
    assert verification.realtime.render_result.output.shape == (4, sample.dry.size)
    assert (
        verification.realtime_vs_resume["output_sha256"]
        == verification.realtime_vs_recompute["output_sha256"]
    )


def test_compare_reports_first_bad_sample_and_frame(short_sample, tmp_path: Path) -> None:
    config = AnalysisConfig(max_parallelism=1)
    render = RenderConfig(convolution_window=512, render_block_size=128)
    pipeline = AcousticPipeline(
        short_sample, config, render, tmp_path / "diff.sqlite3"
    )
    good = pipeline.run()
    pipeline.close()
    changed = np.array(good.render_result.output, copy=True)
    changed[2, 900] += 1e-6
    bad_render = replace(good.render_result, output=changed)
    bad = replace(good, render_result=bad_render)
    context_path = tmp_path / "diff_context.json"
    with pytest.raises(WorkbenchError) as raised:
        compare_run_results(
            good,
            bad,
            block_size=render.render_block_size,
            total_frames=good.total_frames,
            comparison_path=context_path,
            label="unit_diff",
        )
    assert raised.value.code == E_DIFF
    assert raised.value.frame_index == 1
    assert raised.value.context["sample_position"] == 900
    assert raised.value.context["output_channel_index"] == 2
    assert context_path.exists()


def test_four_injection_codes_and_bad_frame_retry(
    repository, short_sample, tmp_path: Path
) -> None:
    report = run_injection_suite(
        repository,
        AnalysisConfig(max_parallelism=1),
        RenderConfig(convolution_window=1024, render_block_size=512),
        tmp_path / "injections",
        retry_bad_frame=True,
    )
    assert report["ok"] is True
    observed = {item["id"]: item["observed_code"] for item in report["cases"]}
    assert observed == {
        "inject_missing_channel": "E_CHAN",
        "inject_order_inversion": "E_ORDER",
        "inject_gain_overflow": "E_CLIP",
        "inject_bad_frame": "E_FRAME",
    }
    bad_frame_case = next(
        item for item in report["cases"] if item["id"] == "inject_bad_frame"
    )
    assert bad_frame_case["progress_at_failure"]["committed_frames"] == 2
    assert bad_frame_case["bad_frame_retry_completed"] is True


def test_scale_policy_records_choices(repository) -> None:
    report = run_scale_report(repository, AnalysisConfig())
    assert report["ok"] is True
    by_suite = {item["suite"]: item for item in report["suites"]}
    assert by_suite["short_single"]["decision"]["effective_mode"] == "calibration"
    assert by_suite["short_single"]["decision"]["estimation_window"] == 4096
    assert by_suite["medium_batch"]["decision"]["effective_mode"] == "preview"
    assert by_suite["long_many"]["sample_count"] == 24
    assert by_suite["long_many"]["decision"]["estimation_window"] == 1024
    assert by_suite["long_many"]["truth_direct_delay_max_abs_error_samples"] == 0.0
    for suite in report["suites"]:
        assert suite["mode_tolerance_check"]["ok"] is True
        assert suite["decision"]["rationale"]
