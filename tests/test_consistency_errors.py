from __future__ import annotations

import copy
from pathlib import Path

import numpy as np
import pytest

from sciseq.compare import compare_results
from sciseq.config import BudgetConfig, DenoiseConfig, RegistrationConfig
from sciseq.engine import SequenceProcessor
from sciseq.errors import SciSeqError
from sciseq.export import export_with_recomputation
from sciseq.planner import plan_mode


def test_parallel_and_serial_are_pixel_identical(tiny_sequence, tiny_config) -> None:
    parallel = SequenceProcessor(tiny_sequence, tiny_config).run_full(parallel=True)
    serial = SequenceProcessor(tiny_sequence, tiny_config).run_full(parallel=False)
    report = compare_results(parallel, serial, label="serial")
    assert report["mismatch_count"] == 0
    assert parallel.decision == serial.decision


def test_decision_sequence_is_path_independent(tiny_sequence, tiny_config, tmp_path: Path) -> None:
    full = SequenceProcessor(tiny_sequence, tiny_config).run_full(parallel=True)
    incremental = SequenceProcessor(
        tiny_sequence,
        tiny_config,
        checkpoint_dir=tmp_path / "checkpoint",
    ).run_incremental(parallel=True, reset=True)
    assert incremental.complete
    assert full.decision == incremental.result.decision
    first = plan_mode(tiny_sequence.frame_count, tiny_sequence.shape, tiny_config)
    second = plan_mode(tiny_sequence.frame_count, tiny_sequence.shape, tiny_config)
    assert first.decision_hash() == second.decision_hash()
    assert list(first.decisions) == list(second.decisions)


def test_compare_reports_first_pixel_frame_and_coordinates(tiny_sequence, tiny_config) -> None:
    baseline = SequenceProcessor(tiny_sequence, tiny_config).run_full(parallel=False)
    corrupted = copy.deepcopy(baseline)
    corrupted.denoised[2, 7, 11] += np.float32(1.0)
    with pytest.raises(SciSeqError) as caught:
        compare_results(baseline, corrupted, label="corrupt")
    assert caught.value.code == "E_DIVERGE"
    assert caught.value.frame == 2
    assert caught.value.field == "denoised"
    assert caught.value.details["y"] == 7
    assert caught.value.details["x"] == 11
    assert caught.value.details["mismatch_count"] == 1
    assert caught.value.details["max_abs_diff"] == 1.0


def test_preview_recomputes_high_precision_before_export(
    tiny_sequence,
    tmp_path: Path,
) -> None:
    pipeline = tiny_config_for_preview(tiny_sequence)
    processor = SequenceProcessor(tiny_sequence, pipeline)
    decision = processor.plan()
    assert decision.mode == "preview"
    preview = processor.run_preview()
    assert preview.preview
    assert preview.downsample_factor >= 2
    output = tmp_path / "export"
    summary = export_with_recomputation(processor, preview, output)
    assert summary["export_high_precision_recomputed"]
    assert summary["preview_comparison"]["within_limit"]
    assert summary["shape"] == list(tiny_sequence.shape)
    assert (output / "denoised.npy").exists()
    exported = np.load(output / "denoised.npy")
    assert exported.shape == (tiny_sequence.frame_count, *tiny_sequence.shape)


def tiny_config_for_preview(sequence):
    from sciseq.config import PipelineConfig

    return PipelineConfig(
        reference_frame=0,
        registration=RegistrationConfig(window="none", upsample_factor=20, max_shift=6.0),
        denoise=DenoiseConfig(filter="gaussian", block_size=16, strength=1.0),
        budget=BudgetConfig(
            time_budget_s=120.0,
            memory_budget_mb=50.0,
            max_workers=2,
            frames_per_worker=1,
            preview_denoise_fraction=0.6,
            preview_mean_abs_limit=35.0,
        ),
    )


def test_budget_error_when_no_mode_fits(tiny_sequence) -> None:
    from sciseq.config import PipelineConfig

    config = PipelineConfig(
        registration=RegistrationConfig(window="none", upsample_factor=20, max_shift=6.0),
        denoise=DenoiseConfig(filter="gaussian", block_size=16, strength=1.0),
        budget=BudgetConfig(
            time_budget_s=0.1,
            memory_budget_mb=16.0,
            max_workers=4,
            frames_per_worker=2,
        ),
    )
    with pytest.raises(SciSeqError) as caught:
        plan_mode(tiny_sequence.frame_count, tiny_sequence.shape, config)
    assert caught.value.code == "E_BUDGET"

