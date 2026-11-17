"""Acceptance tests for the deterministic feedback-tracing pipeline."""

from __future__ import annotations

import json
import os
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf

from feedback_pipeline import errors
from feedback_pipeline.pipeline import run_pipeline
from feedback_pipeline.project import load_project
from feedback_pipeline.synth import generate_project, resonance_frequency

ROOT = Path(__file__).resolve().parent.parent
SR = 48000
DURATION = 6.0
BLOCK = 24000  # 0.5 s blocks -> 12 blocks for the default material


@pytest.fixture(scope="module")
def demo(tmp_path_factory):
    base = tmp_path_factory.mktemp("demo")
    project = generate_project(base / "proj", seed=7, song_segment=2, duration=DURATION)
    return project.parent


@pytest.fixture(scope="module")
def run1(demo, tmp_path_factory):
    out = tmp_path_factory.mktemp("run1") / "out"
    report = run_pipeline(demo / "project.json", out, block_size=BLOCK)
    return out, report


def _manifest(demo):
    return json.loads((demo / "ground_truth" / "manifest.json").read_text())


# ---------------------------------------------------------------- generation

def test_generation_is_byte_deterministic(tmp_path):
    p1 = generate_project(tmp_path / "a", seed=11, song_segment=3, duration=2.0)
    p2 = generate_project(tmp_path / "b", seed=11, song_segment=3, duration=2.0)
    files1 = sorted(p.relative_to(p1.parent) for p in p1.parent.rglob("*") if p.is_file())
    files2 = sorted(p.relative_to(p2.parent) for p in p2.parent.rglob("*") if p.is_file())
    assert [str(f) for f in files1] == [str(f) for f in files2]
    for f1, f2 in zip(files1, files2):
        assert (p1.parent / f1).read_bytes() == (p2.parent / f2).read_bytes(), f1


def test_generation_matches_manifest_and_segment_changes_fundamental(tmp_path):
    project = generate_project(tmp_path / "p", seed=3, song_segment=1, duration=2.0)
    meta = json.loads(project.read_text())
    assert len(meta["channels"]) == 7
    for ch in meta["channels"]:
        info = sf.info(str(project.parent / ch["file"]))
        assert info.samplerate == SR
        assert info.channels == ch["channels"]
        assert info.frames == int(2.0 * SR)
    assert resonance_frequency(0) != resonance_frequency(1)
    assert len({resonance_frequency(s) for s in range(4)}) == 4


# ---------------------------------------------------------------- validation

def _write_variant(demo, tmp_path, mutate, tag="proj"):
    proj_dir = tmp_path / tag
    shutil.copytree(demo, proj_dir, ignore=shutil.ignore_patterns("out*"))
    meta = json.loads((proj_dir / "project.json").read_text())
    mutate(meta, proj_dir)
    (proj_dir / "project.json").write_text(json.dumps(meta, indent=2, sort_keys=True))
    return proj_dir / "project.json"


def test_validation_missing_file(demo, tmp_path):
    path = _write_variant(demo, tmp_path, lambda m, d: os.remove(d / "audio" / "bass.wav"))
    with pytest.raises(errors.PipelineError) as ei:
        load_project(path)
    assert ei.value.code == "CHANNEL_FILE_MISSING"
    assert ei.value.channel == "bass"


def test_validation_sample_rate_mismatch(demo, tmp_path):
    def mutate(meta, d):
        data, _ = sf.read(str(d / "audio" / "bass.wav"), dtype="float64")
        sf.write(str(d / "audio" / "bass.wav"), data[:44100], 44100, subtype="PCM_24")
    path = _write_variant(demo, tmp_path, mutate)
    with pytest.raises(errors.PipelineError) as ei:
        load_project(path)
    assert ei.value.code == "SAMPLE_RATE_MISMATCH"
    assert ei.value.channel == "bass"


def test_validation_channel_count_mismatch(demo, tmp_path):
    def mutate(meta, d):
        data, _ = sf.read(str(d / "audio" / "keys.wav"), dtype="float64")
        sf.write(str(d / "audio" / "keys.wav"), np.stack([data, data], axis=1), SR, subtype="PCM_24")
    path = _write_variant(demo, tmp_path, mutate)
    with pytest.raises(errors.PipelineError) as ei:
        load_project(path)
    assert ei.value.code == "CHANNEL_COUNT_MISMATCH"
    assert ei.value.channel == "keys"


def test_validation_duplicate_name(demo, tmp_path):
    def mutate(meta, d):
        meta["channels"][1]["name"] = meta["channels"][0]["name"]
    path = _write_variant(demo, tmp_path, mutate)
    with pytest.raises(errors.PipelineError) as ei:
        load_project(path)
    assert ei.value.code == "DUPLICATE_CHANNEL_NAME"


