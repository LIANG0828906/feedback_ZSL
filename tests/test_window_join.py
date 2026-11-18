"""Window join correctness: batch vs stream, dedup, slot skew handling."""

from __future__ import annotations

from risklake.batchpath import baseline_windows
from risklake.datagen import generate
from risklake.engine import run_job

from conftest import load_manifest, windows_cents


def test_batch_and_stream_paths_match(generated):
    report = run_job(generated)
    baseline = baseline_windows(generated)
    assert windows_cents(report["windows"]) == baseline
    assert report["watermark"]["late_events"] == 0


def test_dedup_drop_count_matches_manifest(generated):
    report = run_job(generated)
    manifest = load_manifest(generated)
    expected = manifest["historical"]["duplicates"] + manifest["incremental"]["duplicates"]
    assert report["input"]["dedup_dropped"] == expected
    assert report["input"]["events_unique"] == (
        report["input"]["events_read"] - expected
    )


def test_slot_count_does_not_change_results(cfg_factory):
    reports = {}
    for slots in (4, 8):
        cfg = cfg_factory(f"slots-{slots}", lateness_tolerance_seconds=700, slots=slots)
        generate(cfg)
        reports[slots] = run_job(cfg)
    assert windows_cents(reports[4]["windows"]) == windows_cents(reports[8]["windows"])


def test_skew_split_records_and_slot_cap(generated):
    generated.slots = 8
    report = run_job(generated)
    # no slot may ever exceed 40% of a batch's events
    assert report["slots"]["max_load_ratio"] <= 0.40 + 1e-9
    # the planted hot key (~45% of events) must have been split
    splits = report["slots"]["splits"]
    assert splits, "expected skew split records"
    assert all(s["key"] == "ORD-HOT-0001" for s in splits)
    assert all(s["parts"] >= 2 for s in splits)


def test_split_results_match_unsplit(cfg_factory):
    split_cfg = cfg_factory("split", lateness_tolerance_seconds=700, slots=8)
    generate(split_cfg)
    split_report = run_job(split_cfg)
    assert split_report["slots"]["splits"]

    nosplit_cfg = cfg_factory(
        "nosplit", lateness_tolerance_seconds=700, slots=8, slot_skew_threshold=0.99
    )
    generate(nosplit_cfg)
    nosplit_report = run_job(nosplit_cfg)
    assert not nosplit_report["slots"]["splits"]

    assert windows_cents(split_report["windows"]) == windows_cents(
        nosplit_report["windows"]
    )
