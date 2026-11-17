import json
import os

import numpy as np
import soundfile as sf

from applause_master.loudness import integrated_loudness, true_peak, true_peak_limit
from applause_master.pipeline import run_pipeline


def _run(demo_workspace, out_name, interrupted=None):
    out = os.path.join(demo_workspace["work"], out_name)
    return run_pipeline(demo_workspace["project"], out, interrupted=interrupted)


def test_full_pipeline_compliant_report(demo_workspace):
    result = _run(demo_workspace, "out_a")
    assert result["status"] == "complete"
    report = json.load(open(result["report"]))
    project = demo_workspace["project"]
    assert abs(report["loudness"]["measured_integrated_lufs"]
               - project["target_loudness_lufs"]) <= 0.5
    assert report["loudness"]["measured_true_peak_dbtp"] <= project["true_peak_limit_dbtp"] + 1e-9
    assert report["total_samples"] == int(
        project["duration_seconds"] * project["sample_rate"]
    )
    master, sr = sf.read(result["master"], always_2d=True)
    assert sr == project["sample_rate"]
    assert master.shape == (report["total_samples"], 2)


def test_master_is_byte_deterministic(demo_workspace):
    first = _run(demo_workspace, "out_b1")
    second = _run(demo_workspace, "out_b2")
    with open(first["master"], "rb") as handle:
        bytes_a = handle.read()
    with open(second["master"], "rb") as handle:
        bytes_b = handle.read()
    assert bytes_a == bytes_b
    report_a = json.load(open(first["report"]))
    report_b = json.load(open(second["report"]))
    assert report_a["master_sha256"] == report_b["master_sha256"]
    assert report_a == report_b


def test_report_has_no_timestamps_or_absolute_paths(demo_workspace):
    result = _run(demo_workspace, "out_c")
    text = open(result["report"]).read()
    for forbidden in ("timestamp", "time_utc", "/home/", "C:\\", "tmp"):
        assert forbidden not in text
    report = json.load(open(result["report"]))
    assert list(report)[0] == "seed"


def test_limiter_keeps_length_and_removes_overshoot():
    sr = 48000
    rng = np.random.default_rng(3)
    signal = rng.standard_normal((sr, 2)) * 0.15
    signal[20000] *= 12.0
    limited, info = true_peak_limit(signal, sr, -1.0)
    assert limited.shape == signal.shape
    tp, linear = true_peak(limited, sr)
    assert linear <= 10.0 ** (-1.0 / 20.0) * 1.0001
    assert info["triggered_samples"] > 0
    # No discontinuity larger than the raw material's natural jumps.
    raw_jumps = np.abs(np.diff(signal, axis=0))
    limited_jumps = np.abs(np.diff(limited, axis=0))
    assert limited_jumps.max() <= raw_jumps.max() * 1.1


def test_integrated_loudness_is_finite():
    sr = 48000
    signal = np.random.default_rng(4).standard_normal((sr * 2, 2)) * 0.1
    loudness = integrated_loudness(signal, sr)
    assert np.isfinite(loudness)
    assert -40.0 < loudness < -10.0


def test_snapshot_resume_matches_one_shot_render(demo_workspace):
    project = demo_workspace["project"]
    interrupted_out = os.path.join(demo_workspace["work"], "out_interrupt")
    calls = {"n": 0}

    def fire_once():
        calls["n"] += 1
        return calls["n"] == 4

    first = run_pipeline(project, interrupted_out, interrupted=fire_once)
    assert first["status"] == "interrupted"
    assert first["exit_code"] == 0
    assert os.path.isfile(first["snapshot"])
    resume_report = json.load(open(os.path.join(interrupted_out, "resume_report.json")))
    assert resume_report["completed_blocks"] > 0

    resumed = run_pipeline(project, interrupted_out)
    reference = run_pipeline(project, os.path.join(demo_workspace["work"], "out_ref"))
    with open(resumed["master"], "rb") as handle:
        resumed_bytes = handle.read()
    with open(reference["master"], "rb") as handle:
        reference_bytes = handle.read()
    assert resumed_bytes == reference_bytes
    report = json.load(open(resumed["report"]))
    assert report["resume_count"] == 1


def test_stale_snapshot_is_rejected_and_restarted(demo_workspace):
    project = demo_workspace["project"]
    out = os.path.join(demo_workspace["work"], "out_stale")
    run_pipeline(project, out)
    snapshot_path = os.path.join(out, "snapshot.json")
    snapshot = json.load(open(snapshot_path))
    snapshot["input_digest"] = "0" * 64
    with open(snapshot_path, "w") as handle:
        json.dump(snapshot, handle)
    result = run_pipeline(project, out)
    report = json.load(open(result["report"]))
    assert report["resume_mismatch_restart"] is True
    fresh_snapshot = json.load(open(snapshot_path))
    assert fresh_snapshot["completed_blocks"] > 0
