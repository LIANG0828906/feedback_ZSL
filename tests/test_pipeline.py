from __future__ import annotations

import hashlib
import json
from pathlib import Path

import numpy as np
import pytest

import mastering.pipeline as pipeline_module
from mastering.generator import generate_project
from mastering.limiter import true_peak_dbtp
from mastering.pipeline import _process_track, process_project
from mastering.verify import verify_master
from mastering.wavio import inspect_wav, read_wav, write_json_atomic, write_wav_atomic
from mastering.manifest import load_manifest


@pytest.fixture(scope="session")
def generated_project(tmp_path_factory):
    directory = tmp_path_factory.mktemp("generated_project")
    manifest_path = generate_project(
        directory,
        duration=8.0,
        seed=12345,
        target_loudness_lufs=-9.0,
    )
    return directory, manifest_path


def _manifest_dict(path):
    return json.loads(path.read_text(encoding="utf-8"))


def _write_variant(tmp_path, manifest, name, source_dir):
    for track in manifest["tracks"]:
        path = Path(track["file"])
        if not path.is_absolute():
            track["file"] = str((source_dir / path).resolve())
    path = tmp_path / f"{name}.json"
    write_json_atomic(path, manifest)
    return path


def test_end_to_end_master_report_and_sync_alignment(generated_project):
    directory, manifest_path = generated_project
    master = directory / "e2e_master.wav"
    report_path = directory / "e2e_report.json"
    result = process_project(manifest_path, master, report_path)
    assert result.ok
    assert master.is_file()
    report = json.loads(report_path.read_text(encoding="utf-8"))
    assert report["status"] == "ok"
    assert report["hard_errors"] == []
    assert report["master_written"] is True
    assert report["limiter"]["triggered_input_samples"] > 0
    assert report["limiter"]["output_true_peak_dbtp"] <= -1.0
    assert report["limiter"]["input_frames"] == report["limiter"]["output_frames"]
    assert abs(report["loudness"]["measured_post_gain_lufs"] - (-9.0)) <= 0.5
    assert len(report["tracks"]) == 11
    for track in report["tracks"]:
        assert track["sync_detection"] == "ok"
        assert abs(track["pulse_residual_output_samples"]) <= 0.5
        assert track["smoothed_segment_count"] == 3
        assert abs(track["total_smoothed_seconds"] - 0.15) < 1e-9

    info = inspect_wav(master)
    assert info.sample_rate == 48000
    assert info.channels == 2
    assert info.bits_per_sample == 24
    assert info.frames == 8 * 48000
    decoded, _ = read_wav(master)
    assert true_peak_dbtp(decoded) <= -1.0
    verification = verify_master(manifest_path, master)
    assert verification["status"] == "ok"
    assert verification["header"]["self_consistent"] is True


def test_master_bytes_are_deterministic(generated_project, tmp_path):
    _, manifest_path = generated_project
    first = tmp_path / "first.wav"
    second = tmp_path / "second.wav"
    first_result = process_project(manifest_path, first, tmp_path / "first.json")
    second_result = process_project(manifest_path, second, tmp_path / "second.json")
    assert first_result.ok and second_result.ok
    assert hashlib.sha256(first.read_bytes()).digest() == hashlib.sha256(
        second.read_bytes()
    ).digest()


def test_header_errors_reject_whole_project(generated_project, tmp_path):
    _, manifest_path = generated_project
    manifest = _manifest_dict(manifest_path)
    manifest["tracks"][0]["sample_rate"] = 44100
    manifest["tracks"][1]["bits_per_sample"] = 16
    manifest["tracks"][2]["channels"] = 2
    manifest["tracks"][3]["file"] = "tracks/not_here.wav"
    variant = _write_variant(
        tmp_path, manifest, "bad_headers", manifest_path.parent
    )
    master = tmp_path / "must_not_exist.wav"
    result = process_project(variant, master, tmp_path / "bad_headers_report.json")
    assert not result.ok
    assert not master.exists()
    codes = {issue["code"] for issue in result.report["hard_errors"]}
    assert {
        "SAMPLE_RATE_MISMATCH",
        "BIT_DEPTH_MISMATCH",
        "CHANNEL_COUNT_MISMATCH",
        "TRACK_FILE_NOT_FOUND",
    } <= codes
    assert all("track" in issue for issue in result.report["hard_errors"])


