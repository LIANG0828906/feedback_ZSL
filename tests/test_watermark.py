"""Watermark / lateness semantics."""

from __future__ import annotations

import json
import os
from pathlib import Path

import pyarrow.parquet as pq

from risklake.datagen import generate
from risklake.engine import run_job

HISTORICAL_SHARDS = 12


def _oracle(cfg) -> tuple[int, int]:
    """Independent replay of the declared semantics.

    Dedup: first occurrence of an event_id wins.  Watermark is the max
    event time seen minus tolerance and never regresses.  An event whose
    window end is at or before the watermark on arrival is late.
    """
    data_dir = Path(cfg.work_dir) / cfg.data_dir
    files = sorted((data_dir / "historical").rglob("*.parquet")) + sorted(
        (data_dir / "incremental").glob("batch_*.jsonl")
    )
    seen = set()
    watermark = -(1 << 62)
    max_seen = -(1 << 62)
    late = 0
    for path in files:
        if path.suffix == ".parquet":
            rows = pq.read_table(path).to_pylist()
        else:
            rows = [json.loads(l) for l in path.read_text().splitlines() if l.strip()]
        for e in rows:
            if e["event_id"] in seen:
                continue
            seen.add(e["event_id"])
            t = int(e["event_time"])
            window_start = (t // cfg.window_seconds) * cfg.window_seconds
            if window_start + cfg.window_seconds <= watermark:
                late += 1
                continue
            max_seen = max(max_seen, t)
            candidate = max_seen - cfg.lateness_tolerance_seconds
            prev = watermark
            watermark = max(watermark, candidate)
            assert watermark >= prev, "watermark must never regress"
    return late, watermark


def test_zero_tolerance_lateness_matches_oracle(cfg_factory):
    cfg = cfg_factory("late0", lateness_tolerance_seconds=0)
    generate(cfg)
    report = run_job(cfg)
    expected_late, expected_watermark = _oracle(cfg)
    assert expected_late > 0, "dataset should contain late events at tolerance 0"
    assert report["watermark"]["late_events"] == expected_late
    assert report["watermark"]["final"] == expected_watermark


def test_large_tolerance_has_no_late_events(cfg_factory):
    cfg = cfg_factory("late-big", lateness_tolerance_seconds=10_000_000)
    generate(cfg)
    report = run_job(cfg)
    assert report["watermark"]["late_events"] == 0


def test_missing_batch_does_not_stall_or_regress(cfg_factory):
    cfg = cfg_factory("missing", lateness_tolerance_seconds=0)
    generate(cfg)
    data_dir = Path(cfg.work_dir) / cfg.data_dir
    batches = sorted((data_dir / "incremental").glob("batch_*.jsonl"))
    os.remove(batches[len(batches) // 2])
    report = run_job(cfg)
    assert report["input"]["skipped_batches"] == 1
    assert report["input"]["batches_consumed"] == HISTORICAL_SHARDS + cfg.incremental_batches
    _, expected_watermark = _oracle(cfg)
    assert report["watermark"]["final"] == expected_watermark
