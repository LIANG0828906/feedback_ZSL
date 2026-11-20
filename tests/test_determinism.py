from __future__ import annotations

from pathlib import Path

from spectral_editor.demo import run_demo, verify_determinism


def test_same_seed_and_operations_are_byte_identical(tmp_path: Path):
    run_a = tmp_path / "a"
    run_b = tmp_path / "b"
    summary_a = run_demo(1234, run_a)
    summary_b = run_demo(1234, run_b)
    assert summary_a == summary_b
    for relative in (
        "materials.json",
        "observer_2deg.json",
        "observer_10deg.json",
        "illuminant_d65.json",
        "baseline_hashes.json",
        "trace.jsonl",
        "summary.json",
        "state/snapshot.json",
        "state/recovery_anchor.json",
    ):
        assert (run_a / relative).read_bytes() == (run_b / relative).read_bytes()


def test_verify_report_marks_all_files_equal(tmp_path: Path):
    report = verify_determinism(42, tmp_path / "verify")
    assert report["byte_identical"] is True
    assert all(item["equal"] for item in report["files"].values())


def test_trace_contains_required_fields_without_timestamp_truth(tmp_path: Path):
    import json

    out = tmp_path / "run"
    run_demo(9, out)
    lines = (out / "trace.jsonl").read_text(encoding="utf-8").splitlines()
    assert len(lines) == 7
    for line in lines:
        event = json.loads(line)
        assert {
            "generation",
            "affected_material_ids",
            "affected_versions",
            "reused_versions",
            "recomputed_count",
            "reused_count",
            "max_channel_deltas",
            "materials",
            "snapshot_hash",
            "publication_hash",
        }.issubset(event)
        assert "timestamp" not in event
        assert "file_size" not in event
        for material in event["materials"].values():
            assert material["content_hash"]
            assert material["chain_hash"]
