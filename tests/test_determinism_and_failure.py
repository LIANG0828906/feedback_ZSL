from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest

from mixrec.engine import MixEngine
from mixrec.errors import E_SLICE_GAP, MixError
from mixrec.index import load_index, validate_index
from mixrec.mixer import Mixer
from tests.conftest import job_config_path


def _tree_hashes(root: Path) -> dict[str, str]:
    return {
        str(path.relative_to(root)): hashlib.sha256(path.read_bytes()).hexdigest()
        for path in sorted(root.rglob("*"))
        if path.is_file()
    }


def test_same_operations_produce_identical_slices_and_trace(fixtures: Path, tmp_path: Path):
    config_path = job_config_path(fixtures, "mixed_rates_stereo")
    first_out = tmp_path / "first"
    second_out = tmp_path / "second"
    first_trace = tmp_path / "first.jsonl"
    second_trace = tmp_path / "second.jsonl"
    first = MixEngine.from_file(config_path, output_dir_override=first_out, trace_override=first_trace)
    second = MixEngine.from_file(config_path, output_dir_override=second_out, trace_override=second_trace)
    assert first.run(resume=False).ok
    assert second.run(resume=False).ok
    assert first_trace.read_bytes() == second_trace.read_bytes()
    left = _tree_hashes(first_out / "slices")
    right = _tree_hashes(second_out / "slices")
    assert left == right


def test_gap_failure_keeps_only_continuous_prefix(fixtures: Path, tmp_path: Path, monkeypatch):
    config_path = job_config_path(fixtures, "short_timeline")
    output = tmp_path / "output"
    trace = tmp_path / "trace.jsonl"
    engine = MixEngine.from_file(config_path, output_dir_override=output, trace_override=trace)
    original_render = Mixer.render

    def broken_render(self, start_sample, end_sample, *, block_samples=None):
        pcm = original_render(self, start_sample, end_sample, block_samples=block_samples)
        if start_sample == 3000:
            return pcm[:-1]
        return pcm

    monkeypatch.setattr(Mixer, "render", broken_render)
    with pytest.raises(MixError) as caught:
        engine.run(resume=False)
    assert caught.value.code == E_SLICE_GAP
    assert caught.value.slice_index == 3
    index = load_index(output / "index.json")
    assert index["committed_slices"] == 3
    assert [item["index"] for item in index["slices"]] == [0, 1, 2]
    assert not (output / "slices/v000001/slice_000003.wav").exists()
    validate_index(index, output_dir=output, verify_files=True)


def test_trace_contains_memory_reservation_and_waits(fixtures: Path, tmp_path: Path):
    config_path = job_config_path(fixtures, "short_timeline")
    output = tmp_path / "output"
    trace = tmp_path / "trace.jsonl"
    engine = MixEngine.from_file(config_path, output_dir_override=output, trace_override=trace)
    # Force a one-task window so the second task observes the global budget.
    engine.config.memory_budget_bytes = engine.config.per_slice_budget_bytes
    result = engine.run(resume=False)
    assert result.ok
    events = [json.loads(line) for line in trace.read_text().splitlines()]
    assert any(item["event"] == "memory_reserved" for item in events)
    assert any(item["event"] == "memory_released" for item in events)
    for event in events:
        if event["event"] == "budget_rejected":
            assert event["seq"] is not None
