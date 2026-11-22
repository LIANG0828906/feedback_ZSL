from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import pytest

from sciseq.errors import SciSeqError
from sciseq.generator import DEFAULT_GROUPS, GroupSpec, generate_group
from sciseq.loader import load_sequence


def test_default_group_coverage() -> None:
    assert len(DEFAULT_GROUPS) >= 3
    assert min(group.frame_count for group in DEFAULT_GROUPS) >= 20
    assert max(group.frame_count for group in DEFAULT_GROUPS) >= 90
    assert min(group.height for group in DEFAULT_GROUPS) >= 300
    assert max(group.width for group in DEFAULT_GROUPS) >= 1000


def test_generator_is_deterministic(tmp_path: Path) -> None:
    spec = GroupSpec("deterministic", 96, 128, 5, 9182, 2.5, 6.0, 2.0)
    first = generate_group(tmp_path / "a", spec)
    second = generate_group(tmp_path / "b", spec)
    first_manifest = json.loads((first / "manifest.json").read_text(encoding="utf-8"))
    second_manifest = json.loads((second / "manifest.json").read_text(encoding="utf-8"))
    assert first_manifest["frame_sha256"] == second_manifest["frame_sha256"]
    assert first_manifest["clean_base_sha256"] == second_manifest["clean_base_sha256"]
    assert (first / "truth.json").read_bytes() == (second / "truth.json").read_bytes()
    sequence = load_sequence(first)
    assert np.all(np.isfinite(sequence.truth.shifts))
    assert sequence.truth.shifts[sequence.reference_frame].tolist() == [0.0, 0.0]


def test_loader_accepts_valid_sequence(tiny_sequence) -> None:
    assert tiny_sequence.frame_count == 8
    assert tiny_sequence.frame(0).shape == tiny_sequence.shape
    assert tiny_sequence.truth.frame_features[3].keys() == {
        feature.feature_id for feature in tiny_sequence.truth.features
    }


def test_missing_metadata_returns_e_meta(sequence_copy: Path) -> None:
    manifest_path = sequence_copy / "manifest.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    frame_count = manifest.pop("frame_count")
    manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
    try:
        load_sequence(sequence_copy)
    except SciSeqError as exc:
        assert exc.code == "E_META"
        assert exc.field == "manifest.frame_count"
    else:
        raise AssertionError("missing metadata did not fail")
    assert frame_count == 8


def test_corrupt_frame_returns_e_decode(sequence_copy: Path) -> None:
    manifest = json.loads((sequence_copy / "manifest.json").read_text(encoding="utf-8"))
    (sequence_copy / manifest["frame_files"][3]).write_bytes(b"broken numpy payload")
    with pytest.raises(SciSeqError) as caught:
        load_sequence(sequence_copy)
    assert caught.value.code == "E_DECODE"
    assert caught.value.frame == 3
    assert caught.value.field == "frame_file"


def test_non_finite_truth_offset_returns_e_meta(sequence_copy: Path) -> None:
    truth_path = sequence_copy / "truth.json"
    truth = json.loads(truth_path.read_text(encoding="utf-8"))
    truth["shifts"][2]["dx"] = float("nan")
    truth_path.write_text(json.dumps(truth), encoding="utf-8")
    with pytest.raises(SciSeqError) as caught:
        load_sequence(sequence_copy, verify_hashes=False)
    assert caught.value.code == "E_META"
    assert caught.value.frame == 2
    assert caught.value.field == "truth.shifts"

