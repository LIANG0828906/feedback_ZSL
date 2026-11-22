"""Requirement 7: anomaly injection chains and the independent verifier."""

import json
import os

import numpy as np
import pytest

from volview import inject, verify
from volview.errors import ErrorCode
from volview.model import VolumeRegistry
from volview.session import ReadingSession
from volview.snapshot import SnapshotStore


@pytest.mark.parametrize("case,code,field", [
    ("missing-intercept", ErrorCode.E_CAL, "intercept"),
    ("window-width-zero", ErrorCode.E_WIN, "window_width"),
    ("thickness-overflow", ErrorCode.E_THICK, "thickness"),
    ("annotation-oob", ErrorCode.E_RANGE, "annotation[0].endpoint[1]"),
    ("truncated-volume", ErrorCode.E_DECODE, "volume.npy"),
])
def test_injection_cases(samples_dir, tmp_path, case, code, field):
    result = inject.run_case(case, samples_dir, str(tmp_path / case))
    assert result["ok"], result
    assert result["error"]["code"] == code
    assert result["error"]["field"] == field


def _write_snapshot(volume_dir, store, session_id="verify-me"):
    session = ReadingSession.open(VolumeRegistry(), volume_dir, store,
                                  session_id)
    session.set_window(1200.0, 400.0)
    session.set_plane("axial", 50.0)
    session.add_annotation([22.0, 32.0, 50.0], [42.0, 32.0, 50.0])
    session.commit()
    return session


def test_verifier_accepts_consistent_snapshot(volume_dir, samples_dir,
                                              tmp_path):
    store = SnapshotStore(str(tmp_path / "snap"))
    _write_snapshot(volume_dir, store)
    rc = verify.main(["--snapshots", store.directory, "--samples", samples_dir])
    assert rc == 0


def test_verifier_reports_first_pixel_mismatch(volume_dir, samples_dir,
                                               tmp_path):
    store = SnapshotStore(str(tmp_path / "snap"))
    _write_snapshot(volume_dir, store)
    plane_path = os.path.join(store.directory, "snapshot_verify-me_plane.npy")
    plane = np.load(plane_path)
    plane[3, 5] = plane[3, 5] ^ 0xFF  # tamper one pixel
    np.save(plane_path, plane)
    result = verify.verify_snapshot(
        os.path.join(store.directory, "snapshot_verify-me.json"), samples_dir)
    assert not result["ok"]
    assert result["plane"] == "axial"
    assert result["pixel"] == [3, 5]
    assert result["field"] == "display_gray"


def test_verifier_reports_annotation_field_mismatch(volume_dir, samples_dir,
                                                    tmp_path):
    store = SnapshotStore(str(tmp_path / "snap"))
    _write_snapshot(volume_dir, store)
    snapshot_path = os.path.join(store.directory, "snapshot_verify-me.json")
    with open(snapshot_path, encoding="utf-8") as fh:
        snapshot = json.load(fh)
    snapshot["annotations"][0]["world_length"] += 1.0  # tamper reading
    with open(snapshot_path, "w", encoding="utf-8") as fh:
        json.dump(snapshot, fh, indent=2, sort_keys=True)
    result = verify.verify_snapshot(snapshot_path, samples_dir)
    assert not result["ok"]
    assert result["annotation_id"] == 0
    assert result["field"] == "world_length"
