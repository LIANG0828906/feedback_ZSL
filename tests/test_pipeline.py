import hashlib

import numpy as np
import pytest

from sciworkbench.engine import Workbench
from sciworkbench.errors import E_ERROR_ATTR, E_MODE_INVALID, WorkbenchError
from sciworkbench.factors import ErrorModel, FactorSet


def test_fixed_error_bounds_cover_fixed_float_deviation(prepared_sample, tmp_path):
    sequence = prepared_sample["sequence"]
    factors = prepared_sample["factors"]
    fixed_workbench = Workbench(
        block_size=16, snapshot_root=tmp_path / "fixed_snapshots"
    )
    fixed_workbench.initialize(sequence, factors, "fixed")
    fixed = fixed_workbench.process()

    float_workbench = Workbench(
        block_size=16, snapshot_root=tmp_path / "float_snapshots"
    )
    float_workbench.initialize(sequence, factors, "float")
    floating = float_workbench.process()

    deviation = np.abs(fixed.pixels - floating.pixels)
    assert np.all(fixed.error_bounds + 1.0e-12 >= deviation)
    assert fixed.fixed_float_comparison["max_abs_diff"] == pytest.approx(
        float(np.max(deviation)), rel=0.0, abs=1.0e-15
    )
    assert all(
        len(fixed.step_summaries[step]) == sequence.header.frame_count
        for step in fixed.step_summaries
    )
    for summaries in fixed.step_summaries.values():
        assert all(item.max_error_bound >= 0.0 for item in summaries)
        assert all(item.max_abs_error >= 0.0 for item in summaries)


def test_fraction_bit_change_recomputes_and_emits_summary_diff(
    prepared_sample, tmp_path
):
    sequence = prepared_sample["sequence"]
    factors = prepared_sample["factors"].replace("frac_bits", 12)
    workbench = Workbench(block_size=16, snapshot_root=tmp_path / "snapshots")
    workbench.initialize(sequence, factors, "fixed")
    first = workbench.process()
    workbench.set_factors(factors.replace("frac_bits", 5))
    second = workbench.process()

    assert second.recomputed_blocks == sequence.structure.total_blocks
    assert second.summary_diff["frac_bits_changed"] is True
    assert second.summary_diff["frac_bits_old"] == 12
    assert second.summary_diff["frac_bits_new"] == 5
    assert second.summary_diff["summary_changed"] is True
    assert all(
        item["delta_error_bound"] is not None
        for item in second.summary_diff["steps"]
    )
    assert any(
        abs(item["delta_error_bound"]) > 0.0
        for item in second.summary_diff["steps"]
    )
    assert first.overall_digest != second.overall_digest


def test_untrusted_pixels_are_attributed(prepared_sample, tmp_path):
    sequence = prepared_sample["sequence"]
    factors = prepared_sample["factors"]
    factors = factors.replace(
        "error",
        ErrorModel(
            dark_error=factors.error.dark_error,
            flat_rel_error=factors.error.flat_rel_error,
            flat_floor=factors.error.flat_floor,
            threshold=0.0,
        ),
    )
    workbench = Workbench(block_size=16, snapshot_root=tmp_path / "snapshots")
    workbench.initialize(sequence, factors, "fixed")
    result = workbench.process()
    assert np.all(result.untrusted)
    assert np.all(result.attribution >= 0)
    assert np.all(result.attribution <= 4)
    assert result.fixed_float_comparison["pixel_count"] == result.untrusted.size


def test_non_finite_pipeline_returns_error_attribution(prepared_sample, tmp_path):
    sequence = prepared_sample["sequence"]
    factors = prepared_sample["factors"]
    factors = factors.replace(
        "block_gain",
        np.full_like(factors.block_gain, 1.0e308, dtype=np.float64),
    )
    workbench = Workbench(block_size=16, snapshot_root=tmp_path / "snapshots")
    workbench.initialize(sequence, factors, "fixed")
    with pytest.raises(WorkbenchError) as caught:
        workbench.process()
    assert caught.value.code == E_ERROR_ATTR
    assert caught.value.frame == 0
    assert caught.value.block is not None
    assert caught.value.pixel is not None


def test_mode_comparison_is_complete_and_semantically_aligned(
    prepared_sample, tmp_path
):
    sequence = prepared_sample["sequence"]
    factors = prepared_sample["factors"]
    fixed_workbench = Workbench(
        block_size=16, snapshot_root=tmp_path / "fixed"
    )
    fixed_workbench.initialize(sequence, factors, "fixed")
    fixed = fixed_workbench.process()
    floating_workbench = Workbench(
        block_size=16, snapshot_root=tmp_path / "float"
    )
    floating_workbench.initialize(sequence, factors, "float")
    floating = floating_workbench.process()

    report = fixed_workbench.compare_modes()
    expected_fixed_digest = hashlib.sha256(
        np.ascontiguousarray(fixed.pixels, dtype="<f8").tobytes(order="C")
    ).hexdigest()
    expected_float_digest = hashlib.sha256(
        np.ascontiguousarray(floating.pixels, dtype="<f8").tobytes(order="C")
    ).hexdigest()
    assert report["fixed_digest"] == expected_fixed_digest
    assert report["float_digest"] == expected_float_digest
    assert report["operator_semantics"] == "identical"
    assert len(report["frames"]) == sequence.header.frame_count
    assert report["mean_abs_diff"] <= report["max_abs_diff"]
    assert report["p95_abs_diff"] <= report["max_abs_diff"]


def test_uninitialized_workbench_returns_mode_invalid(tmp_path):
    workbench = Workbench(block_size=16, snapshot_root=tmp_path / "snapshots")
    with pytest.raises(WorkbenchError) as caught:
        workbench.process()
    assert caught.value.code == E_MODE_INVALID


def test_invalid_mode_returns_mode_invalid(prepared_sample, tmp_path):
    workbench = Workbench(block_size=16, snapshot_root=tmp_path / "snapshots")
    workbench.initialize(
        prepared_sample["sequence"], prepared_sample["factors"], "fixed"
    )
    with pytest.raises(WorkbenchError) as caught:
        workbench.process(mode="quad")
    assert caught.value.code == E_MODE_INVALID
