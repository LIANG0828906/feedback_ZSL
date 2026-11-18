"""Full-scale acceptance: ~100k historical rows + 128 increment batches,
matching the production incident scale, inside the resource budget."""

from __future__ import annotations

import time

from risklake.batchpath import baseline_windows
from risklake.datagen import generate
from risklake.engine import run_job

from conftest import windows_cents

TIME_BUDGET_SECONDS = 360  # spec: end-to-end under 6 minutes
MEMORY_BUDGET_BYTES = 1 << 30  # spec: peak under 1 GiB


def test_full_scale_window_join(cfg_factory):
    cfg = cfg_factory(
        "full",
        historical_rows=100_000,
        incremental_batches=128,
        incremental_rows=40_000,
        lateness_tolerance_seconds=700,
        slots=8,
    )
    started = time.time()
    generate(cfg)
    report = run_job(cfg)
    elapsed = time.time() - started

    assert elapsed < TIME_BUDGET_SECONDS
    assert report["resources"]["peak_memory_bytes"] < MEMORY_BUDGET_BYTES
    assert report["input"]["parquet_shards"] == 12
    assert report["input"]["jsonl_batches"] == 128
    assert report["input"]["batches_consumed"] == 12 + 128
    assert report["slots"]["max_load_ratio"] <= 0.40 + 1e-9
    assert report["slots"]["splits"]
    assert windows_cents(report["windows"]) == baseline_windows(cfg)
