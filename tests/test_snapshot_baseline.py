from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

from cover_replay.engine import ReplayEngine
from cover_replay.errors import E_BASELINE_VERSION, E_RECOVERY_INVALID, ReplayError
from cover_replay.generator import generate_sample
from cover_replay.loader import load_scenario


def test_resume_produces_identical_trace_and_no_duplicate_shots(tmp_path):
    sample_dir = tmp_path / "sample"
    generate_sample(
        sample_dir,
        seed=123,
        cover_count=4,
        player_count=2,
        revoke_density=0.2,
        steps=5,
        clean=True,
    )
    scenario = load_scenario(sample_dir)
    full_engine = ReplayEngine(
        scenario,
        tmp_path / "full",
        clean=True,
        compare_baseline=False,
    )
    full_result = full_engine.run()

    interrupted = ReplayEngine(
        scenario,
        tmp_path / "resumed",
        clean=True,
        compare_baseline=False,
    )
    interrupted.run(max_steps=2)
    resumed = ReplayEngine(
        scenario,
        tmp_path / "resumed",
        resume=True,
        compare_baseline=False,
    )
    resumed_result = resumed.run()

    assert resumed_result.status == "completed"
    assert (
        (tmp_path / "full" / "trajectory.jsonl").read_bytes()
        == (tmp_path / "resumed" / "trajectory.jsonl").read_bytes()
    )
    assert len(resumed.state.settled_shot_keys) == len(
        set(resumed.state.settled_shot_keys)
    )
    assert resumed.state.step == scenario.config.physical_steps - 1
    assert len(full_result.trace_frames) == len(resumed_result.trace_frames) + 2


def test_tampered_snapshot_reports_first_break_step(tmp_path):
    sample_dir = tmp_path / "sample"
    generate_sample(
        sample_dir,
        seed=9,
        cover_count=3,
        player_count=2,
        revoke_density=0.0,
        steps=3,
        clean=True,
    )
    scenario = load_scenario(sample_dir)
    output = tmp_path / "run"
    engine = ReplayEngine(scenario, output, clean=True, compare_baseline=False)
    engine.run(max_steps=2)
    snapshot_path = output / "snapshots" / "step_000001.json"
    document = json.loads(snapshot_path.read_text(encoding="utf-8"))
    document["state"]["covers"][0]["damage_units"] += 1
    snapshot_path.write_text(
        json.dumps(document, sort_keys=True, separators=(",", ":")),
        encoding="utf-8",
    )
    with pytest.raises(ReplayError) as caught:
        ReplayEngine(scenario, output, resume=True, compare_baseline=False)
    error = caught.value
    assert error.code == E_RECOVERY_INVALID
    assert error.details["first_break_step"] == 1
    assert error.field_path.endswith("state_hash")


def test_missing_snapshot_frame_field_is_recovery_error(tmp_path):
    sample_dir = tmp_path / "sample"
    generate_sample(
        sample_dir,
        seed=10,
        cover_count=2,
        player_count=1,
        revoke_density=0.0,
        steps=2,
        clean=True,
    )
    scenario = load_scenario(sample_dir)
    output = tmp_path / "run"
    engine = ReplayEngine(scenario, output, clean=True, compare_baseline=False)
    engine.run(max_steps=1)
    snapshot_path = output / "snapshots" / "step_000000.json"
    document = json.loads(snapshot_path.read_text(encoding="utf-8"))
    del document["frame"]["buffer_watermark_bytes"]
    snapshot_path.write_text(
        json.dumps(document, sort_keys=True, separators=(",", ":")),
        encoding="utf-8",
    )
    with pytest.raises(ReplayError) as caught:
        ReplayEngine(scenario, output, resume=True, compare_baseline=False)
    assert caught.value.code == E_RECOVERY_INVALID
    assert caught.value.field_path.endswith("frame.buffer_watermark_bytes")


def test_missing_snapshot_state_field_is_recovery_error(tmp_path):
    sample_dir = tmp_path / "sample"
    generate_sample(
        sample_dir,
        seed=11,
        cover_count=2,
        player_count=1,
        revoke_density=0.0,
        steps=2,
        clean=True,
    )
    scenario = load_scenario(sample_dir)
    output = tmp_path / "run"
    engine = ReplayEngine(scenario, output, clean=True, compare_baseline=False)
    engine.run(max_steps=1)
    snapshot_path = output / "snapshots" / "step_000000.json"
    document = json.loads(snapshot_path.read_text(encoding="utf-8"))
    del document["state"]["buffer_watermark_bytes"]
    snapshot_path.write_text(
        json.dumps(document, sort_keys=True, separators=(",", ":")),
        encoding="utf-8",
    )
    with pytest.raises(ReplayError) as caught:
        ReplayEngine(scenario, output, resume=True, compare_baseline=False)
    assert caught.value.code == E_RECOVERY_INVALID
    assert caught.value.field_path.endswith("state.buffer_watermark_bytes")


