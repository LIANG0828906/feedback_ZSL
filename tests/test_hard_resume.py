"""Subprocess-level hard-kill resume verification."""

import json
import os
import signal
import subprocess
import sys
import time

import soundfile as sf

from applause_master.cli import build_demo_project
from applause_master.project import load_project
from applause_master.synth import generate_project_audio


def _scaffold(base, duration=8.0, seed=4242):
    project_path = build_demo_project(base, seed=seed, duration_seconds=duration)
    with open(project_path) as handle:
        raw = json.load(handle)
    root = os.path.dirname(project_path)
    for mic in raw["microphones"]:
        mic["file"] = os.path.join(root, mic["file"])
    raw["project_path"] = project_path
    generate_project_audio(raw)
    return load_project(project_path), project_path


def _start_render(project_path, out_dir):
    env = dict(os.environ)
    return subprocess.Popen(
        [sys.executable, "-m", "applause_master.cli", "render",
         project_path, "--output", out_dir, "--no-interrupt"],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env,
    )


def test_sigkill_then_resume_is_byte_identical(tmp_path):
    base = str(tmp_path / "work")
    project, project_path = _scaffold(base)
    out_kill = str(tmp_path / "out_kill")
    proc = _start_render(project_path, out_kill)
    snapshot = os.path.join(out_kill, "snapshot.json")
    deadline = time.time() + 20
    while time.time() < deadline and not os.path.isfile(snapshot):
        time.sleep(0.02)
    # Wait until at least two blocks are committed, then hard-kill.
    deadline = time.time() + 20
    while time.time() < deadline:
        if os.path.isfile(snapshot):
            data = json.load(open(snapshot))
            if data["completed_blocks"] >= 2:
                break
        time.sleep(0.02)
    proc.send_signal(signal.SIGKILL)
    proc.wait()
    assert proc.returncode != 0
    committed = json.load(open(snapshot))["completed_blocks"]
    assert committed >= 2

    resumed = _start_render(project_path, out_kill)
    stdout, stderr = resumed.communicate(timeout=60)
    assert resumed.returncode == 0, stderr.decode()

    reference_out = str(tmp_path / "out_ref")
    ref = _start_render(project_path, reference_out)
    stdout, stderr = ref.communicate(timeout=60)
    assert ref.returncode == 0, stderr.decode()

    killed_master = open(os.path.join(out_kill, "master_final.wav"), "rb").read()
    ref_master = open(os.path.join(reference_out, "master_final.wav"), "rb").read()
    assert killed_master == ref_master

    report = json.load(open(os.path.join(out_kill, "report.json")))
    assert report["resume_count"] == 1
    data, sr = sf.read(os.path.join(out_kill, "master_final.wav"), always_2d=True)
    assert len(data) == report["total_samples"]


def test_sigint_completes_block_and_exits_zero(tmp_path, monkeypatch):
    from applause_master.cli import main
    import applause_master.render as render_module

    base = str(tmp_path / "work_sigint")
    project, project_path = _scaffold(base, duration=20.0)
    out_dir = str(tmp_path / "out_sigint")

    real_commit = render_module.commit_snapshot
    calls = {"n": 0}

    def interrupting_commit(path, payload):
        real_commit(path, payload)
        calls["n"] += 1
        if calls["n"] == 3:
            # Same cooperative path the CLI SIGINT handler flips.
            os.kill(os.getpid(), signal.SIGINT)

    monkeypatch.setattr(render_module, "commit_snapshot", interrupting_commit)
    exit_code = main(["render", project_path, "--output", out_dir])
    assert exit_code == 0

    resume_report = json.load(open(os.path.join(out_dir, "resume_report.json")))
    assert resume_report["completed_blocks"] >= 3
    assert resume_report["events"]

    # Resuming after the clean interrupt yields the same master as a fresh run.
    monkeypatch.undo()
    assert main(["render", project_path, "--output", out_dir]) == 0
    reference_out = str(tmp_path / "out_ref_sigint")
    assert main(["render", project_path, "--output", reference_out]) == 0
    assert open(os.path.join(out_dir, "master_final.wav"), "rb").read() == \
        open(os.path.join(reference_out, "master_final.wav"), "rb").read()
