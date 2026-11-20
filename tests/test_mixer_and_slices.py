from __future__ import annotations

from pathlib import Path

import numpy as np
import pytest

from mixrec.engine import MixEngine
from mixrec.errors import E_BUDGET_EXCEEDED, E_MIX_INPUT, MixError
from mixrec.index import load_index, validate_index
from mixrec.mixer import Mixer
from mixrec.models import MixConfig
from mixrec.wavio import read_wav
from tests.conftest import copy_config_with_paths, job_config_path


def test_stream_blocks_match_slice_rendering(fixtures: Path):
    config = MixConfig.from_file(job_config_path(fixtures, "mixed_rates_mono"))
    mixer = Mixer(config)
    total = config.resolved_total_samples()
    whole = mixer.render(0, total, block_samples=total)
    blocked = mixer.render(0, total, block_samples=777)
    assert np.array_equal(whole, blocked)


def test_slices_are_continuous_and_hash_consistent(fixtures: Path, tmp_path: Path):
    config_path = job_config_path(fixtures, "short_timeline")
    output = tmp_path / "output"
    trace = tmp_path / "trace.jsonl"
    engine = MixEngine.from_file(config_path, output_dir_override=output, trace_override=trace)
    result = engine.run(resume=False)
    assert result.ok
    index = load_index(output / "index.json")
    validate_index(index, output_dir=output, verify_files=True)
    expected_start = 0
    combined = []
    for record in index["slices"]:
        assert record["start_sample"] == expected_start
        assert record["sample_count"] == record["end_sample"] - record["start_sample"]
        expected_start = record["end_sample"]
        wav = read_wav(output / record["file"])
        combined.append(wav.pcm)
    concatenated = np.concatenate(combined, axis=0)
    full = Mixer(MixConfig.from_file(config_path)).render(
        0, index["total_samples"], block_samples=113
    )
    assert np.array_equal(concatenated, full)
    assert expected_start == index["total_samples"]


def test_delay_limit_is_rejected_with_track_id(fixtures: Path, tmp_path: Path):
    destination = tmp_path / "config.json"
    source = job_config_path(fixtures, "short_timeline")
    import json

    value = json.loads(source.read_text())
    for track in value["tracks"]:
        track["path"] = str((source.parent / track["path"]).resolve())
    value["tracks"][1]["delay_samples"] = 48001
    value["max_delay_samples"] = 48000
    value["output_dir"] = str(tmp_path / "output")
    value["trace_path"] = str(tmp_path / "trace.jsonl")
    destination.write_text(json.dumps(value))
    engine = MixEngine.from_file(destination)
    with pytest.raises(MixError) as caught:
        engine.run(resume=False)
    assert caught.value.code == E_MIX_INPUT
    assert caught.value.track_id == "ambience"


def test_budget_rejection_leaves_no_canonical_slice(fixtures: Path, tmp_path: Path):
    destination = tmp_path / "config.json"
    source = job_config_path(fixtures, "short_timeline")
    import json

    value = json.loads(source.read_text())
    for track in value["tracks"]:
        track["path"] = str((source.parent / track["path"]).resolve())
    value["scheduler"]["per_slice_budget_bytes"] = 1000
    value["output_dir"] = str(tmp_path / "output")
    value["trace_path"] = str(tmp_path / "trace.jsonl")
    destination.write_text(json.dumps(value))
    engine = MixEngine.from_file(destination)
    with pytest.raises(MixError) as caught:
        engine.run(resume=False)
    assert caught.value.code == E_BUDGET_EXCEEDED
    assert not (tmp_path / "output" / "index.json").exists()
    assert not (tmp_path / "output" / "slices").exists()
    trace = (tmp_path / "trace.jsonl").read_text().splitlines()
    budget = [line for line in trace if '"budget_rejected"' in line]
    assert budget
    assert "null" not in budget[0]
