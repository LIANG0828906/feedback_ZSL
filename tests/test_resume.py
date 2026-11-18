from __future__ import annotations

import json
import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest

from conftest import (
    load_json,
    start_cli,
    wait_for_blocks,
    write_manifest_variant,
)


def _render_args(
    manifest: Path,
    output: Path,
    report: Path,
    snapshot: Path,
    *,
    pause_ms: float = 0.0,
    fresh: bool = True,
) -> list[str]:
    args = [
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
        "12000",
        "--block-pause-ms",
        str(pause_ms),
    ]
    if fresh:
        args.append("--fresh")
    return args


def _kill_after_blocks(process: subprocess.Popen[str], snapshot: Path, blocks: int) -> dict:
    state = wait_for_blocks(snapshot, blocks)
    process.kill()
    process.wait(timeout=20)
    return state


def test_strong_kill_resume_is_byte_identical(
    manifest_path: Path,
    tmp_path: Path,
    reference_render: dict[str, Path],
) -> None:
    output = tmp_path / "resumed.wav"
    report = tmp_path / "resumed.json"
    snapshot = tmp_path / "resumed.snapshot"
    process = start_cli(
        _render_args(manifest_path, output, report, snapshot, pause_ms=120)
    )
    state = _kill_after_blocks(process, snapshot, 3)
    assert process.returncode != 0
    assert state["completed_blocks"] < state["total_blocks"]
    assert not report.exists()

    resumed = subprocess.run(
        [
            sys.executable,
            "-m",
            "choir_master",
            *_render_args(manifest_path, output, report, snapshot, fresh=False),
        ],
        cwd=Path(__file__).resolve().parents[1],
        text=True,
        capture_output=True,
        timeout=90,
    )
    assert resumed.returncode == 0, resumed.stderr
    assert output.read_bytes() == reference_render["output"].read_bytes()
    document = load_json(report)
    assert document["status"] == "complete"
    assert document["recovery_count"] == 1
    assert document["output_sha256"] == load_json(reference_render["report"])["output_sha256"]


def test_sigint_commits_block_and_writes_recovery_report(
    manifest_path: Path,
    tmp_path: Path,
    reference_render: dict[str, Path],
) -> None:
    output = tmp_path / "graceful.wav"
    report = tmp_path / "graceful.json"
    snapshot = tmp_path / "graceful.snapshot"
    process = start_cli(
        _render_args(manifest_path, output, report, snapshot, pause_ms=160)
    )
    wait_for_blocks(snapshot, 3)
    process.send_signal(signal.SIGINT)
    stdout, stderr = process.communicate(timeout=60)
    assert process.returncode == 0, (stdout, stderr)
    assert report.exists()
    recovery = report.with_name(report.stem + ".recovery" + report.suffix)
    assert recovery.exists()
    partial = load_json(report)
    assert partial["status"] == "interrupted"
    assert partial["completed_blocks"] < partial["total_blocks"]
    assert not output.exists()
    state = load_json(snapshot / "state.json")
    assert state["completed_blocks"] == partial["completed_blocks"]

    resumed = subprocess.run(
        [
            sys.executable,
            "-m",
            "choir_master",
            *_render_args(manifest_path, output, report, snapshot, fresh=False),
        ],
        cwd=Path(__file__).resolve().parents[1],
        text=True,
        capture_output=True,
        timeout=90,
    )
    assert resumed.returncode == 0, resumed.stderr
    assert output.read_bytes() == reference_render["output"].read_bytes()
    final = load_json(report)
    assert final["status"] == "complete"
    assert final["recovery_count"] == 1
    assert not recovery.exists()


def test_resume_manifest_mismatch_discards_progress(
    manifest_path: Path,
    tmp_path: Path,
) -> None:
    output = tmp_path / "mismatch.wav"
    report = tmp_path / "mismatch.json"
    snapshot = tmp_path / "mismatch.snapshot"
    process = start_cli(
        _render_args(manifest_path, output, report, snapshot, pause_ms=120)
    )
    _kill_after_blocks(process, snapshot, 2)
    assert snapshot.exists()

    changed = write_manifest_variant(
        manifest_path,
        manifest_path.parent / "project-mismatch.json",
        lambda document: document.__setitem__("target_loudness_lufs", -21.0),
    )
    result = subprocess.run(
        [
            sys.executable,
            "-m",
            "choir_master",
            *_render_args(changed, output, report, snapshot, fresh=False),
        ],
        cwd=Path(__file__).resolve().parents[1],
        text=True,
        capture_output=True,
        timeout=90,
    )
    assert result.returncode == 3
    payload = json.loads(result.stderr)
    assert payload["errors"][0]["code"] == "E_RESUME_MISMATCH"
    assert payload["errors"][0]["mic"] == "<master>"
    assert not snapshot.exists()
    assert not output.exists()


def test_resume_analysis_mismatch_discards_progress(
    manifest_path: Path,
    tmp_path: Path,
) -> None:
    output = tmp_path / "analysis-mismatch.wav"
    report = tmp_path / "analysis-mismatch.json"
    snapshot = tmp_path / "analysis-mismatch.snapshot"
    process = start_cli(
        _render_args(manifest_path, output, report, snapshot, pause_ms=120)
    )
    _kill_after_blocks(process, snapshot, 2)
    state_path = snapshot / "state.json"
    state = load_json(state_path)
    state["analysis"]["voice_intervals"][2]["intervals"][0]["start_s"] += 0.001
    with open(state_path, "w", encoding="utf-8") as handle:
        json.dump(state, handle, ensure_ascii=False, indent=2)

    result = subprocess.run(
        [
            sys.executable,
            "-m",
            "choir_master",
            *_render_args(manifest_path, output, report, snapshot, fresh=False),
        ],
        cwd=Path(__file__).resolve().parents[1],
        text=True,
        capture_output=True,
        timeout=90,
    )
    assert result.returncode == 3
    payload = json.loads(result.stderr)
    assert payload["errors"][0]["code"] == "E_RESUME_MISMATCH"
    assert not snapshot.exists()
    assert not report.exists()

