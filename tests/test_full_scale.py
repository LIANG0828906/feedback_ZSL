from __future__ import annotations

from pathlib import Path

import pytest

from minute_backfill.generator import GeneratorConfig, generate_dataset
from minute_backfill.timeutil import parse_time_us

from .helpers import normalized_windows, run_engine
from .oracle import reference_run


@pytest.mark.slow
def test_full_scale_budget_and_oracle(tmp_path: Path) -> None:
    config = GeneratorConfig(
        out_dir=tmp_path / "full-data",
        seed=42,
        payment_rows=100_000,
        refund_rows=40_000,
        batches=128,
        minutes=128,
        duplicate_rate=0.05,
        delay_rate=0.03,
        out_of_order_rate=0.20,
        force=True,
    )
    generate_dataset(config)
    report, _, _ = run_engine(
        tmp_path,
        config.out_dir,
        "full",
        slots=6,
        workers=2,
        lateness=60,
        spill_mb=128,
        memory_limit_mb=900,
    )
    assert report["timing"]["duration_seconds"] < 360
    assert report["resources"]["peak_memory_bytes"] < 1024**3
    assert len(report["windows"]) == 128
    assert report["dedup"]["dropped"] > 0
    assert report["late"]["total"] > 0
    assert report["skew"]["actual_threshold_satisfied"] is True

    oracle = reference_run(config.out_dir, 60)
    windows = normalized_windows(report)
    for window in windows:
        window["window_start"] = parse_time_us(window["window_start"])
    assert windows == oracle["windows"]
    assert report["dedup"]["dropped"] == oracle["dedup_dropped"]
    assert report["late"]["total"] == oracle["late_total"]
