from __future__ import annotations

import hashlib
import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest

from offline_master.codes import BusinessError, ErrorCode
from offline_master.pipeline import render_project


def _digest(path):
    with open(path, "rb") as handle:
        return hashlib.sha256(handle.read()).hexdigest()


def test_full_pipeline_meets_all_business_rules(project_dir, tmp_path):
    project, manifest = project_dir
    out = tmp_path / "out"
    report = render_project(str(project / "manifest.json"), str(out))

    assert report["status"] == "OK"
    assert report["master_samples"] == int(
        manifest["duration_s"] * manifest["sample_rate"]
    )

    # Requirement 3: integer-sample alignment, ambient detection warning.
    by_file = {row["file"]: row for row in report["alignment"]}
    for track in manifest["tracks"]:
        row = by_file[track["file"]]
        if track["material_type"] == "ambient":
            assert row["status"] == "DETECTION_FAILED"
        else:
            assert row["status"] == "ALIGNED"
            assert abs(row["residual_error_samples"]) <= 1
    assert any(w["reason"] == "DETECTION_FAILED" for w in report["warnings"])

    # Requirement 4: stability, no side reversal, azimuth truth within 15 deg.
    for stability in report["stability"]:
        assert stability["within_threshold"]
        assert stability["side_reversal_angles"] == []
    for accuracy in report["azimuth_accuracy"]:
        assert all(frame["within_15_deg"] for frame in accuracy["frames"])

    # Requirement 5: polarity recovered, low-band gain >= 6 dB, <=1 sample move.
    pair = report["phase_pairs"][0]
    assert pair["polarity"] == -1
    assert report["phase_summary"]["groups"][0]["meets_6db"]
    assert all(row["within_one_sample"] for row in report["transient_displacement"])

    # Requirement 7: loudness within +/-0.5 LU and true peak below ceiling.
    assert abs(report["loudness"]["loudness_error_lu"]) <= 0.5
    assert report["loudness"]["true_peak_after_db"] <= manifest["mastering"]["true_peak_db"]


def test_master_render_is_byte_identical_across_runs(project_dir, tmp_path):
    project, _ = project_dir
    out_a = tmp_path / "a"
    out_b = tmp_path / "b"
    render_project(str(project / "manifest.json"), str(out_a))
    render_project(str(project / "manifest.json"), str(out_b))
    assert _digest(out_a / "master.wav") == _digest(out_b / "master.wav")


def test_snapshot_resume_matches_uninterrupted_render(project_dir, tmp_path):
    project, manifest = project_dir
    full_out = tmp_path / "full"
    render_project(str(project / "manifest.json"), str(full_out))

    resumed_out = tmp_path / "resumed"
    snapshot_dir = resumed_out / "snapshot"
    env = dict(os.environ)
    env["PYTHONPATH"] = str(Path(__file__).resolve().parents[1])
    cmd = [
        sys.executable,
        "-m",
        "offline_master.demo",
        "render",
        str(project / "manifest.json"),
        "--out",
        str(resumed_out),
        "--block-sleep",
        "0.3",
    ]
    process = subprocess.Popen(
        cmd, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE
    )
    snapshot_file = snapshot_dir / "snapshot.json"
    deadline = time.time() + 20.0
    while time.time() < deadline and not snapshot_file.is_file():
        time.sleep(0.05)
    assert snapshot_file.is_file()
    process.send_signal(signal.SIGINT)
    stdout, stderr = process.communicate(timeout=30)
    assert process.returncode == 0
    assert (snapshot_dir / "snapshot.json").is_file()
    assert (snapshot_dir / "recovery_report.json").is_file()

    # Resume completes from the committed snapshot and must match byte-for-byte.
    report = render_project(str(project / "manifest.json"), str(resumed_out))
    assert report["status"] == "OK"
    assert _digest(full_out / "master.wav") == _digest(resumed_out / "master.wav")


def test_resume_mismatch_discards_stale_snapshot(project_dir, tmp_path):
    project, manifest = project_dir
    out = tmp_path / "out"
    render_project(str(project / "manifest.json"), str(out))
    snapshot = out / "snapshot" / "snapshot.json"
    payload = json.loads(snapshot.read_text())
    payload["manifest_digest"] = "stale"
    snapshot.write_text(json.dumps(payload))

    try:
        render_project(str(project / "manifest.json"), str(out))
    except BusinessError as exc:
        assert exc.code == ErrorCode.E_RESUME_MISMATCH
    else:  # pragma: no cover - defensive
        raise AssertionError("stale snapshot was not rejected")

    # After rejection the stale state is removed and a fresh render succeeds.
    report = render_project(str(project / "manifest.json"), str(out))
    assert report["status"] == "OK"


def test_invalid_manifest_cli_exit_code(project_dir, tmp_path):
    project, manifest = project_dir
    manifest["turntable_angles_deg"] = [0, 5]
    bad = project / "bad_manifest.json"
    bad.write_text(json.dumps(manifest))
    with pytest.raises(BusinessError) as exc:
        render_project(str(bad), str(tmp_path / "never"))
    assert exc.value.code == ErrorCode.E_MANIFEST_INVALID
