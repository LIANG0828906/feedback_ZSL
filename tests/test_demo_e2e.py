import json
from pathlib import Path

from loudmeter.demo import run


def test_demo_outputs_and_recovery(tmp_path):
    report = run(tmp_path / "w1")
    assert report["recovery"]["consistent"] is True
    for name in ("trace.jsonl", "blocks.jsonl", "report.json", "config.json"):
        assert (tmp_path / "w1" / name).exists()
    # every block trace entry carries the required fields
    for line in open(tmp_path / "w1" / "blocks.jsonl"):
        e = json.loads(line)
        for key in ("asset_id", "block_index", "short_term_lufs", "gated_in",
                    "parallelism", "budget_used", "content_hash",
                    "sync_marker", "tick"):
            assert key in e


def test_demo_determinism_byte_identical(tmp_path):
    run(tmp_path / "w1")
    run(tmp_path / "w2")
    for name in ("trace.jsonl", "blocks.jsonl", "report.json"):
        a = (tmp_path / "w1" / name).read_bytes()
        b = (tmp_path / "w2" / name).read_bytes()
        assert a == b, f"{name} differs between identical runs"


def test_no_wallclock_in_trace(tmp_path):
    run(tmp_path / "w1")
    text = (tmp_path / "w1" / "trace.jsonl").read_text()
    for forbidden in ("time", "timestamp", "date"):
        assert forbidden not in text
