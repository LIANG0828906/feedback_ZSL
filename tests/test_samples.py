from __future__ import annotations

import json
from pathlib import Path

from game_audio_restorer.samples import generate_dataset


def _tree_bytes(root: Path, patterns: tuple[str, ...]) -> dict[str, bytes]:
    output: dict[str, bytes] = {}
    for pattern in patterns:
        for path in sorted(root.glob(pattern)):
            if path.is_file():
                output[path.relative_to(root).as_posix()] = path.read_bytes()
    return output


def test_generator_is_byte_deterministic(tmp_path: Path) -> None:
    options = {
        "seed": 1234,
        "sample_rates": (8000, 16000),
        "channel_layouts": (1, 2),
        "duration_s": 0.9,
        "segment_seconds": 0.5,
    }
    first = generate_dataset(tmp_path / "first", **options)
    second = generate_dataset(tmp_path / "second", **options)
    assert first["sample_count"] == 4
    assert first["run_count"] == 12
    assert first["trace_sha256"] == second["trace_sha256"]
    assert first["baseline_hash"] == second["baseline_hash"]

    patterns = (
        "samples/*.wav",
        "runs/*/v*/restored.wav",
        "runs/*/v*/trace.jsonl",
        "runs/*/comparison_trace.jsonl",
        "trace.jsonl",
        "baseline.json",
        "recipes/*.json",
    )
    first_files = _tree_bytes(tmp_path / "first", patterns)
    second_files = _tree_bytes(tmp_path / "second", patterns)
    assert first_files.keys() == second_files.keys()
    for name in first_files:
        assert first_files[name] == second_files[name], name

    trace = (tmp_path / "first" / "trace.jsonl").read_text("utf-8").splitlines()
    assert len(trace) == 1 + 4 * 3 * 2
    header = json.loads(trace[0])
    assert header["seed"] == 1234
    row = json.loads(trace[1])
    assert {
        "sample_id",
        "segment",
        "recipe_version",
        "field_mapping",
        "metrics",
        "content_sha256",
    } <= set(row)

    baseline = json.loads((tmp_path / "first" / "baseline.json").read_text("utf-8"))
    assert baseline["trace_sha256"] == first["trace_sha256"]
    assert baseline["hash"] == first["baseline_hash"]