def test_validation_role_missing_and_no_reference(demo, tmp_path):
    path = _write_variant(demo, tmp_path, lambda m, d: m["channels"][0].pop("role"), tag="p1")
    with pytest.raises(errors.PipelineError) as ei:
        load_project(path)
    assert ei.value.code == "CHANNEL_ROLE_MISSING"

    def no_ref(meta, d):
        for c in meta["channels"]:
            if c["role"] == "reference":
                c["role"] = "direct"
    path = _write_variant(demo, tmp_path, no_ref, tag="p2")
    with pytest.raises(errors.PipelineError) as ei:
        load_project(path)
    assert ei.value.code == "REFERENCE_MISSING"


# ---------------------------------------------------------------- detection

def test_detection_channel_set_and_frequency(demo, run1):
    _, report = run1
    truth = _manifest(demo)
    injected = set(truth["injected_channels"])
    flagged = set(report["detection"]["flagged_channels"])
    assert flagged == injected
    false_positives = len(flagged - injected)
    project = json.loads((demo / "project.json").read_text())
    assert false_positives <= project["allowed_false_positives"]
    for name in injected:
        est = report["detection"]["channels"][name]["fundamental_hz"]
        assert abs(est - truth["fundamental_hz"]) <= 1.0
        assert report["detection"]["channels"][name]["confidence"] > 0.5
        assert len(report["detection"]["channels"][name]["segments_samples"]) >= 1


# ---------------------------------------------------------------- removal

def test_removal_quality(demo, run1):
    out, report = run1
    truth = _manifest(demo)
    project = json.loads((demo / "project.json").read_text())
    f0 = truth["fundamental_hz"]
    from feedback_pipeline.dsp import spectrum

    def peak_db(x):
        freqs, mag = spectrum(x, SR)
        return 20.0 * np.log10(mag[np.argmin(np.abs(freqs - f0))])

    for name in report["detection"]["flagged_channels"]:
        rem = report["removal"][name]
        assert rem["attenuation_db"] <= -project["min_attenuation_db"]
        assert not rem["residual_feedback_detected"]
        assert rem["modified_ranges_count"] == len(rem["segments_samples"])
        assert rem["max_modification"] > 0.0
        processed, sr = sf.read(str(out / "work" / "processed" / f"{name}.wav"), dtype="float64")
        clean, _ = sf.read(str(demo / "ground_truth" / f"{name}.wav"), dtype="float64")
        original, _ = sf.read(str(demo / "audio" / f"{name}.wav"), dtype="float64")
        assert sr == SR and len(processed) == len(original)
        # Residual peak at the hum frequency drops to the clean floor.
        assert peak_db(processed) <= peak_db(clean) + 3.0
        # Samples outside the declared segments are untouched.
        mask = np.zeros(len(original), dtype=bool)
        for s, e in rem["segments_samples"]:
            mask[s:e] = True
        assert np.array_equal(processed[~mask], original[~mask])
        info = sf.info(str(out / "work" / "processed" / f"{name}.wav"))
        assert info.subtype == "PCM_24"


def test_unflagged_channels_bit_identical(demo, run1):
    out, report = run1
    flagged = set(report["detection"]["flagged_channels"])
    for ch in json.loads((demo / "project.json").read_text())["channels"]:
        if ch["role"] == "reference" or ch["name"] in flagged:
            continue
        original, _ = sf.read(str(demo / "audio" / f"{ch['name']}.wav"), dtype="float64")
        processed, _ = sf.read(str(out / "work" / "processed" / f"{ch['name']}.wav"), dtype="float64")
        assert np.array_equal(original, processed)


# ---------------------------------------------------------------- bus

def test_bus_loudness_true_peak_and_length(demo, run1):
    out, report = run1
    project = json.loads((demo / "project.json").read_text())
    bus = report["bus"]
    assert abs(bus["integrated_loudness_lufs"] - project["loudness_target_lufs"]) <= 0.5
    assert bus["true_peak_dbtp"] <= project["true_peak_ceiling_dbtp"]
    info = sf.info(str(out / "bus.wav"))
    longest = max(sf.info(str(demo / "audio" / f"{c['name']}.wav")).frames
                  for c in project["channels"] if c["role"] != "reference")
    assert info.frames == longest
    assert info.channels == 2
    assert abs(sum(bus["contributions"].values()) - 1.0) < 1e-6


def test_report_deterministic(demo, tmp_path):
    out_a = tmp_path / "a"
    out_b = tmp_path / "b"
    run_pipeline(demo / "project.json", out_a, block_size=BLOCK)
    run_pipeline(demo / "project.json", out_b, block_size=BLOCK)
    ra = json.loads((out_a / "report.json").read_text())
    rb = json.loads((out_b / "report.json").read_text())
    for r in (ra, rb):
        r.pop("elapsed_seconds")
        r.pop("project")
        r["bus"].pop("file")
    assert ra == rb
    assert (out_a / "bus.wav").read_bytes() == (out_b / "bus.wav").read_bytes()


# ------------------------------------------------------------- resume / kill

