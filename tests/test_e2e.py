from __future__ import annotations

import hashlib
import json
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf

from theatre_binaural.snapshot import _snapshot_checksum


ROOT = Path(__file__).resolve().parents[1]
EXAMPLE_PROJECT = ROOT / "examples" / "project.json"


def run_cli(*arguments: str, timeout: float = 120.0) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, "-m", "theatre_binaural", *arguments],
        cwd=ROOT,
        text=True,
        capture_output=True,
        timeout=timeout,
    )


def parse_error(result: subprocess.CompletedProcess[str]) -> dict[str, object]:
    assert result.returncode != 0
    assert result.stderr.strip(), result.stdout
    return json.loads(result.stderr)


def copy_project_workspace(tmp_path: Path) -> tuple[Path, dict[str, object]]:
    shutil.copytree(EXAMPLE_PROJECT.parent / "audio", tmp_path / "audio")
    project = json.loads(EXAMPLE_PROJECT.read_text(encoding="utf-8"))
    project_path = tmp_path / "project.json"
    project_path.write_text(
        json.dumps(project, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    return project_path, project


def wait_for_blocks(state_dir: Path, minimum: int, timeout: float = 30.0) -> int:
    deadline = time.monotonic() + timeout
    completed = 0
    while time.monotonic() < deadline:
        snapshot = state_dir / "snapshot.json"
        if snapshot.is_file():
            try:
                completed = int(
                    json.loads(snapshot.read_text(encoding="utf-8"))["completed_blocks"]
                )
            except (json.JSONDecodeError, KeyError, ValueError):
                completed = 0
            if completed >= minimum:
                return completed
        time.sleep(0.02)
    raise AssertionError(f"snapshot did not reach {minimum} blocks")


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


@pytest.fixture(scope="session")
def baseline(tmp_path_factory: pytest.TempPathFactory) -> dict[str, object]:
    directory = tmp_path_factory.mktemp("baseline")
    result = run_cli(
        "render",
        "--project",
        str(EXAMPLE_PROJECT),
        "--master",
        str(directory / "master.wav"),
        "--report",
        str(directory / "report.json"),
        "--state-dir",
        str(directory / "state"),
        "--fresh",
    )
    assert result.returncode == 0, result.stderr
    report = json.loads((directory / "report.json").read_text(encoding="utf-8"))
    return {
        "directory": directory,
        "master": directory / "master.wav",
        "report": report,
        "state": directory / "state",
        "sha256": sha256(directory / "master.wav"),
    }


def test_generation_is_byte_identical(tmp_path: Path) -> None:
    project_path, _ = copy_project_workspace(tmp_path)
    for path in (tmp_path / "audio").glob("*.wav"):
        path.unlink()
    first = run_cli("generate", "--project", str(project_path))
    assert first.returncode == 0, first.stderr
    first_hashes = {
        path.name: sha256(path) for path in sorted((tmp_path / "audio").glob("*.wav"))
    }
    second = run_cli("generate", "--project", str(project_path))
    assert second.returncode == 0, second.stderr
    second_hashes = {
        path.name: sha256(path) for path in sorted((tmp_path / "audio").glob("*.wav"))
    }
    assert first_hashes == second_hashes
    generated = json.loads(first.stdout)
    assert generated["status"] == "ok"
    assert len(generated["tracks"]) == 8
    assert set(generated["leakage"]["injection_channels"]) == {
        "actor_01",
        "actor_03",
        "actor_05",
    }


def test_validation_errors_are_structured(tmp_path: Path) -> None:
    project_path, project = copy_project_workspace(tmp_path)

    missing = copy_project_workspace(tmp_path / "missing")[0]
    (tmp_path / "missing" / "audio" / "actor_04.wav").unlink()
    error = parse_error(run_cli("validate", "--project", str(missing)))
    assert error["error_code"] == "E_FILE_MISSING"
    assert error["channel"] == "actor_04"

    sample_rate_project, _ = copy_project_workspace(tmp_path / "rate")
    target = tmp_path / "rate" / "audio" / "actor_01.wav"
    audio, _ = sf.read(target, always_2d=True)
    sf.write(target, audio, 44100, subtype="PCM_24")
    error = parse_error(run_cli("validate", "--project", str(sample_rate_project)))
    assert error["error_code"] == "E_SAMPLE_RATE_MISMATCH"
    assert error["channel"] == "actor_01"

    bit_depth_project, _ = copy_project_workspace(tmp_path / "bits")
    target = tmp_path / "bits" / "audio" / "actor_02.wav"
    audio, rate = sf.read(target, always_2d=True)
    sf.write(target, audio, rate, subtype="PCM_16")
    error = parse_error(run_cli("validate", "--project", str(bit_depth_project)))
    assert error["error_code"] == "E_BIT_DEPTH_MISMATCH"
    assert error["channel"] == "actor_02"

    channels_project, _ = copy_project_workspace(tmp_path / "channels")
    target = tmp_path / "channels" / "audio" / "actor_03.wav"
    audio, rate = sf.read(target, always_2d=True)
    sf.write(target, np.concatenate((audio, audio), axis=1), rate, subtype="PCM_24")
    error = parse_error(run_cli("validate", "--project", str(channels_project)))
    assert error["error_code"] == "E_CHANNEL_COUNT_MISMATCH"
    assert error["channel"] == "actor_03"

    tracking_project, tracking = copy_project_workspace(tmp_path / "tracking")
    tracking["head_tracking"]["samples"][1]["t"] = 0.0
    tracking_project.write_text(json.dumps(tracking, indent=2) + "\n", encoding="utf-8")
    error = parse_error(run_cli("validate", "--project", str(tracking_project)))
    assert error["error_code"] == "E_TRACKING_TIME_NOT_STRICT"
    assert error["channel"] == "head_tracking"

    quaternion_project, quaternion = copy_project_workspace(tmp_path / "quaternion")
    quaternion["head_tracking"]["samples"][5]["orientation_quaternion"][0] *= 1.5
    quaternion_project.write_text(
        json.dumps(quaternion, indent=2) + "\n", encoding="utf-8"
    )
    error = parse_error(run_cli("validate", "--project", str(quaternion_project)))
    assert error["error_code"] == "E_QUATERNION_NOT_NORMALIZED"
    assert error["channel"] == "head_tracking"


def test_full_render_compliance_and_report(baseline: dict[str, object]) -> None:
    report = baseline["report"]
    assert report["status"] == "ok"
    master = report["master"]
    assert abs(master["integrated_loudness_lufs"] - (-23.0)) <= 0.5
    assert master["true_peak_dbtp"] <= master["true_peak_limit_dbtp"]
    assert master["frames"] == report["mix"]["bus_frames"] == 288000
    info = sf.info(str(baseline["master"]))
    assert info.frames == 288000
    assert info.channels == 2

    injected = {
        estimate["channel"]: estimate
        for estimate in report["leakage"]
    }
    for channel in ("actor_01", "actor_03", "actor_05"):
        assert injected[channel]["reduction_db"] >= 12.0
        assert injected[channel]["direct_peak_loss_db"] <= 1.0
    assert abs(injected["actor_01"]["delay_ms"] - 9.0) <= 1.0

    rate = report["binaural"]["rate"]
    assert rate["max_ild_rate_db_per_s"] <= rate["ild_rate_limit_db_per_s"]
    assert rate["max_itd_rate_us_per_s"] <= rate["itd_rate_limit_us_per_s"]
    interpolation = report["binaural"]["interpolation"]
    assert interpolation["max_azimuth_error_deg"] <= interpolation["tolerance_deg"]
    for check in interpolation["checks"]:
        assert check["within_tolerance"] is True
        assert check["ild_db"] == pytest.approx(
            check["right_gain_db"] - check["left_gain_db"]
        )
        assert check["itd_us"] == pytest.approx(
            (check["right_delay_ms"] - check["left_delay_ms"]) * 1000.0
        )
    assert (
        abs(sum(item["contribution_ratio"] for item in report["mix"]["contributions"]) - 1.0)
        < 1.0e-6
    )
    serialized = json.dumps(report, ensure_ascii=False)
    assert str(baseline["directory"]) not in serialized
    assert str(ROOT) not in serialized


def test_one_shot_report_is_stable(baseline: dict[str, object], tmp_path: Path) -> None:
    result = run_cli(
        "render",
        "--project",
        str(EXAMPLE_PROJECT),
        "--master",
        str(tmp_path / "master.wav"),
        "--report",
        str(tmp_path / "report.json"),
        "--state-dir",
        str(tmp_path / "state"),
        "--fresh",
    )
    assert result.returncode == 0, result.stderr
    assert sha256(tmp_path / "master.wav") == baseline["sha256"]
    assert (tmp_path / "report.json").read_bytes() == (
        baseline["directory"] / "report.json"
    ).read_bytes()


def test_actual_sigkill_resume_is_byte_identical(
    baseline: dict[str, object], tmp_path: Path
) -> None:
    process = subprocess.Popen(
        [
            sys.executable,
            "-m",
            "theatre_binaural",
            "render",
            "--project",
            str(EXAMPLE_PROJECT),
            "--master",
            str(tmp_path / "master.wav"),
            "--report",
            str(tmp_path / "report.json"),
            "--state-dir",
            str(tmp_path / "state"),
            "--fresh",
            "--block-delay-ms",
            "20",
        ],
        cwd=ROOT,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        completed = wait_for_blocks(tmp_path / "state", 3)
        assert completed < 120
        process.kill()
        process.wait(timeout=10)
    finally:
        if process.poll() is None:
            process.kill()
    assert process.returncode != 0
    assert not (tmp_path / "master.wav").exists()

    resumed = run_cli(
        "render",
        "--project",
        str(EXAMPLE_PROJECT),
        "--master",
        str(tmp_path / "master.wav"),
        "--report",
        str(tmp_path / "report.json"),
        "--state-dir",
        str(tmp_path / "state"),
    )
    assert resumed.returncode == 0, resumed.stderr
    assert sha256(tmp_path / "master.wav") == baseline["sha256"]
    report = json.loads((tmp_path / "report.json").read_text(encoding="utf-8"))
    assert report["resume"]["snapshot_verified"] is True
    assert report["resume"]["resumed_blocks"] >= 3


def test_sigint_finishes_block_and_writes_recovery_report(
    baseline: dict[str, object], tmp_path: Path
) -> None:
    process = subprocess.Popen(
        [
            sys.executable,
            "-m",
            "theatre_binaural",
            "render",
            "--project",
            str(EXAMPLE_PROJECT),
            "--master",
            str(tmp_path / "master.wav"),
            "--report",
            str(tmp_path / "report.json"),
            "--state-dir",
            str(tmp_path / "state"),
            "--fresh",
            "--block-delay-ms",
            "30",
        ],
        cwd=ROOT,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        wait_for_blocks(tmp_path / "state", 2)
        process.send_signal(signal.SIGINT)
        stdout, stderr = process.communicate(timeout=20)
    finally:
        if process.poll() is None:
            process.kill()
    assert process.returncode == 0, stderr
    recovery_path = tmp_path / "report.recovery.json"
    assert recovery_path.is_file()
    recovery = json.loads(recovery_path.read_text(encoding="utf-8"))
    assert recovery["status"] == "interrupted"
    assert recovery["completed_blocks"] >= 2
    assert recovery["next_block"] == recovery["completed_blocks"]
    assert not (tmp_path / "master.wav").exists()

    resumed = run_cli(
        "render",
        "--project",
        str(EXAMPLE_PROJECT),
        "--master",
        str(tmp_path / "master.wav"),
        "--report",
        str(tmp_path / "report.json"),
        "--state-dir",
        str(tmp_path / "state"),
    )
    assert resumed.returncode == 0, resumed.stderr
    assert sha256(tmp_path / "master.wav") == baseline["sha256"]


def test_resume_mismatch_discards_progress(
    baseline: dict[str, object], tmp_path: Path
) -> None:
    state = tmp_path / "state"
    shutil.copytree(baseline["state"], state)
    project_path, project = copy_project_workspace(tmp_path / "changed")
    project["seed"] += 1
    project_path.write_text(json.dumps(project, indent=2) + "\n", encoding="utf-8")
    result = run_cli(
        "render",
        "--project",
        str(project_path),
        "--master",
        str(tmp_path / "master.wav"),
        "--report",
        str(tmp_path / "report.json"),
        "--state-dir",
        str(state),
    )
    error = parse_error(result)
    assert result.returncode == 3
    assert error["error_code"] == "E_RESUME_MISMATCH"
    assert not (state / "snapshot.json").exists()


def test_snapshot_corruption_is_detected(
    baseline: dict[str, object], tmp_path: Path
) -> None:
    state = tmp_path / "state"
    shutil.copytree(baseline["state"], state)
    snapshot_path = state / "snapshot.json"
    snapshot = json.loads(snapshot_path.read_text(encoding="utf-8"))
    snapshot["completed_blocks"] += 1
    snapshot_path.write_text(json.dumps(snapshot), encoding="utf-8")
    result = run_cli(
        "render",
        "--project",
        str(EXAMPLE_PROJECT),
        "--master",
        str(tmp_path / "master.wav"),
        "--report",
        str(tmp_path / "report.json"),
        "--state-dir",
        str(state),
    )
    error = parse_error(result)
    assert result.returncode == 4
    assert error["error_code"] == "E_SNAPSHOT_CORRUPT"
    assert error["channel"] == "snapshot"


def test_resume_divergence_reports_block_and_channel(
    baseline: dict[str, object], tmp_path: Path
) -> None:
    state = tmp_path / "state"
    shutil.copytree(baseline["state"], state)
    snapshot_path = state / "snapshot.json"
    snapshot = json.loads(snapshot_path.read_text(encoding="utf-8"))
    entry = snapshot["blocks"][0]
    block_path = state / entry["file"]
    raw = bytearray(block_path.read_bytes())
    raw[0] ^= 0x01
    block_path.write_bytes(raw)
    entry["sha256"] = hashlib.sha256(raw).hexdigest()
    snapshot["checksum"] = _snapshot_checksum(snapshot)
    snapshot_path.write_text(
        json.dumps(snapshot, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    result = run_cli(
        "render",
        "--project",
        str(EXAMPLE_PROJECT),
        "--master",
        str(tmp_path / "master.wav"),
        "--report",
        str(tmp_path / "report.json"),
        "--state-dir",
        str(state),
    )
    error = parse_error(result)
    assert result.returncode == 5
    assert error["error_code"] == "E_RESUME_DIVERGENCE"
    differences = error["details"]["differences"]
    assert differences[0]["block"] == 0
    assert differences[0]["channel"] in {"left", "right"}


def test_limiter_preserves_length_and_compliance(tmp_path: Path) -> None:
    project_path, project = copy_project_workspace(tmp_path)
    project["targets"]["loudness_lufs"] = -8.0
    project_path.write_text(json.dumps(project, indent=2) + "\n", encoding="utf-8")
    result = run_cli(
        "render",
        "--project",
        str(project_path),
        "--master",
        str(tmp_path / "master.wav"),
        "--report",
        str(tmp_path / "report.json"),
        "--state-dir",
        str(tmp_path / "state"),
        "--fresh",
    )
    assert result.returncode == 0, result.stderr
    report = json.loads((tmp_path / "report.json").read_text(encoding="utf-8"))
    limiter = report["master"]["limiter"]
    assert limiter["applied"] is True
    assert limiter["latency_compensated"] is True
    assert limiter["triggered_samples"] > 0
    assert limiter["triggered_samples"] <= report["frames"]
    assert limiter["lookahead_samples"] > 0
    assert report["master"]["frames"] == 288000
    assert abs(report["master"]["integrated_loudness_lufs"] + 8.0) <= 0.5
    assert report["master"]["true_peak_dbtp"] <= -1.0 + 1.0e-6


def test_ild_is_monotonic_with_azimuth() -> None:
    from theatre_binaural.project import Project, load_project

    project = load_project(EXAMPLE_PROJECT, validate_audio=True)
    az = np.deg2rad(np.linspace(-65.0, 65.0, 101))
    elevation = np.zeros_like(az)
    distance = np.full_like(az, 3.0)
    from theatre_binaural.binaural import _head_model

    _, _, _, _, ild, _ = _head_model(az, elevation, distance, project)
    assert np.all(np.diff(ild) > 0.0)

