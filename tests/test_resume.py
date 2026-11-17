from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest

from festival_master.manifest import load_manifest
from festival_master.render import run_render


ROOT = Path(__file__).resolve().parents[1]


def _start_render(
    manifest: Path,
    output: Path,
    report: Path,
    work: Path,
    *,
    block_samples: int,
    fresh: bool = False,
) -> subprocess.Popen:
    command = [
        sys.executable,
        "-m",
        "festival_master",
        "render",
        "--manifest",
        str(manifest),
        "--output",
        str(output),
        "--report",
        str(report),
        "--work-dir",
        str(work),
        "--block-samples",
        str(block_samples),
    ]
    if fresh:
        command.append("--fresh")
    return subprocess.Popen(
        command,
        cwd=ROOT,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )


def _wait_for_snapshot(work: Path, timeout: float = 20.0) -> dict:
    deadline = time.monotonic() + timeout
    snapshot = work / "snapshot.json"
    while time.monotonic() < deadline:
        if snapshot.exists():
            try:
                payload = json.loads(snapshot.read_text(encoding="utf-8"))
            except (OSError, json.JSONDecodeError):
                time.sleep(0.01)
                continue
            if payload.get("completed_blocks", 0) >= 1:
                return payload
        time.sleep(0.005)
    raise AssertionError("snapshot was not committed in time")


def test_graceful_sigint_commits_and_restarts(project: dict[str, Path], tmp_path: Path) -> None:
    manifest_path = project["manifest"]
    output = tmp_path / "resumed.wav"
    report = tmp_path / "resumed.json"
    work = tmp_path / "work"
    process = _start_render(
        manifest_path, output, report, work, block_samples=256, fresh=True
    )
    try:
        snapshot = _wait_for_snapshot(work)
        os.kill(process.pid, signal.SIGINT)
        stdout, stderr = process.communicate(timeout=30)
    finally:
        if process.poll() is None:
            process.kill()
            process.communicate()
    assert process.returncode == 0, (stdout, stderr)
    assert not output.exists()
    interrupted = json.loads(report.read_text(encoding="utf-8"))
    assert interrupted["status"] == "interrupted"
    assert interrupted["exit_code"] == 0
    assert "E_INTERRUPTED" in interrupted["business_codes"]
    assert interrupted["progress"]["completed_blocks"] >= snapshot["completed_blocks"]

    resumed = _start_render(
        manifest_path, output, report, work, block_samples=256, fresh=False
    )
    stdout, stderr = resumed.communicate(timeout=30)
    assert resumed.returncode == 0, (stdout, stderr)
    complete = json.loads(report.read_text(encoding="utf-8"))
    assert complete["status"] == "complete"
    assert complete["resume"]["resumed_from_snapshot"] is True

    fresh_output = tmp_path / "fresh.wav"
    fresh_report = tmp_path / "fresh.json"
    run_render(
        load_manifest(manifest_path),
        fresh_output,
        fresh_report,
        tmp_path / "fresh_work",
        block_samples=256,
        fresh=True,
    )
    assert output.read_bytes() == fresh_output.read_bytes()


def test_forced_kill_resumes_from_committed_snapshot(project: dict[str, Path], tmp_path: Path) -> None:
    manifest_path = project["manifest"]
    output = tmp_path / "killed.wav"
    report = tmp_path / "killed.json"
    work = tmp_path / "work"
    process = _start_render(
        manifest_path, output, report, work, block_samples=256, fresh=True
    )
    try:
        _wait_for_snapshot(work)
        os.kill(process.pid, signal.SIGKILL)
        stdout, stderr = process.communicate(timeout=10)
    finally:
        if process.poll() is None:
            process.kill()
            process.communicate()
    assert process.returncode != 0
    assert not output.exists()
    assert not report.exists()

    resumed = _start_render(
        manifest_path, output, report, work, block_samples=256, fresh=False
    )
    stdout, stderr = resumed.communicate(timeout=30)
    assert resumed.returncode == 0, (stdout, stderr)
    complete = json.loads(report.read_text(encoding="utf-8"))
    assert complete["status"] == "complete"
    assert complete["resume"]["resumed_from_snapshot"] is True

    fresh_output = tmp_path / "fresh.wav"
    fresh_report = tmp_path / "fresh.json"
    run_render(
        load_manifest(manifest_path),
        fresh_output,
        fresh_report,
        tmp_path / "fresh_work",
        block_samples=256,
        fresh=True,
    )
    assert output.read_bytes() == fresh_output.read_bytes()


def test_changed_manifest_discards_progress_with_business_code(
    project: dict[str, Path], tmp_path: Path
) -> None:
    manifest_path = project["manifest"]
    output = tmp_path / "mismatch.wav"
    report = tmp_path / "mismatch.json"
    work = tmp_path / "work"
    run_render(
        load_manifest(manifest_path),
        output,
        report,
        work,
        block_samples=512,
        fresh=True,
    )
    first_hash = output.read_bytes()

    data = json.loads(manifest_path.read_text(encoding="utf-8"))
    data["target_loudness_lufs"] = -15.0
    manifest_path.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    events: list[dict] = []
    run_render(
        load_manifest(manifest_path),
        output,
        report,
        work,
        block_samples=512,
        fresh=False,
        event=events.append,
    )
    complete = json.loads(report.read_text(encoding="utf-8"))
    assert complete["status"] == "complete"
    assert "E_RESUME_MISMATCH" in complete["business_codes"]
    assert complete["resume"]["mismatch_code"] == "E_RESUME_MISMATCH"
    assert complete["progress"]["resumed_blocks"] == 0
    assert any(event.get("code") == "E_RESUME_MISMATCH" for event in events)
    assert output.read_bytes() != first_hash
