from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path

import numpy as np
import soundfile as sf

from choir_master.manifest import load_project
from choir_master.mastering import read_project_audio
from choir_master.render import prepare_render, render_project
from choir_master.generate import init_workspace

from conftest import load_json, write_manifest_variant


def _sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _voice_bus(project, signals: dict[str, list[np.ndarray]], voice: str) -> np.ndarray:
    total = max(array.shape[0] for array in signals[voice])
    output = np.zeros(total, dtype=np.float64)
    for mic, audio in zip(
        sorted(project.mics_for_voice(voice), key=lambda item: item.name),
        signals[voice],
    ):
        output += mic.downmix_weight * np.mean(audio, axis=1)
    return output


def test_detection_compensation_and_master_compliance(manifest_path: Path) -> None:
    project = load_project(manifest_path)
    prepared = prepare_render(project)
    truth = project.manifest["generation"]["mutation_intervals_s"][0]
    detected = prepared.intervals["男高"]
    assert len(detected) == 1
    rmse = math.sqrt(
        ((detected[0].start_s - truth[0]) ** 2 + (detected[0].end_s - truth[1]) ** 2)
        / 2.0
    )
    assert rmse <= 0.050
    for voice in ("女高", "女低", "男低"):
        assert prepared.intervals[voice] == []
    assert len(prepared.compensation) == 1
    assert prepared.compensation[0].improvement_db >= 8.0

    original_signals, _ = read_project_audio(project)
    interval = detected[0]
    start = int(round(interval.start_s * project.sample_rate))
    end = int(round(interval.end_s * project.sample_rate))
    tenor_original = _voice_bus(project, original_signals, "男高")
    mask = np.ones(tenor_original.size, dtype=bool)
    mask[start:end] = False
    assert np.array_equal(prepared.voice_buses["男高"][mask], tenor_original[mask])
    alto_original = _voice_bus(project, original_signals, "女低")
    assert np.array_equal(prepared.voice_buses["女低"], alto_original)

    master = prepared.master
    assert abs(master.integrated_loudness_lufs - project.target_loudness_lufs) <= 0.5
    assert master.true_peak_dbtp <= project.true_peak_limit_dbtp
    assert max(
        abs(balance.achieved_ratio - balance.target_ratio)
        for balance in master.balances
    ) <= 0.02


def test_render_is_byte_deterministic(
    manifest_path: Path,
    tmp_path: Path,
    reference_render: dict[str, Path],
) -> None:
    second = tmp_path / "second"
    output = second / "master.wav"
    report = second / "report.json"
    snapshot = second / "master.snapshot"
    render_project(
        manifest_path,
        output_path=output,
        report_path=report,
        snapshot_dir=snapshot,
        block_frames=12000,
        fresh=True,
    )
    assert output.read_bytes() == reference_render["output"].read_bytes()
    assert report.read_bytes() == reference_render["report"].read_bytes()

    info = sf.info(output)
    assert info.samplerate == 48000
    assert info.channels == 2
    assert info.subtype == "PCM_24"
    project = load_project(manifest_path)
    assert info.frames == int(project.duration_seconds * project.sample_rate)
    document = load_json(report)
    assert document["status"] == "complete"
    assert document["recovery_count"] == 0
    assert document["limiter_triggered_samples"] == 0
    assert "/" not in document["output_sha256"]
    serialized = json.dumps(document, ensure_ascii=False)
    assert str(tmp_path) not in serialized
    assert str(Path.home()) not in serialized
    assert document["checks"]["minimum_compensation_improvement_db"] >= 8.0
    assert document["checks"]["maximum_voice_balance_deviation"] <= 0.02


def test_true_peak_limiter_path(manifest_path: Path, tmp_path: Path) -> None:
    variant = write_manifest_variant(
        manifest_path,
        manifest_path.parent / "project-loud.json",
        lambda document: document.__setitem__("target_loudness_lufs", -12.0),
    )
    output = tmp_path / "loud.wav"
    report = tmp_path / "loud.json"
    render_project(
        variant,
        output_path=output,
        report_path=report,
        snapshot_dir=tmp_path / "loud.snapshot",
        block_frames=12000,
        fresh=True,
    )
    document = load_json(report)
    assert document["limiter_active"] is True
    assert document["limiter_triggered_samples"] > 0
    assert abs(document["master_loudness_lufs"] - (-12.0)) <= 0.5
    assert document["true_peak_dbtp"] <= -1.0
    assert document["checks"]["maximum_voice_balance_deviation"] <= 0.02
    info = sf.info(output)
    assert info.frames == document["total_frames"]


def test_16_bit_pcm_output(tmp_path: Path) -> None:
    root = tmp_path / "pcm16"
    manifest = init_workspace(
        root,
        seed=17,
        duration_seconds=4.0,
        block_frames=12000,
        target_loudness_lufs=-20.0,
        bit_depth=16,
    )
    output = root / "master.wav"
    report = root / "report.json"
    render_project(
        manifest,
        output_path=output,
        report_path=report,
        snapshot_dir=root / "master.snapshot",
        block_frames=12000,
        fresh=True,
    )
    info = sf.info(output)
    assert info.subtype == "PCM_16"
    assert info.samplerate == 48000
    document = load_json(report)
    assert document["bit_depth"] == 16
    assert abs(document["master_loudness_lufs"] + 20.0) <= 0.5
    assert document["true_peak_dbtp"] <= -1.0

