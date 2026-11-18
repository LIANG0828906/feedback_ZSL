from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path

import numpy as np
import pytest

from scene_audio.errors import BusinessError
from scene_audio.render import render_scene
from scene_audio.snapshot import load_snapshot, save_snapshot


def _snapshot_blocks(path: Path) -> int | None:
    if not path.exists():
        return None
    try:
        metadata, _ = load_snapshot(path)
        return int(metadata["completed_blocks"])
    except Exception:
        return None


def test_ctrl_c_commits_and_resume_is_byte_identical(
    demo_scene: Path,
    full_render: tuple[Path, dict],
    tmp_path: Path,
) -> None:
    reference_out, _ = full_render
    out = tmp_path / "resumed"
    process = subprocess.Popen(
        [
            sys.executable,
            "-m",
            "scene_audio",
            "render",
            "--scene",
            str(demo_scene),
            "--out",
            str(out),
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    snapshot_path = out / "render_snapshot.npz"
    deadline = time.time() + 30.0
    seen_blocks: int | None = None
    while time.time() < deadline:
        seen_blocks = _snapshot_blocks(snapshot_path)
        if seen_blocks is not None and seen_blocks >= 2:
            break
        if process.poll() is not None:
            break
        time.sleep(0.01)
    assert seen_blocks is not None and seen_blocks >= 2
    assert process.poll() is None
    process.send_signal(signal.SIGINT)
    stdout, stderr = process.communicate(timeout=30)
    assert process.returncode == 0, (stdout, stderr)
    assert not (out / "master.wav").exists()
    recovery = json.loads(
        (out / "recovery_report.json").read_text(encoding="utf-8")
    )
    assert recovery["status"] == "interrupted"
    assert recovery["report_kind"] == "recovery"
    assert _snapshot_blocks(snapshot_path) is not None

    resumed = subprocess.run(
        [
            sys.executable,
            "-m",
            "scene_audio",
            "render",
            "--scene",
            str(demo_scene),
            "--out",
            str(out),
            "--verify-reference",
            str(reference_out / "master.wav"),
        ],
        capture_output=True,
        text=True,
        timeout=60,
    )
    assert resumed.returncode == 0, (resumed.stdout, resumed.stderr)
    assert (out / "master.wav").read_bytes() == (
        reference_out / "master.wav"
    ).read_bytes()
    report = json.loads((out / "report.json").read_text(encoding="utf-8"))
    assert report["recovery_count"] == 1
    assert report["status"] == "complete"


def test_snapshot_scene_mismatch_is_rejected_and_discarded(
    demo_scene: Path,
    tmp_path: Path,
) -> None:
    out = tmp_path / "interrupted"
    calls = 0

    def stop_after_first_block() -> bool:
        nonlocal calls
        calls += 1
        return calls >= 2

    result = render_scene(
        demo_scene,
        out,
        interrupt_requested=stop_after_first_block,
    )
    assert result.exit_code == 0
    assert result.interrupted is True
    snapshot_path = out / "render_snapshot.npz"
    assert snapshot_path.exists()

    raw = json.loads(demo_scene.read_text(encoding="utf-8"))
    raw["reverb"]["regions"][0]["rt60"] = 0.81
    changed_scene_dir = tmp_path / "changed_scene"
    changed_scene_dir.mkdir()
    for track in raw["tracks"]:
        source = demo_scene.parent / track["path"]
        target = changed_scene_dir / track["path"]
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(source.read_bytes())
    changed_scene = changed_scene_dir / "scene.json"
    changed_scene.write_text(json.dumps(raw), encoding="utf-8")

    with pytest.raises(BusinessError) as captured:
        render_scene(changed_scene, out)
    assert captured.value.code == "E_RESUME_MISMATCH"
    assert not snapshot_path.exists()
    assert not (out / "master.wav").exists()


def test_snapshot_reverb_interval_mismatch_is_rejected(
    demo_scene: Path,
    tmp_path: Path,
) -> None:
    out = tmp_path / "tampered"
    calls = 0

    def stop_after_first_block() -> bool:
        nonlocal calls
        calls += 1
        return calls >= 2

    result = render_scene(
        demo_scene,
        out,
        interrupt_requested=stop_after_first_block,
    )
    assert result.interrupted is True
    snapshot_path = out / "render_snapshot.npz"
    metadata, arrays = load_snapshot(snapshot_path)
    metadata["current_reverb"]["rt60"] += 0.01
    save_snapshot(snapshot_path, metadata, arrays)

    with pytest.raises(BusinessError) as captured:
        render_scene(demo_scene, out)
    assert captured.value.code == "E_RESUME_MISMATCH"
    assert "reverb" in captured.value.message
    assert not snapshot_path.exists()


def test_forced_termination_recovers_from_last_committed_snapshot(
    demo_scene: Path,
    full_render: tuple[Path, dict],
    tmp_path: Path,
) -> None:
    reference_out, _ = full_render
    out = tmp_path / "forced"
    process = subprocess.Popen(
        [
            sys.executable,
            "-m",
            "scene_audio",
            "render",
            "--scene",
            str(demo_scene),
            "--out",
            str(out),
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    snapshot_path = out / "render_snapshot.npz"
    deadline = time.time() + 30.0
    seen_blocks: int | None = None
    while time.time() < deadline:
        seen_blocks = _snapshot_blocks(snapshot_path)
        if seen_blocks is not None and seen_blocks >= 2:
            break
        if process.poll() is not None:
            break
        time.sleep(0.01)
    assert seen_blocks is not None and seen_blocks >= 2
    process.kill()
    process.communicate(timeout=30)
    assert process.returncode != 0
    assert not (out / "master.wav").exists()
    assert snapshot_path.exists()

    resumed = subprocess.run(
        [
            sys.executable,
            "-m",
            "scene_audio",
            "render",
            "--scene",
            str(demo_scene),
            "--out",
            str(out),
            "--verify-reference",
            str(reference_out / "master.wav"),
        ],
        capture_output=True,
        text=True,
        timeout=60,
    )
    assert resumed.returncode == 0, (resumed.stdout, resumed.stderr)
    assert (out / "master.wav").read_bytes() == (
        reference_out / "master.wav"
    ).read_bytes()
    report = json.loads((out / "report.json").read_text(encoding="utf-8"))
    assert report["recovery_count"] == 1
