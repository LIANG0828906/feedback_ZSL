import json
import os

import pytest

from subpipeline.errors import E_REF, E_SCHEMA, E_WIN, PipelineError
from subpipeline.loader import load_project


def _project(tmp_path, samples_root):
    src = os.path.join(samples_root, "proj_main_20261123")
    proj_dir = tmp_path / "proj"
    proj_dir.mkdir()
    for name in os.listdir(src):
        data = open(os.path.join(src, name), "rb").read()
        (proj_dir / name).write_bytes(data)
    return str(proj_dir)


def test_loads_valid_project(tmp_path, samples_root):
    project, manifest, wave, computed = load_project(
        _project(tmp_path, samples_root))
    assert project.sample_rate == 16000
    assert len(project.segments) == 8
    assert project.params.max_chars_per_line == 10
    assert wave is not None


def test_missing_top_level_field(tmp_path, samples_root):
    proj_dir = _project(tmp_path, samples_root)
    manifest = json.load(open(os.path.join(proj_dir, "manifest.json")))
    del manifest["sample_rate"]
    json.dump(manifest, open(os.path.join(proj_dir, "manifest.json"), "w"))
    with pytest.raises(PipelineError) as exc:
        load_project(proj_dir)
    assert exc.value.code == E_SCHEMA
    assert exc.value.field == "sample_rate"


def test_missing_subtitle_field(tmp_path, samples_root):
    proj_dir = _project(tmp_path, samples_root)
    manifest = json.load(open(os.path.join(proj_dir, "manifest.json")))
    del manifest["subtitles"][0]["text"]
    json.dump(manifest, open(os.path.join(proj_dir, "manifest.json"), "w"))
    with pytest.raises(PipelineError) as exc:
        load_project(proj_dir)
    assert exc.value.code == E_SCHEMA
    assert exc.value.subtitle_id == 1
    assert exc.value.field == "text"


def test_bad_segment_reference(tmp_path, samples_root):
    proj_dir = _project(tmp_path, samples_root)
    manifest = json.load(open(os.path.join(proj_dir, "manifest.json")))
    manifest["subtitles"][0]["segment_ref"] = "seg99"
    json.dump(manifest, open(os.path.join(proj_dir, "manifest.json"), "w"))
    with pytest.raises(PipelineError) as exc:
        load_project(proj_dir)
    assert exc.value.code == E_REF
    assert exc.value.subtitle_id == 1
    assert exc.value.field == "segment_ref"


def test_window_outside_segment(tmp_path, samples_root):
    proj_dir = _project(tmp_path, samples_root)
    manifest = json.load(open(os.path.join(proj_dir, "manifest.json")))
    sub = manifest["subtitles"][0]
    seg = next(s for s in manifest["segments"]
               if s["segment_id"] == sub["segment_ref"])
    sub["explicit_start"] = seg["start_sample"]
    sub["explicit_end"] = seg["end_sample"] + 100
    json.dump(manifest, open(os.path.join(proj_dir, "manifest.json"), "w"))
    with pytest.raises(PipelineError) as exc:
        load_project(proj_dir)
    assert exc.value.code == E_WIN
    assert exc.value.subtitle_id == 1


def test_non_positive_params(tmp_path, samples_root):
    proj_dir = _project(tmp_path, samples_root)
    manifest = json.load(open(os.path.join(proj_dir, "manifest.json")))
    manifest["params"]["line_height_px"] = 0
    json.dump(manifest, open(os.path.join(proj_dir, "manifest.json"), "w"))
    with pytest.raises(PipelineError) as exc:
        load_project(proj_dir)
    assert exc.value.code == E_SCHEMA
    assert exc.value.field == "line_height_px"


def test_non_positive_read_speed(tmp_path, samples_root):
    proj_dir = _project(tmp_path, samples_root)
    manifest = json.load(open(os.path.join(proj_dir, "manifest.json")))
    manifest["subtitles"][0]["max_read_speed"] = -1
    json.dump(manifest, open(os.path.join(proj_dir, "manifest.json"), "w"))
    with pytest.raises(PipelineError) as exc:
        load_project(proj_dir)
    assert exc.value.code == E_SCHEMA
    assert exc.value.field == "max_read_speed"


def test_corrupt_manifest_is_decode_error(tmp_path, samples_root):
    proj_dir = _project(tmp_path, samples_root)
    with open(os.path.join(proj_dir, "manifest.json"), "w") as fh:
        fh.write("{not json")
    from subpipeline.errors import E_DECODE
    with pytest.raises(PipelineError) as exc:
        load_project(proj_dir)
    assert exc.value.code == E_DECODE