def test_baseline_first_difference_has_field_and_context(tmp_path):
    sample_dir = tmp_path / "sample"
    generate_sample(
        sample_dir,
        seed=17,
        cover_count=3,
        player_count=2,
        revoke_density=0.1,
        steps=4,
        clean=True,
    )
    baseline_path = sample_dir / "baseline.json"
    baseline = json.loads(baseline_path.read_text(encoding="utf-8"))
    baseline["frames"][2]["budget_usage"]["peak_memory_bytes"] = 999999
    baseline_path.write_text(
        json.dumps(baseline, ensure_ascii=False, sort_keys=True),
        encoding="utf-8",
    )
    scenario = load_scenario(sample_dir)
    engine = ReplayEngine(
        scenario,
        tmp_path / "run",
        clean=True,
        compare_baseline=True,
    )
    result = engine.run()
    assert result.status == "baseline_mismatch"
    difference = result.baseline_diff
    assert difference["step"] == 2
    assert difference["field_path"] == "frames[2].budget_usage.peak_memory_bytes"
    assert difference["expected"] == 999999
    assert difference["actual"] != 999999
    assert "effective_shot_client_sequence" in difference
    assert "buffer_watermark_bytes" in difference
    report = json.loads((tmp_path / "run" / "baseline_diff.json").read_text())
    assert report["first_difference"]["step"] == 2


def test_baseline_version_mismatch_raises_dedicated_code(tmp_path):
    sample_dir = tmp_path / "sample"
    generate_sample(
        sample_dir,
        seed=1,
        cover_count=2,
        player_count=1,
        revoke_density=0.0,
        steps=2,
        clean=True,
    )
    baseline_path = sample_dir / "baseline.json"
    baseline = json.loads(baseline_path.read_text(encoding="utf-8"))
    baseline["damage_model_version"] = "different-model"
    baseline_path.write_text(json.dumps(baseline), encoding="utf-8")
    scenario = load_scenario(sample_dir)
    with pytest.raises(ReplayError) as caught:
        ReplayEngine(
            scenario,
            tmp_path / "run",
            clean=True,
            compare_baseline=True,
        )
    assert caught.value.code == E_BASELINE_VERSION
    assert caught.value.field_path == "baseline.damage_model_version"


def test_generator_is_byte_deterministic(tmp_path):
    parameters = {
        "seed": 88,
        "cover_count": 4,
        "player_count": 2,
        "revoke_density": 0.3,
        "steps": 4,
        "clean": True,
    }
    first = tmp_path / "first"
    second = tmp_path / "second"
    generate_sample(first, **parameters)
    generate_sample(second, **parameters)
    for name in (
        "config.json",
        "covers.json",
        "match.json",
        "baseline.json",
        "trajectory.jsonl",
        "manifest.json",
    ):
        assert (first / name).read_bytes() == (second / name).read_bytes()


def test_forced_process_termination_resumes_from_disk_snapshot(tmp_path):
    sample_dir = tmp_path / "sample"
    generate_sample(
        sample_dir,
        seed=71,
        cover_count=3,
        player_count=2,
        revoke_density=0.2,
        steps=3,
        clean=True,
    )
    project_root = Path(__file__).resolve().parents[1]
    environment = dict(os.environ)
    environment["PYTHONPATH"] = str(project_root / "src")
    crash_output = tmp_path / "crash"
    crash = subprocess.run(
        [
            sys.executable,
            "-m",
            "cover_replay",
            "replay",
            "--input",
            str(sample_dir),
            "--output",
            str(crash_output),
            "--clean",
            "--no-baseline",
            "--crash-after-step",
            "1",
        ],
        check=False,
        env=environment,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert crash.returncode == 137, crash.stderr
    assert len(list((crash_output / "snapshots").glob("step_*.json"))) == 2
    resumed = subprocess.run(
        [
            sys.executable,
            "-m",
            "cover_replay",
            "replay",
            "--input",
            str(sample_dir),
            "--output",
            str(crash_output),
            "--resume",
            "--no-baseline",
        ],
        check=False,
        env=environment,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert resumed.returncode == 0, resumed.stderr
    full_output = tmp_path / "full"
    full = subprocess.run(
        [
            sys.executable,
            "-m",
            "cover_replay",
            "replay",
            "--input",
            str(sample_dir),
            "--output",
            str(full_output),
            "--clean",
            "--no-baseline",
        ],
        check=False,
        env=environment,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert full.returncode == 0, full.stderr
    assert (crash_output / "trajectory.jsonl").read_bytes() == (
        full_output / "trajectory.jsonl"
    ).read_bytes()

