from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import soundfile as sf

from studio_calibrator.config import load_config
from studio_calibrator.engine import AnalysisEngine, RecomputeManager
from studio_calibrator.measurement_io import load_manifest, read_component
from studio_calibrator.samplegen import generate_sample
from studio_calibrator.snapshot import SnapshotStore
from studio_calibrator.util import file_hash


def test_sample_generator_is_byte_deterministic(sample: Path, tmp_path: Path) -> None:
    second = tmp_path / "second"
    generate_sample(second, seed=42, duration=1.0)
    first_files = sorted(
        path.relative_to(sample).as_posix()
        for path in sample.rglob("*")
        if path.is_file()
    )
    second_files = sorted(
        path.relative_to(second).as_posix()
        for path in second.rglob("*")
        if path.is_file()
    )
    assert first_files == second_files
    for relative in first_files:
        assert file_hash(sample / relative) == file_hash(second / relative)


def test_mono_and_51_layouts_decode_identical_components(sample: Path) -> None:
    mono_manifest = load_manifest(sample / "manifest.json", "mono")
    layout_manifest = load_manifest(sample / "manifest.json", "5.1")
    mono_pair = mono_manifest.pair("FR", "BL")
    layout_pair = layout_manifest.pair("FR", "BL")
    mono = read_component(mono_manifest, mono_pair)
    five_one = read_component(layout_manifest, layout_pair)
    assert np.array_equal(mono, five_one)
    info = sf.info(str(layout_pair.path))
    assert info.samplerate == 48000
    assert info.channels == 6
    assert info.subtype == "PCM_16"


def test_alignment_coupling_budget_and_calibration_match_truth(
    sample: Path, baseline: dict
) -> None:
    state = baseline["state"]
    truth = json.loads((sample / "truth.json").read_text(encoding="utf-8"))
    assert state.status == "ok"
    assert len(state.alignments) == 36
    assert len(state.coupling) == 216

    arrival_errors = []
    for alignment in state.alignments:
        key = f"{alignment['source']}|{alignment['receiver']}"
        arrival_errors.append(
            abs(alignment["arrival_samples"] - truth["delays"][key])
        )
        assert alignment["confidence"] in {"high", "medium"}
    assert max(arrival_errors) <= 2

    gain_errors = []
    phase_errors = []
    for estimate in state.coupling:
        key = f"{estimate['source']}|{estimate['receiver']}"
        expected = truth["coupling"][key][estimate["band_id"]]
        gain_errors.append(abs(estimate["strength_db"] - expected["gain_db"]))
        phase_errors.append(
            abs((estimate["phase_deg"] - expected["phase_deg"] + 180.0) % 360.0 - 180.0)
        )
        assert estimate["strength_ci_db"][0] <= estimate["strength_ci_db"][1]
        assert estimate["phase_ci_deg"][0] <= estimate["phase_ci_deg"][1]
        assert estimate["coherence"] > 0.95
        assert estimate["valid_samples"] >= 64
    assert max(gain_errors) < 0.3
    assert max(phase_errors) < 2.0

    for band in state.budget:
        assert band["short_bands"] == []
        for entry in band["entries"]:
            if entry["source"] != entry["receiver"]:
                assert (
                    entry["target_suppression_db"]
                    == state.config["target_suppression_db"]
                )
                assert entry["status"] in {"ok", "short"}
                assert entry["achievable_suppression_db"] >= 0.0

    calibration = state.calibration
    assert calibration["feasible"] is True
    assert calibration["publishable"] is True
    assert calibration["constraint_order"] == [
        "phase_polarity",
        "target_suppression",
        "gain_cap",
        "main_gain_preserved",
    ]
    assert len(calibration["settings"]) == 216
    cap = float(state.config["max_cancellation_gain_db"])
    for setting in calibration["settings"]:
        assert 0.0 <= setting["gain_db"] <= cap
        if setting["source"] == setting["receiver"]:
            assert setting["gain_db"] == 0.0
            assert setting["polarity"] == 1
        assert setting["main_gain_unchanged"] is True
        assert setting["feasible"] is True

    route = state.route_matrix[0]
    for receiver, row in route["matrix"].items():
        for source, value in row.items():
            if source == receiver:
                assert value["amplitude"] == 1.0
                assert value["is_main"] is True
    assert any(
        finding["kind"] == "polarity_reversed"
        and finding["source"] == "FL"
        and finding["receiver"] == "FR"
        for finding in state.phase_findings
    )
    assert any(
        finding["kind"] == "boundary_phase_wrap"
        for finding in state.phase_findings
    )


def test_51_layout_reaches_same_coupling(
    sample: Path, baseline: dict, tmp_path: Path
) -> None:
    raw = json.loads((sample / "config.json").read_text(encoding="utf-8"))
    raw["measurement_layout"] = "5.1"
    config_path = tmp_path / "config_51.json"
    config_path.write_text(
        json.dumps(raw, sort_keys=True, indent=2) + "\n", encoding="utf-8"
    )
    manager = RecomputeManager(
        AnalysisEngine(sample / "manifest.json", sample / "geometry.json"),
        SnapshotStore(tmp_path / "snapshots_51"),
    )
    state_51 = manager.recompute(load_config(config_path), tmp_path / "trace_51.jsonl")
    assert state_51.status == "ok"
    mono = {
        (item["source"], item["receiver"], item["band_id"]): (
            item["strength_db"],
            item["phase_deg"],
        )
        for item in baseline["state"].coupling
    }
    for item in state_51.coupling:
        key = (item["source"], item["receiver"], item["band_id"])
        assert abs(item["strength_db"] - mono[key][0]) < 1e-9
        assert abs(item["phase_deg"] - mono[key][1]) < 1e-9


def test_repeat_run_produces_identical_trace_and_state(
    sample: Path, baseline: dict, tmp_path: Path
) -> None:
    manager = RecomputeManager(
        AnalysisEngine(sample / "manifest.json", sample / "geometry.json"),
        SnapshotStore(tmp_path / "snapshots_repeat"),
    )
    state = manager.recompute(
        load_config(sample / "config.json"), tmp_path / "trace_repeat.jsonl"
    )
    assert state.content_hash_value == baseline["state"].content_hash_value
    assert (tmp_path / "trace_repeat.jsonl").read_bytes() == Path(
        baseline["trace_path"]
    ).read_bytes()
    events = [
        json.loads(line)
        for line in Path(baseline["trace_path"]).read_text(encoding="utf-8").splitlines()
    ]
    assert any(
        event["node"] == "coupling"
        and event["action"] == "estimate"
        and "strength_db" in event["details"]
        and "content_hash" in event
        for event in events
    )
    assert any(
        event["node"] == "budget"
        and event["action"] == "pair_budget"
        and "residual_energy" in event["details"]
        and event["cached"] is False
        for event in events
    )
    assert any(
        event["node"] == "calibration"
        and event["action"] == "setting"
        and "gain_db" in event["details"]
        and "content_hash" in event
        for event in events
    )
    assert not any("timestamp" in event for event in events)
    assert not any("file_size" in event for event in events)