def _cli_env():
    env = dict(os.environ)
    env["PYTHONPATH"] = str(ROOT) + os.pathsep + env.get("PYTHONPATH", "")
    return env


def _wait_blocks(out_dir: Path, n: int, timeout: float = 60.0) -> dict:
    snap = out_dir / "work" / "snapshot.2.json"
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            data = json.loads(snap.read_text())
            if data.get("completed_blocks", 0) >= n:
                return data
        except (OSError, json.JSONDecodeError):
            pass
        time.sleep(0.05)
    raise TimeoutError(f"snapshot never reached {n} blocks")


def _run_cli(project: Path, out: Path, delay_ms: int = 0) -> subprocess.Popen:
    cmd = [sys.executable, "-m", "feedback_pipeline", "run",
           "--project", str(project), "--out", str(out),
           "--block-size", str(BLOCK), "--block-delay-ms", str(delay_ms)]
    return subprocess.Popen(cmd, cwd=ROOT, env=_cli_env(),
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def test_resume_after_forced_kill_is_byte_identical(demo, tmp_path):
    one_shot = tmp_path / "one_shot"
    run_pipeline(demo / "project.json", one_shot, block_size=BLOCK)

    resumed = tmp_path / "resumed"
    proc = _run_cli(demo / "project.json", resumed, delay_ms=250)
    _wait_blocks(resumed, 3)
    proc.kill()
    proc.wait()

    proc2 = _run_cli(demo / "project.json", resumed)
    _, stderr = proc2.communicate(timeout=120)
    assert proc2.returncode == 0, stderr.decode()
    assert (one_shot / "bus.wav").read_bytes() == (resumed / "bus.wav").read_bytes()


def test_graceful_interrupt_commits_and_exits_zero(demo, tmp_path):
    out = tmp_path / "interrupted"
    proc = _run_cli(demo / "project.json", out, delay_ms=250)
    _wait_blocks(out, 2)
    proc.send_signal(signal.SIGINT)
    proc.communicate(timeout=60)
    assert proc.returncode == 0
    recovery = json.loads((out / "recovery_report.json").read_text())
    assert recovery["status"] == "interrupted"
    assert recovery["completed_blocks"] >= 2

    proc2 = _run_cli(demo / "project.json", out)
    proc2.communicate(timeout=120)
    assert proc2.returncode == 0
    one_shot = tmp_path / "one_shot"
    run_pipeline(demo / "project.json", one_shot, block_size=BLOCK)
    assert (one_shot / "bus.wav").read_bytes() == (out / "bus.wav").read_bytes()


def test_corrupt_snapshot_falls_back(demo, tmp_path):
    out = tmp_path / "corrupt"
    proc = _run_cli(demo / "project.json", out, delay_ms=250)
    _wait_blocks(out, 4)
    proc.kill()
    proc.wait()

    snap_new = out / "work" / "snapshot.2.json"
    snap_new.write_text("{ not valid json !!!")
    proc2 = _run_cli(demo / "project.json", out)
    proc2.communicate(timeout=120)
    assert proc2.returncode == 0

    one_shot = tmp_path / "one_shot"
    run_pipeline(demo / "project.json", one_shot, block_size=BLOCK)
    assert (one_shot / "bus.wav").read_bytes() == (out / "bus.wav").read_bytes()


def test_corrupt_all_snapshots_restarts_clean(demo, tmp_path):
    out = tmp_path / "corrupt_all"
    proc = _run_cli(demo / "project.json", out, delay_ms=250)
    _wait_blocks(out, 3)
    proc.kill()
    proc.wait()
    for name in ("snapshot.1.json", "snapshot.2.json"):
        p = out / "work" / name
        if p.exists():
            p.write_text("garbage")
    proc2 = _run_cli(demo / "project.json", out)
    proc2.communicate(timeout=120)
    assert proc2.returncode == 0
    report = json.loads((out / "report.json").read_text())
    assert report["status"] == "completed"


def test_resume_mismatch_discards_old_progress(demo, tmp_path):
    out = tmp_path / "mismatch"
    proc = _run_cli(demo / "project.json", out, delay_ms=250)
    _wait_blocks(out, 3)
    proc.kill()
    proc.wait()

    proj_dir = tmp_path / "proj_changed"
    shutil.copytree(demo, proj_dir)
    meta = json.loads((proj_dir / "project.json").read_text())
    meta["loudness_target_lufs"] = -14.0
    (proj_dir / "project.json").write_text(json.dumps(meta, indent=2, sort_keys=True))

    proc2 = _run_cli(proj_dir / "project.json", out)
    proc2.communicate(timeout=120)
    assert proc2.returncode == 0
    report = json.loads((out / "report.json").read_text())
    codes = [e["code"] for e in report["resume_events"]]
    assert errors.E_RESUME_MISMATCH in codes
    assert report["bus"]["integrated_loudness_lufs"] == pytest.approx(-14.0, abs=0.5)
