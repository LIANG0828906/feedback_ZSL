"""Requirement 1: volume loading and calibration validation."""

import json
import os

import numpy as np
import pytest

from conftest import copy_volume
from volview.errors import ErrorCode, VolViewError
from volview.model import VolumeModel, VolumeRegistry


def _edit_manifest(volume_dir, mutate):
    path = os.path.join(volume_dir, "manifest.json")
    with open(path, encoding="utf-8") as fh:
        manifest = json.load(fh)
    mutate(manifest)
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(manifest, fh, indent=2, sort_keys=True)


def test_load_ok_builds_geometric_model(model):
    assert model.manifest.axis_order == ("Z", "Y", "X")
    assert model.data.shape == (40, 64, 64)
    assert model.shape_of_axis == {"Z": 40, "Y": 64, "X": 64}
    assert model.extent("Z") == pytest.approx(39 * 2.5)
    assert model.center_world() == pytest.approx((31.5, 31.5, 48.75))
    assert len(model.content_hash) == 64


def test_axis_order_missing_axis_reports_conflict(samples_dir, tmp_path):
    volume_dir = copy_volume(samples_dir, str(tmp_path))
    _edit_manifest(volume_dir, lambda m: m.update(axis_order="ZZX"))
    with pytest.raises(VolViewError) as exc:
        VolumeModel.load(volume_dir)
    assert exc.value.code == ErrorCode.E_AXIS
    assert exc.value.details["conflict_axis"] == "Y"


def test_axis_order_length_mismatch(samples_dir, tmp_path):
    volume_dir = copy_volume(samples_dir, str(tmp_path))
    _edit_manifest(volume_dir, lambda m: m.update(axis_order="ZY"))
    with pytest.raises(VolViewError) as exc:
        VolumeModel.load(volume_dir)
    assert exc.value.code == ErrorCode.E_AXIS
    assert exc.value.field == "axis_order"


def test_shape_mismatch_reports_conflict_axis(samples_dir, tmp_path):
    volume_dir = copy_volume(samples_dir, str(tmp_path))
    _edit_manifest(volume_dir, lambda m: m.update(shape=[41, 64, 64]))
    with pytest.raises(VolViewError) as exc:
        VolumeModel.load(volume_dir)
    assert exc.value.code == ErrorCode.E_AXIS
    assert exc.value.details["conflict_axis"] == "Z"


def test_non_positive_spacing_names_field(samples_dir, tmp_path):
    volume_dir = copy_volume(samples_dir, str(tmp_path))
    _edit_manifest(volume_dir, lambda m: m["spacing"].update(Z=0.0))
    with pytest.raises(VolViewError) as exc:
        VolumeModel.load(volume_dir)
    assert exc.value.code == ErrorCode.E_CAL
    assert exc.value.field == "spacing.Z"


def test_missing_slope_and_intercept(samples_dir, tmp_path):
    for field in ("slope", "intercept"):
        volume_dir = copy_volume(samples_dir, str(tmp_path / field))
        _edit_manifest(volume_dir, lambda m, f=field: m.pop(f))
        with pytest.raises(VolViewError) as exc:
            VolumeModel.load(volume_dir)
        assert exc.value.code == ErrorCode.E_CAL
        assert exc.value.field == field


def test_truncated_payload_is_e_decode(samples_dir, tmp_path):
    volume_dir = copy_volume(samples_dir, str(tmp_path))
    payload = os.path.join(volume_dir, "volume.npy")
    with open(payload, "r+b") as fh:
        fh.truncate(os.path.getsize(payload) // 2)
    with pytest.raises(VolViewError) as exc:
        VolumeModel.load(volume_dir)
    assert exc.value.code == ErrorCode.E_DECODE
    assert exc.value.field == "volume.npy"


def test_failed_load_leaves_no_half_initialised_model(samples_dir, tmp_path):
    registry = VolumeRegistry()
    volume_dir = copy_volume(samples_dir, str(tmp_path))
    _edit_manifest(volume_dir, lambda m: m.pop("slope"))
    with pytest.raises(VolViewError):
        registry.get(volume_dir)
    assert volume_dir not in registry
    # a valid volume still loads afterwards into the same registry
    assert registry.get(os.path.join(samples_dir, "vol_a")).manifest.volume_id == "vol_a"


def test_generator_is_deterministic(samples_dir, tmp_path):
    from volview.generator import generate_dataset
    other = tmp_path / "regen"
    generate_dataset(str(other))
    for vid in ("vol_a", "vol_b", "vol_c"):
        for name in ("volume.npy", "manifest.json", "truth.json"):
            with open(os.path.join(samples_dir, vid, name), "rb") as fh:
                original = fh.read()
            with open(os.path.join(str(other), vid, name), "rb") as fh:
                assert original == fh.read(), f"{vid}/{name} not deterministic"
