from __future__ import annotations

from pathlib import Path

import pytest

from volrender.demo import (
    render_budget_trajectory,
    run_all_injections,
)


def test_all_injections_have_independent_codes_and_no_images(tmp_path: Path, volume_path: Path) -> None:
    expected = {
        "dim_mismatch": ("E_VOLUME_INVALID", "DIM_LENGTH_MISMATCH"),
        "window_nonfinite": ("E_VOLUME_INVALID", "WINDOW_NON_FINITE"),
        "tf_out_of_range": ("E_TF_INVALID", "CONTROL_POINT_OUT_OF_RANGE"),
        "truncated_voxels": ("E_VOLUME_INVALID", "DATA_TRUNCATED"),
        "budget_overflow": ("E_BUDGET_EXCEEDED", "DEGRADATION_EXHAUSTED"),
    }
    rows = run_all_injections(volume_path=volume_path, root_output=tmp_path / "injections")
    assert {row["injection"] for row in rows} == set(expected)
    for row in rows:
        injection = row["injection"]
        code, detail = expected[injection]
        error = row["error"]
        assert error["code"] == code
        assert error["detail"] == detail
        assert error["inject"] == injection
        assert error["injection_point"].endswith(f":{injection}")
        assert "field" in error or "usage" in error
        output = Path(row["trace"]).parent
        assert sorted(path.name for path in output.iterdir()) == ["trace.jsonl"]


def test_demo_is_byte_identical_for_same_seed(tmp_path: Path, volume_path: Path) -> None:
    first = render_budget_trajectory(
        volume_path=volume_path,
        output_dir=tmp_path / "a",
        tier="small",
        seed=42,
    )
    second = render_budget_trajectory(
        volume_path=volume_path,
        output_dir=tmp_path / "b",
        tier="small",
        seed=42,
    )
    assert first["repeat_identical"] and second["repeat_identical"]
    assert [result.summary_dict() for result in first["results"]] == [
        result.summary_dict() for result in second["results"]
    ]
    names = ["trace.jsonl", "render.png", "error_heatmap.png", "snapshot.json"]
    for name in names:
        assert (tmp_path / "a" / name).read_bytes() == (tmp_path / "b" / name).read_bytes()
    trace = (tmp_path / "a" / "trace.jsonl").read_text(encoding="utf-8")
    assert "timestamp" not in trace
    assert '"degradation_stages":["increase_step","lower_early_term","reduce_batch"]' in trace
    assert '"event":"repeat_check","identical":true' in trace

