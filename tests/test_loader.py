import json

import pytest

from caption_pipeline.errors import E_PARA, E_REF, E_SCHEMA, PipelineError
from caption_pipeline.loader import load_project
from tests.conftest import load_manifest, write_manifest


def test_load_ok_and_summary(manifest_path):
    project = load_project(manifest_path)
    summary = project.summary()
    assert summary["total_lines"] == project.total_lines == 8
    assert summary["sample_rate"] == 16000
    assert summary["param_version"] == "pv1"


def _broken(tmp_path, manifest_path, mutate):
    data = load_manifest(manifest_path)
    mutate(data)
    return write_manifest(str(tmp_path / "proj"), data,
                          audio_from=manifest_path.replace("manifest.json",
                                                           data["audio_file"]))


def test_missing_field_e_schema(tmp_path, manifest_path):
    path = _broken(tmp_path, manifest_path,
                   lambda d: d.pop("sample_rate"))
    with pytest.raises(PipelineError) as ei:
        load_project(path)
    assert ei.value.code == E_SCHEMA
    assert ei.value.field == "sample_rate"


def test_dangling_segment_ref_e_ref(tmp_path, manifest_path):
    def mutate(d):
        d["subtitles"][0]["segment_id"] = 999
    path = _broken(tmp_path, manifest_path, mutate)
    with pytest.raises(PipelineError) as ei:
        load_project(path)
    assert ei.value.code == E_REF
    assert ei.value.field == "subtitles[0].segment_id"


def test_window_out_of_segment_e_ref(tmp_path, manifest_path):
    def mutate(d):
        seg = d["segments"][0]
        d["subtitles"][0]["window"] = [seg["start"], seg["end"] + 5000]
    path = _broken(tmp_path, manifest_path, mutate)
    with pytest.raises(PipelineError) as ei:
        load_project(path)
    assert ei.value.code == E_REF
    assert ei.value.field == "subtitles[0].window"


@pytest.mark.parametrize("field,value", [
    ("max_chars_per_line", 0),
    ("line_height", -3),
    ("margin", -1),
    ("screen_height", 0),
    ("wrap_weight", -0.5),
])
def test_bad_presentation_params_e_para(tmp_path, manifest_path, field, value):
    def mutate(d):
        d["presentation"][field] = value
    path = _broken(tmp_path, manifest_path, mutate)
    with pytest.raises(PipelineError) as ei:
        load_project(path)
    assert ei.value.code == E_PARA
    assert ei.value.field == f"presentation.{field}"


def test_non_positive_cps_e_para(tmp_path, manifest_path):
    def mutate(d):
        d["subtitles"][2]["max_cps"] = 0
    path = _broken(tmp_path, manifest_path, mutate)
    with pytest.raises(PipelineError) as ei:
        load_project(path)
    assert ei.value.code == E_PARA
    assert ei.value.field == "subtitles[2].max_cps"
