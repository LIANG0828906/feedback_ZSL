import json
import os

import numpy as np
import soundfile as sf

from applause_master.bands import compute_ratios
from applause_master.detect import detect_events
from applause_master.pipeline import read_microphones, run_pipeline
from applause_master.project import load_project
from applause_master.synth import generate_project_audio


def test_stereo_microphone_project_runs(tmp_path):
    from applause_master.cli import build_demo_project
    project_path = build_demo_project(str(tmp_path / "work"), seed=11, duration_seconds=6.0)
    with open(project_path) as handle:
        raw = json.load(handle)
    base = os.path.dirname(project_path)
    for mic in raw["microphones"]:
        mic["file"] = os.path.join(base, mic["file"])
    raw["microphones"][0]["channels"] = 2
    with open(project_path, "w") as handle:
        json.dump(raw, handle)
    raw["project_path"] = project_path
    generate_project_audio(raw)
    project = load_project(project_path)
    assert project["microphones"][0]["channels"] == 2
    info = sf.info(project["microphones"][0]["file"])
    assert info.channels == 2
    result = run_pipeline(project, str(tmp_path / "out"))
    report = json.load(open(result["report"]))
    assert report["total_samples"] == int(6.0 * project["sample_rate"])


def test_full_gate_falls_back_to_equal_weights(demo_workspace):
    project = demo_workspace["project"]
    signals, _ = read_microphones(project)
    events = detect_events(signals, project["sample_rate"], project["detection_threshold"])
    names = [mic["name"] for mic in project["microphones"]]
    # Gate above 1.0 means no real microphone can participate in any band.
    table, _, _ = compute_ratios(events, names, project["freq_bands"], signals,
                                 project["sample_rate"], 10.0)
    for entry in table:
        for band in entry["bands"]:
            assert band["fallback_equal_weights"] is True
            values = list(band["ratios"].values())
            assert np.allclose(values, 1.0 / len(names))
            assert abs(sum(values) - 1.0) <= 1e-6


def test_different_seed_changes_schedule_but_keeps_accuracy(tmp_path):
    from applause_master.cli import build_demo_project
    from applause_master.pipeline import detection_accuracy
    for seed in (1, 99, 20261118):
        project_path = build_demo_project(str(tmp_path / f"w{seed}"),
                                          seed=seed, duration_seconds=8.0)
        with open(project_path) as handle:
            raw = json.load(handle)
        base = os.path.dirname(project_path)
        for mic in raw["microphones"]:
            mic["file"] = os.path.join(base, mic["file"])
        raw["project_path"] = project_path
        truth = generate_project_audio(raw)
        project = load_project(project_path)
        signals, _ = read_microphones(project)
        events = detect_events(signals, project["sample_rate"],
                               project["detection_threshold"])
        assert abs(len(events) - truth["event_count"]) <= 1
        assert detection_accuracy(events, truth, project["sample_rate"]) <= 20.0
