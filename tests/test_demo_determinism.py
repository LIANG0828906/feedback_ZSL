from __future__ import annotations

from pathlib import Path

from broadcast_loudness.demo import run_demo
from broadcast_loudness.report import load_report


def test_demo_is_byte_reproducible(tmp_path: Path) -> None:
    operations = {
        "initial_params": {},
        "operations": [
            {"op": "play"},
            {"op": "advance", "advance_ms": 800},
            {"op": "refresh", "count": 2},
            {"op": "change_params", "target_lufs": -20.0},
            {"op": "advance", "advance_ms": 700},
            {"op": "refresh"},
            {"op": "change_params", "true_peak_limit_dbtp": -12.0},
            {"op": "advance", "advance_ms": 700},
            {"op": "refresh"},
            {"op": "change_params", "relative_gate_lu": -8.0},
            {"op": "advance", "advance_ms": 700},
            {"op": "refresh"},
            {
                "op": "change_params",
                "downmix_preset": "stereo",
                "target_lufs": -21.0,
            },
            {"op": "advance", "advance_ms": 700},
            {"op": "refresh"},
        ],
    }
    first = tmp_path / "first"
    second = tmp_path / "second"
    result_one = run_demo(
        seed=81,
        operations_document=operations,
        output_directory=first,
        channels=6,
        duration_seconds=4.0,
        bit_depth=24,
    )
    run_demo(
        seed=81,
        operations_document=operations,
        output_directory=second,
        channels=6,
        duration_seconds=4.0,
        bit_depth=24,
    )
    for name in ("sample.wav", "report.json", "trace.jsonl", "offline.json"):
        assert (first / name).read_bytes() == (second / name).read_bytes()
    assert result_one["validation"]["online_offline"]["compared_blocks"] > 0
    assert result_one["validation"]["online_integrated_lufs"] == pytest.approx(
        result_one["validation"]["offline_integrated_lufs"], abs=1e-9
    )
    loaded = load_report(first / "checkpoint.json")
    assert loaded.blocks


import pytest
