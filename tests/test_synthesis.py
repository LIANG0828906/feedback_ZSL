from __future__ import annotations

import hashlib

import soundfile as sf

from offline_master.synthesis import synthesize_project


def _digest(path):
    with open(path, "rb") as handle:
        return hashlib.sha256(handle.read()).hexdigest()


def test_synthesis_is_byte_identical(project_dir, tmp_path):
    project, manifest = project_dir
    first = tmp_path / "a"
    second = tmp_path / "b"
    synthesize_project(manifest, str(first))
    synthesize_project(manifest, str(second))
    for track in manifest["tracks"]:
        assert (
            _digest(first / track["file"])
            == _digest(second / track["file"])
        )


def test_declared_start_offset_is_recoverable(project_dir):
    project, manifest = project_dir
    dialogue = next(t for t in manifest["tracks"] if t["material_type"] == "dialogue")
    data, sr = sf.read(project / dialogue["file"])
    offset_samples = int(round(dialogue["start_offset_s"] * sr))
    assert not data[:offset_samples].any()
    assert data[offset_samples:].any()


def test_all_stems_match_manifest_format(project_dir):
    project, manifest = project_dir
    for track in manifest["tracks"]:
        info = sf.info(project / track["file"])
        assert info.samplerate == manifest["sample_rate"]
        assert info.channels == manifest["channels"]
        assert info.subtype == "PCM_16"