@pytest.mark.parametrize("kind", ["unordered", "out_of_range"])
def test_invalid_automation_rejects_before_processing(generated_project, tmp_path, kind):
    _, manifest_path = generated_project
    manifest = _manifest_dict(manifest_path)
    automation = manifest["tracks"][0]["automation"]
    if kind == "unordered":
        automation[2]["time"] = automation[1]["time"] - 0.001
    else:
        automation[0]["gain_db"] = 100.0
    variant = _write_variant(
        tmp_path, manifest, f"automation_{kind}", manifest_path.parent
    )
    master = tmp_path / f"automation_{kind}.wav"
    result = process_project(variant, master, tmp_path / f"automation_{kind}.json")
    assert not result.ok
    assert not master.exists()
    issue = result.report["hard_errors"][0]
    assert issue["code"] == "INVALID_AUTOMATION_CURVE"
    assert issue["track"] == "vocal"


def test_detection_failure_is_warning_and_processing_continues(generated_project, tmp_path):
    _, manifest_path = generated_project
    manifest = _manifest_dict(manifest_path)
    vocal = manifest["tracks"][0]
    zero_path = tmp_path / "silent_vocal.wav"
    write_wav_atomic(
        zero_path,
        np.zeros((8 * 48000, 2), dtype=np.float64),
        48000,
        24,
    )
    vocal["file"] = str(zero_path)
    variant = _write_variant(
        tmp_path, manifest, "silent_vocal", manifest_path.parent
    )
    master = tmp_path / "silent_vocal_master.wav"
    result = process_project(variant, master, tmp_path / "silent_vocal_report.json")
    assert result.ok
    warning = result.report["quality_warnings"][0]
    assert warning["code"] == "DETECTION_FAILED"
    assert warning["track"] == "vocal"
    track = next(item for item in result.report["tracks"] if item["track"] == "vocal")
    assert track["sync_detection"] == "failed"


def test_loudness_tolerance_failure_writes_no_master(
    generated_project, tmp_path, monkeypatch
):
    _, manifest_path = generated_project
    manifest = _manifest_dict(manifest_path)
    variant = _write_variant(
        tmp_path, manifest, "tight_tolerance", manifest_path.parent
    )
    measurements = iter((-3.0, -10.0))
    monkeypatch.setattr(
        pipeline_module,
        "integrated_loudness",
        lambda samples, sample_rate: next(measurements),
    )
    master = tmp_path / "tight_tolerance.wav"
    result = process_project(variant, master, tmp_path / "tight_tolerance_report.json")
    assert not result.ok
    assert not master.exists()
    assert result.report["hard_errors"][0]["code"] == "LOUDNESS_OUT_OF_TOLERANCE"
    assert result.report["loudness"]["measured_post_gain_lufs"] is not None


def test_generated_project_is_reproducible(tmp_path):
    first_dir = tmp_path / "first_generation"
    second_dir = tmp_path / "second_generation"
    first_manifest = generate_project(first_dir, duration=2.0, seed=99)
    second_manifest = generate_project(second_dir, duration=2.0, seed=99)
    assert first_manifest.read_bytes() == second_manifest.read_bytes()
    manifest = _manifest_dict(first_manifest)
    assert len(manifest["tracks"]) == 11
    for track in manifest["tracks"]:
        first_bytes = (first_dir / track["file"]).read_bytes()
        second_bytes = (second_dir / track["file"]).read_bytes()
        assert hashlib.sha256(first_bytes).digest() == hashlib.sha256(
            second_bytes
        ).digest()
