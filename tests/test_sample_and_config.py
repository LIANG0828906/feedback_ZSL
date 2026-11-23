from __future__ import annotations

import json
import tempfile
from pathlib import Path

import pytest

from acoustic_workbench.codes import E_META, E_PARA
from acoustic_workbench.config import AnalysisConfig, validate_analysis_config
from acoustic_workbench.errors import WorkbenchError
from acoustic_workbench.io import SampleRepository
from acoustic_workbench.models import RenderConfig
from acoustic_workbench.render import validate_render_config
from acoustic_workbench.samplegen import DEFAULT_SEED, generate_samples
from acoustic_workbench.models import canonical_json


def test_generated_suite_scale_and_truth(repository: SampleRepository) -> None:
    suites = {item["name"]: item for item in repository.suites()}
    assert suites["short_single"]["count"] == 1
    assert suites["long_many"]["count"] == 24
    assert repository.manifest["seed"] == DEFAULT_SEED
    sample = repository.load("short_single_000")
    assert sample.ir.shape[0] == 4
    truth_channels = sample.truth["channels"]
    assert len(truth_channels) == 4
    for channel in truth_channels:
        assert channel["direct_delay_samples"] > 0
        assert channel["early_reflection_energy"] > 0
        assert "drr_db" in channel


def test_generator_is_deterministic() -> None:
    with tempfile.TemporaryDirectory() as left_dir, tempfile.TemporaryDirectory() as right_dir:
        left = generate_samples(left_dir, seed=DEFAULT_SEED, force=True)
        right = generate_samples(right_dir, seed=DEFAULT_SEED, force=True)
        assert canonical_json(left) == canonical_json(right)
        left_ir = (Path(left_dir) / left["samples"][0]["ir_path"]).read_bytes()
        right_ir = (Path(right_dir) / right["samples"][0]["ir_path"]).read_bytes()
        assert left_ir == right_ir


def test_missing_metadata_returns_meta_code(tmp_path: Path) -> None:
    manifest = {
        "schema_version": 1,
        "samples": [
            {
                "id": "broken",
                "suite": "broken",
                "layout": {"name": "quad", "channels": []},
                "ir_path": "missing.npy",
                "dry_path": "missing.npy",
            }
        ],
    }
    (tmp_path / "manifest.json").write_text(json.dumps(manifest), encoding="utf-8")
    repository = SampleRepository(tmp_path)
    with pytest.raises(WorkbenchError) as raised:
        repository.load("broken")
    assert raised.value.code == E_META
    assert raised.value.field == "sample_rate"


def test_config_validation_names_offending_field() -> None:
    with pytest.raises(WorkbenchError) as raised:
        validate_analysis_config(AnalysisConfig(frame_length=32))
    assert raised.value.code == E_PARA
    assert raised.value.field == "frame_length"

    with pytest.raises(WorkbenchError) as raised:
        validate_analysis_config(AnalysisConfig(hop_length=8192, frame_length=4096))
    assert raised.value.code == E_PARA
    assert raised.value.field == "hop_length"

    with pytest.raises(WorkbenchError) as raised:
        validate_render_config(RenderConfig(convolution_window=8))
    assert raised.value.code == E_PARA
    assert raised.value.field == "convolution_window"
