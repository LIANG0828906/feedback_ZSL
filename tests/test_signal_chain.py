from __future__ import annotations

import hashlib
import math
from pathlib import Path

import numpy as np

from festival_master.audio_io import read_wav_mono
from festival_master.manifest import load_manifest
from festival_master.render import detect_onset, prepare_render, run_render
from festival_master.spatial import pan_parameters, source_angles
from festival_master.synth import generate_material, synthesize_channel


def test_generation_and_spatial_metadata_are_reproducible(project: dict[str, Path]) -> None:
    manifest_path: Path = project["manifest"]
    manifest = load_manifest(manifest_path)

    first = [synthesize_channel(manifest, index) for index in range(len(manifest.channels))]
    second = [synthesize_channel(manifest, index) for index in range(len(manifest.channels))]
    for left, right in zip(first, second):
        assert np.array_equal(left, right)

    before = {
        spec.channel_id: hashlib.sha256(Path(spec.path).read_bytes()).hexdigest()
        for spec in manifest.channels
    }
    report = generate_material(manifest)
    after = {
        spec.channel_id: hashlib.sha256(Path(spec.path).read_bytes()).hexdigest()
        for spec in manifest.channels
    }
    assert before == after

    by_id = {entry["channel"]: entry for entry in report["channels"]}
    for spec in manifest.channels:
        azimuth, elevation = source_angles(spec.anchor, manifest.listener)
        pan = pan_parameters(azimuth, manifest.sample_rate)
        entry = by_id[spec.channel_id]
        assert math.isclose(entry["azimuth_deg"], azimuth, abs_tol=1e-12)
        assert math.isclose(entry["elevation_deg"], elevation, abs_tol=1e-12)
        assert math.isclose(entry["ild_db"], pan.ild_db, abs_tol=1e-12)
        assert entry["start_offset_samples"] == spec.start_offset_samples


def test_alignment_within_one_sample_and_longest_timeline(project: dict[str, Path]) -> None:
    manifest = load_manifest(project["manifest"])
    prepared = prepare_render(manifest)
    durations = []
    for record in prepared.alignment:
        assert abs(detect_onset(record.waveform) - record.spec.start_offset_samples) <= 1
        assert record.aligned_offset_samples == record.spec.start_offset_samples
        durations.append(record.waveform.size)
    assert prepared.total_samples == max(durations)
    assert prepared.total_blocks == math.ceil(prepared.total_samples / manifest.block_samples)


def test_pan_mapping_has_no_side_switch_and_static_ild_is_constant() -> None:
    previous = None
    for index in range(-4500, 4501):
        azimuth = index / 100.0
        pan = pan_parameters(azimuth, 48000)
        if previous is not None:
            assert abs(pan.ild_db - previous.ild_db) <= 0.01
        previous = pan

    left = pan_parameters(-0.001, 48000)
    right = pan_parameters(0.001, 48000)
    assert abs(left.left_gain - right.left_gain) < 1e-4
    assert abs(left.right_gain - right.right_gain) < 1e-4
    fixed = pan_parameters(-29.744881, 48000)
    assert fixed.max_ild_step_db == 0.0


def test_render_is_deterministic_and_compliant(project: dict[str, Path], tmp_path: Path) -> None:
    manifest = load_manifest(project["manifest"])
    output_a = tmp_path / "a.wav"
    report_a = tmp_path / "a.json"
    output_b = tmp_path / "b.wav"
    report_b = tmp_path / "b.json"

    status_a = run_render(
        manifest,
        output_a,
        report_a,
        tmp_path / "work_a",
        fresh=True,
    )
    status_b = run_render(
        manifest,
        output_b,
        report_b,
        tmp_path / "work_b",
        fresh=True,
    )
    assert status_a == status_b == "complete"
    assert output_a.read_bytes() == output_b.read_bytes()

    import json

    report = json.loads(report_a.read_text(encoding="utf-8"))
    assert report["status"] == "complete"
    assert report["exit_code"] == 0
    assert len(report["tracks"]) == 6
    assert math.isclose(sum(g["contribution_percent"] for g in report["groups"]), 100.0, abs_tol=1e-9)
    assert report["progress"]["completed_blocks"] == report["progress"]["total_blocks"]
    assert report["master"]["total_samples"] == report["progress"]["total_samples"]
    assert report["master"]["sha256"] == hashlib.sha256(output_a.read_bytes()).hexdigest()
    assert abs(report["loudness"]["deviation_lu"]) <= 0.5
    assert report["loudness"]["true_peak_dbtp"] <= report["loudness"]["true_peak_limit_dbtp"]
    assert report["loudness"]["limiter"]["triggered_samples"] <= report["master"]["total_samples"]

    prepared = prepare_render(manifest)
    longest = max(record.waveform.size for record in prepared.alignment)
    assert prepared.total_samples == longest
    for track in report["tracks"]:
        assert math.isclose(
            track["left_energy_percent"] + track["right_energy_percent"],
            100.0,
            abs_tol=1e-9,
        )
        assert track["max_ild_step_db"] <= track["spatial_stability_threshold_db"]
        assert track["spatially_stable"] is True

    top_level = [
        "report_version",
        "status",
        "exit_code",
        "business_codes",
        "manifest_sha256",
        "seed",
        "sample_rate",
        "bit_depth",
        "target_loudness_lufs",
        "true_peak_limit_dbtp",
        "spatial_stability_threshold_db",
        "tracks",
        "groups",
        "progress",
        "resume",
        "master",
        "loudness",
    ]
    assert list(report.keys()) == top_level
    assert not any(
        isinstance(value, str) and value.startswith("/")
        for value in _walk_strings(report)
    )


def _walk_strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for item in value.values():
            yield from _walk_strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from _walk_strings(item)
