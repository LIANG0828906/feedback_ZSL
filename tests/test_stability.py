from __future__ import annotations

import json
from pathlib import Path

from mixrec.engine import MixEngine
from mixrec.trace import TraceWriter
from tests.conftest import job_config_path


def test_trace_append_drops_incomplete_terminated_line(tmp_path: Path):
    trace = tmp_path / "trace.jsonl"
    trace.write_bytes(b'{"event":"first","seq":1}\n{"event":"partial","seq":2')
    with TraceWriter(trace, append=True) as writer:
        assert writer.sequence == 1
        writer.emit("second")
    lines = trace.read_text(encoding="utf-8").splitlines()
    values = [json.loads(line) for line in lines]
    assert [item["event"] for item in values] == ["first", "second"]
    assert [item["seq"] for item in values] == [1, 2]


def test_recovery_cleans_stale_temporary_files(fixtures: Path, tmp_path: Path):
    config_path = job_config_path(fixtures, "short_timeline")
    output = tmp_path / "output"
    trace = tmp_path / "trace.jsonl"
    engine = MixEngine.from_file(config_path, output_dir_override=output, trace_override=trace)
    assert engine.run(resume=False).ok
    stale_index = output / "index.json.tmp-999999"
    stale_wav = output / "slices/v000001/slice_000002.wav.tmp-999999"
    stale_index.write_text("partial", encoding="utf-8")
    stale_wav.write_bytes(b"partial")
    recovery = MixEngine.from_file(config_path, output_dir_override=output, trace_override=trace)
    assert recovery.run(resume=True).ok
    assert not stale_index.exists()
    assert not stale_wav.exists()
