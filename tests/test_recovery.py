from __future__ import annotations

import hashlib
import json
import os
import signal
import subprocess
import time
from pathlib import Path

import pytest

from conftest import PROJECT_ROOT, python_executable
from orchestral_mastering.pipeline import RenderConfig, render_project


def _sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _render_command(
    manifest: Path,
    output: Path,
    report: Path,
    snapshot: Path,
    *,
    delay_ms: float = 0.0,
) -> list[str]:
    command = [
        python_executable(),
        "-m",
        "orchestral_mastering",
        "render",
        "--manifest",
        str(manifest),
        "--output",
        str(output),
        "--report",
        str(report),
        "--snapshot",
        str(snapshot),
        "--block-frames",
        "32768",
    ]
    if delay_ms:
        command.extend(["--block-delay-ms", str(delay_ms)])
    return command


def _wait_for_blocks(snapshot: Path, count: int, process: subprocess.Popen, timeout: float = 20.0) -> int:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise AssertionError(f"render exited before snapshot was observed: {process.returncode}")
        try:
            state = json.loads(snapshot.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            time.sleep(0.03)
            continue
        completed = int(state.get("completed_blocks", 0))
        if completed >= count:
            return completed
        time.sleep(0.03)
    raise AssertionError("timed out waiting for committed blocks")


@pytest.mark.skipif(os.name == "nt", reason="SIGKILL byte-for-byte recovery test uses POSIX signals")
def test_forced_termination_resume_matches_one_shot(demo_root: Path, tmp_path: Path) -> None:
    manifest = demo_root / "manifest.json"
    reference = tmp_path / "reference.wav"
    render_project(
        RenderConfig(
            manifest_path=manifest,
            output_path=reference,
            report_path=tmp_path / "reference.json",
            snapshot_path=tmp_path / "reference_snapshot.json",
            block_frames=32768,
        )
    )
    resumed_output = tmp_path / "resumed.wav"
    resumed_report = tmp_path / "resumed.json"
    snapshot = tmp_path / "snapshot.json"
    process = subprocess.Popen(
        _render_command(
            manifest,
            resumed_output,
            resumed_report,
            snapshot,
            delay_ms=90,
        ),
        cwd=PROJECT_ROOT,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    completed = _wait_for_blocks(snapshot, 2, process)
    os.kill(process.pid, signal.SIGKILL)
    process.wait(timeout=10)
    assert process.returncode == -signal.SIGKILL
    persisted = json.loads(snapshot.read_text(encoding="utf-8"))
    assert persisted["completed_blocks"] == completed
    assert not resumed_report.exists()

    resumed = subprocess.run(
        _render_command(manifest, resumed_output, resumed_report, snapshot),
        cwd=PROJECT_ROOT,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert resumed.returncode == 0, resumed.stderr
    final_report = json.loads(resumed_report.read_text(encoding="utf-8"))
    assert final_report["status"] == "complete"
    assert final_report["snapshot"]["commit_count"] == final_report["snapshot"]["total_blocks"]
    assert _sha(resumed_output) == _sha(reference)


@pytest.mark.skipif(os.name == "nt", reason="SIGINT child-process test uses POSIX signals")
def test_graceful_interrupt_commits_and_reports(demo_root: Path, tmp_path: Path) -> None:
    manifest = demo_root / "manifest.json"
    reference = tmp_path / "reference.wav"
    render_project(
        RenderConfig(
            manifest_path=manifest,
            output_path=reference,
            report_path=tmp_path / "reference.json",
            snapshot_path=tmp_path / "reference_snapshot.json",
            block_frames=32768,
        )
    )
    output = tmp_path / "interrupted.wav"
    report = tmp_path / "interrupted.json"
    snapshot = tmp_path / "snapshot.json"
    process = subprocess.Popen(
        _render_command(manifest, output, report, snapshot, delay_ms=120),
        cwd=PROJECT_ROOT,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    completed = _wait_for_blocks(snapshot, 2, process)
    os.kill(process.pid, signal.SIGINT)
    stdout, stderr = process.communicate(timeout=20)
    assert process.returncode == 0, stderr
    state = json.loads(snapshot.read_text(encoding="utf-8"))
    recovery = json.loads(report.read_text(encoding="utf-8"))
    assert state["status"] == "interrupted"
    assert recovery["status"] == "interrupted"
    assert state["commit_count"] == completed
    assert recovery["snapshot"]["commits"] == completed
    assert not output.exists()
    assert "interrupted" in stdout

    resumed = subprocess.run(
        _render_command(manifest, output, report, snapshot),
        cwd=PROJECT_ROOT,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert resumed.returncode == 0, resumed.stderr
    assert json.loads(report.read_text(encoding="utf-8"))["status"] == "complete"
    assert _sha(output) == _sha(reference)


def test_resume_mismatch_discards_progress(demo_root: Path, tmp_path: Path) -> None:
    manifest = json.loads((demo_root / "manifest.json").read_text(encoding="utf-8"))
    base = demo_root.resolve()
    for track in manifest["tracks"]:
        track["path"] = str((base / track["path"]).resolve())
        track["reference_path"] = str((base / track["reference_path"]).resolve())
    manifest_path = tmp_path / "manifest.json"
    manifest_path.write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    output = tmp_path / "master.wav"
    report = tmp_path / "report.json"
    snapshot = tmp_path / "snapshot.json"
    render_project(
        RenderConfig(
            manifest_path=manifest_path,
            output_path=output,
            report_path=report,
            snapshot_path=snapshot,
            block_frames=32768,
        )
    )
    assert snapshot.exists()
    manifest["master"]["target_loudness_lufs"] = -19.0
    manifest_path.write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    completed = subprocess.run(
        _render_command(manifest_path, output, report, snapshot),
        cwd=PROJECT_ROOT,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert completed.returncode == 3
    payload = json.loads(completed.stderr)
    assert payload["errors"][0]["code"] == "E_RESUME_MISMATCH"
    assert not snapshot.exists()


def test_tampered_determined_parameters_trigger_resume_mismatch(
    demo_root: Path, tmp_path: Path
) -> None:
    manifest = demo_root / "manifest.json"
    output = tmp_path / "master.wav"
    report = tmp_path / "report.json"
    snapshot = tmp_path / "snapshot.json"
    render_project(
        RenderConfig(
            manifest_path=manifest,
            output_path=output,
            report_path=report,
            snapshot_path=snapshot,
            block_frames=32768,
        )
    )
    state = json.loads(snapshot.read_text(encoding="utf-8"))
    state["alignment_params"]["tracks"][0]["shift_samples"] += 1
    snapshot.write_text(
        json.dumps(state, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    completed = subprocess.run(
        _render_command(manifest, output, report, snapshot),
        cwd=PROJECT_ROOT,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert completed.returncode == 3
    assert json.loads(completed.stderr)["errors"][0]["code"] == "E_RESUME_MISMATCH"
    assert not snapshot.exists()
