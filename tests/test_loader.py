"""需求 1: 加载与结构校验, E_SCHEMA / E_PARA。"""

import json
import os
import shutil

import pytest

from a11y_media.errors import E_PARA, E_SCHEMA, ComponentError
from a11y_media.loader import load_project

import sys
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from scripts.make_corrupt_samples import make_all


@pytest.fixture(scope="module")
def corrupt_dir(samples_dir, tmp_path_factory):
    src = os.path.join(samples_dir, sorted(os.listdir(samples_dir))[0])
    out = tmp_path_factory.mktemp("corrupt")
    make_all(src, str(out))
    return str(out)


def _expect(path, code, field=None):
    with pytest.raises(ComponentError) as ei:
        load_project(path)
    assert ei.value.code == code, f"{path}: {ei.value}"
    if field is not None:
        assert ei.value.context.get("field") == field


def test_valid_project_loads(project):
    assert project.sample_rate > 0
    assert len(project.subtitles) == len(project.assist.char_weights)
    assert len(project.truth.windows) == len(project.subtitles)


def test_missing_field(corrupt_dir):
    _expect(os.path.join(corrupt_dir, "missing_field"), E_SCHEMA, "manifest.frame_len")


def test_bad_segment_ref(corrupt_dir):
    _expect(os.path.join(corrupt_dir, "bad_segment_ref"), E_SCHEMA,
            "subtitles[0].segment")


def test_window_out_of_segment(corrupt_dir):
    _expect(os.path.join(corrupt_dir, "window_out_of_segment"), E_SCHEMA,
            "subtitles[0].window")


def test_bad_speed(corrupt_dir):
    _expect(os.path.join(corrupt_dir, "bad_speed"), E_PARA, "assist.speed")


def test_nan_speed(corrupt_dir):
    _expect(os.path.join(corrupt_dir, "nan_speed"), E_PARA, "assist.speed")


def test_zero_weights(corrupt_dir):
    _expect(os.path.join(corrupt_dir, "zero_weights"), E_PARA, "assist.char_weights")


def test_bad_hop(corrupt_dir):
    _expect(os.path.join(corrupt_dir, "bad_hop"), E_PARA, "hop")


def test_total_samples_mismatch(corrupt_dir):
    _expect(os.path.join(corrupt_dir, "bad_total_samples"), E_SCHEMA,
            "total_samples")


def test_missing_manifest(tmp_path):
    _expect(str(tmp_path), E_SCHEMA)


def test_broken_json(samples_dir, tmp_path):
    src = os.path.join(samples_dir, sorted(os.listdir(samples_dir))[0])
    dst = tmp_path / "broken_json"
    shutil.copytree(src, dst)
    with open(dst / "manifest.json", "w") as f:
        f.write("{not json")
    _expect(str(dst), E_SCHEMA)
