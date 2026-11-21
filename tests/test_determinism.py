from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

from haptic_studio.samples import generate_samples


def _tree_bytes(root: Path) -> dict[str, bytes]:
    return {
        str(path.relative_to(root)): path.read_bytes()
        for path in sorted(root.rglob("*"))
        if path.is_file()
    }


def test_sample_generator_is_byte_deterministic(tmp_path: Path) -> None:
    first = tmp_path / "first"
    second = tmp_path / "second"
    first_manifest = generate_samples(first, seed=9911)
    second_manifest = generate_samples(second, seed=9911)
    assert first_manifest == second_manifest
    assert _tree_bytes(first) == _tree_bytes(second)
    other = tmp_path / "other"
    other_manifest = generate_samples(other, seed=9912)
    assert (
        other_manifest["samples"][0]["wave_hash"]
        != first_manifest["samples"][0]["wave_hash"]
    )


def test_demo_trace_and_signals_are_byte_reproducible(tmp_path: Path) -> None:
    first = tmp_path / "demo1"
    second = tmp_path / "demo2"
    command = [
        sys.executable,
        "-m",
        "haptic_studio",
        "demo",
        "--sample",
        "dense_9",
        "--out",
    ]
    one = subprocess.run(
        [*command, str(first)],
        check=False,
        capture_output=True,
        text=True,
        timeout=60,
    )
    two = subprocess.run(
        [*command, str(second)],
        check=False,
        capture_output=True,
        text=True,
        timeout=60,
    )
    assert one.returncode == 0, one.stderr
    assert two.returncode == 0, two.stderr
    assert (first / "trace.jsonl").read_bytes() == (second / "trace.jsonl").read_bytes()
    first_signals = {
        path.name: path.read_bytes() for path in sorted((first / "signals").iterdir())
    }
    second_signals = {
        path.name: path.read_bytes() for path in sorted((second / "signals").iterdir())
    }
    assert first_signals == second_signals
    first_summary = json.loads((first / "summary.json").read_text(encoding="utf-8"))
    second_summary = json.loads((second / "summary.json").read_text(encoding="utf-8"))
    assert first_summary == second_summary
    assert all(first_summary["baseline_matches"].values())
    assert all(item["match"] for item in first_summary["history_verification"])
    trace_lines = (first / "trace.jsonl").read_text(encoding="utf-8").splitlines()
    publish_lines = [json.loads(line) for line in trace_lines if '"publish"' in line]
    assert len(publish_lines) == 5
    required = {
        "version",
        "affected_ranges",
        "recomputed_interval_count",
        "reused_interval_count",
        "limited_sample_count",
        "signal_hash",
    }
    assert required.issubset(publish_lines[0])
