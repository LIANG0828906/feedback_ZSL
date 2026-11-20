from __future__ import annotations

from pathlib import Path

import pytest

from mixrec.engine import MixEngine
from mixrec.errors import E_MIX_INPUT, MixError
from mixrec.generator import generate_samples
from mixrec.models import MixConfig
from mixrec.wavio import read_wav
from tests.conftest import job_config_path


def test_generator_is_byte_deterministic(tmp_path: Path):
    left = tmp_path / "left"
    right = tmp_path / "right"
    generate_samples(left, seed=99, with_baseline=False)
    generate_samples(right, seed=99, with_baseline=False)
    left_files = sorted(path.relative_to(left) for path in left.rglob("*") if path.is_file())
    right_files = sorted(path.relative_to(right) for path in right.rglob("*") if path.is_file())
    assert left_files == right_files
    for relative in left_files:
        assert (left / relative).read_bytes() == (right / relative).read_bytes(), relative


def test_generator_seed_changes_pcm(tmp_path: Path):
    left = tmp_path / "left"
    right = tmp_path / "right"
    generate_samples(left, seed=1, with_baseline=False)
    generate_samples(right, seed=2, with_baseline=False)
    a = left / "jobs/mixed_rates_mono/tracks/commentary.wav"
    b = right / "jobs/mixed_rates_mono/tracks/commentary.wav"
    assert a.read_bytes() != b.read_bytes()


def test_generated_wav_rates_and_layouts(fixtures: Path):
    root = fixtures / "jobs/mixed_rates_mono/tracks"
    assert read_wav(root / "commentary.wav").sample_rate == 16000
    assert read_wav(root / "commentary.wav").channels == 1
    ambience = read_wav(root / "ambience.wav")
    assert ambience.sample_rate == 48000
    assert ambience.channels == 2


def test_declared_channel_layout_mismatch_is_mix_input(fixtures: Path, tmp_path: Path):
    import json

    source = job_config_path(fixtures, "short_timeline")
    value = json.loads(source.read_text())
    for track in value["tracks"]:
        track["path"] = str((source.parent / track["path"]).resolve())
    value["tracks"][0]["channels"] = 2
    value["output_dir"] = str(tmp_path / "output")
    value["trace_path"] = str(tmp_path / "trace.jsonl")
    config = tmp_path / "config.json"
    config.write_text(json.dumps(value))
    engine = MixEngine.from_file(config)
    with pytest.raises(MixError) as caught:
        engine.run(resume=False)
    assert caught.value.code == E_MIX_INPUT
    assert caught.value.track_id == "commentary"
