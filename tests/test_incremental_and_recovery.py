from __future__ import annotations

import hashlib
import json
import shutil
import time
from pathlib import Path

import pytest

from mixrec.engine import ControlToken, MixEngine
from mixrec.errors import E_BUSY, E_BUDGET_EXCEEDED, E_RECOVERY_INVALID, MixError
from mixrec.index import load_index, validate_index
from tests.conftest import job_config_path


def test_incremental_recomputes_only_affected_slices(fixtures: Path, tmp_path: Path):
    config_path = job_config_path(fixtures, "mixed_rates_mono")
    output = tmp_path / "output"
    trace = tmp_path / "trace.jsonl"
    engine = MixEngine.from_file(config_path, output_dir_override=output, trace_override=trace)
    first = engine.run(resume=False)
    assert first.ok
    before = load_index(output / "index.json")
    old_hashes = [item["content_sha256"] for item in before["slices"]]
    old_files = [item["file"] for item in before["slices"]]

    after = engine.update_track("ambience", mute_ranges=[[0.4, 0.44]])
    new_hashes = [item["content_sha256"] for item in after["slices"]]
    assert after["index_version"] == before["index_version"] + 1
    affected = [i for i, (a, b) in enumerate(zip(old_hashes, new_hashes)) if a != b]
    assert affected == [4, 5, 6]
    assert all(old_files[i] == after["slices"][i]["file"] for i in range(len(old_files)) if i not in affected)
    assert all(new_hashes[i] == old_hashes[i] for i in range(len(old_hashes)) if i not in affected)
    validate_index(after, output_dir=output, verify_files=True)
    events = [json.loads(line) for line in trace.read_text().splitlines()]
    start = next(item for item in events if item["event"] == "incremental_start")
    assert start["affected_slices"] == [4, 5, 6]
    assert start["changed_ranges"] == [[18271, 19231], [21151, 24991]]


def test_incremental_budget_failure_keeps_old_index(fixtures: Path, tmp_path: Path):
    config_path = job_config_path(fixtures, "mixed_rates_mono")
    output = tmp_path / "output"
    trace = tmp_path / "trace.jsonl"
    engine = MixEngine.from_file(config_path, output_dir_override=output, trace_override=trace)
    assert engine.run(resume=False).ok
    before_bytes = (output / "index.json").read_bytes()
    before_files = sorted(path.relative_to(output) for path in output.rglob("*.wav"))
    engine.config.per_slice_budget_bytes = 100
    with pytest.raises(MixError) as caught:
        engine.update_track("ambience", gain_db=-1.0)
    assert caught.value.code == E_BUDGET_EXCEEDED
    assert (output / "index.json").read_bytes() == before_bytes
    assert sorted(path.relative_to(output) for path in output.rglob("*.wav")) == before_files


def test_resume_after_committed_prefix_matches_full_run(fixtures: Path, tmp_path: Path):
    config_path = job_config_path(fixtures, "mixed_rates_mono")
    full_output = tmp_path / "full"
    full_engine = MixEngine.from_file(
        config_path,
        output_dir_override=full_output,
        trace_override=tmp_path / "full-trace.jsonl",
    )
    full = full_engine.run(resume=False)
    assert full.ok

    prefix_output = tmp_path / "prefix"
    trace = tmp_path / "prefix-trace.jsonl"
    prefix_engine = MixEngine.from_file(
        config_path, output_dir_override=prefix_output, trace_override=trace
    )
    prefix = prefix_engine.run(resume=False, max_slices=3)
    assert prefix.stopped_early
    assert prefix.index["committed_slices"] == 3
    resumed_engine = MixEngine.from_file(
        config_path, output_dir_override=prefix_output, trace_override=trace
    )
    resumed = resumed_engine.run(resume=True)
    assert resumed.ok
    assert resumed.summary["content_hashes"] == full.summary["content_hashes"]
    assert resumed.summary["chain_head"] == full.summary["chain_head"]
    assert resumed.summary["index_version"] == full.summary["index_version"]
    validate_index(load_index(prefix_output / "index.json"), output_dir=prefix_output)


def test_corrupt_slice_is_recovery_invalid_and_never_overwritten(fixtures: Path, tmp_path: Path):
    config_path = job_config_path(fixtures, "short_timeline")
    output = tmp_path / "output"
    trace = tmp_path / "trace.jsonl"
    engine = MixEngine.from_file(config_path, output_dir_override=output, trace_override=trace)
    assert engine.run(resume=False).ok
    index = load_index(output / "index.json")
    victim = output / index["slices"][2]["file"]
    data = bytearray(victim.read_bytes())
    data[60] ^= 0xFF
    victim.write_bytes(bytes(data))
    corrupt_bytes = victim.read_bytes()
    index_bytes = (output / "index.json").read_bytes()
    recovery = MixEngine.from_file(config_path, output_dir_override=output, trace_override=trace)
    with pytest.raises(MixError) as caught:
        recovery.run(resume=True)
    assert caught.value.code == E_RECOVERY_INVALID
    assert victim.read_bytes() == corrupt_bytes
    assert (output / "index.json").read_bytes() == index_bytes


def test_duplicate_start_returns_busy(fixtures: Path, tmp_path: Path):
    config_path = job_config_path(fixtures, "mixed_rates_mono")
    output = tmp_path / "output"
    trace = tmp_path / "trace.jsonl"
    first = MixEngine.from_file(config_path, output_dir_override=output, trace_override=trace)
    control = ControlToken()
    control.pause()
    thread = first.start_async(resume=False, control=control)
    for _ in range(100):
        if str(output.resolve()) in MixEngine.active_jobs():
            break
        time.sleep(0.01)
    second = MixEngine.from_file(
        config_path, output_dir_override=output, trace_override=tmp_path / "second.jsonl"
    )
    with pytest.raises(MixError) as caught:
        second.run(resume=False)
    assert caught.value.code == E_BUSY
    control.cancel()
    thread.join(timeout=5)
    assert not thread.is_alive()


def test_tampered_index_version_fails_seal(fixtures: Path, tmp_path: Path):
    config_path = job_config_path(fixtures, "short_timeline")
    output = tmp_path / "output"
    trace = tmp_path / "trace.jsonl"
    engine = MixEngine.from_file(config_path, output_dir_override=output, trace_override=trace)
    assert engine.run(resume=False).ok
    index_path = output / "index.json"
    index = json.loads(index_path.read_text())
    index["index_version"] += 1
    index_path.write_text(json.dumps(index))
    recovery = MixEngine.from_file(config_path, output_dir_override=output, trace_override=trace)
    with pytest.raises(MixError) as caught:
        recovery.run(resume=True)
    assert caught.value.code == E_RECOVERY_INVALID


def test_high_priority_never_reorders_commit(fixtures: Path, tmp_path: Path):
    config_path = job_config_path(fixtures, "short_timeline")
    output = tmp_path / "output"
    trace = tmp_path / "trace.jsonl"
    engine = MixEngine.from_file(config_path, output_dir_override=output, trace_override=trace)
    engine.config.task_priorities = {3: 1000, 4: 999}
    result = engine.run(resume=False)
    assert result.ok
    events = [json.loads(line) for line in trace.read_text().splitlines()]
    committed = [item["slice_index"] for item in events if item["event"] == "slice_committed"]
    assert committed == list(range(5))
    starts = [item for item in events if item["event"] == "task_started"]
    assert [item["slice_index"] for item in starts] == list(range(5))
    assert starts[3]["priority"] == 1000
