from __future__ import annotations

import hashlib
import json
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf

from orchestral_mastering.alignment import align_stems
from orchestral_mastering.dsp import integrated_loudness, true_peak_dbtp, window_gate
from orchestral_mastering.manifest import load_manifest
from orchestral_mastering.pipeline import RenderConfig, _load_sources, render_project
from orchestral_mastering.synthesis import create_demo_project
from orchestral_mastering.unmask import unmask_stems


def test_alignment_unmask_balance_and_loudness(rendered: dict) -> None:
    report = rendered["report"]
    alignment = report["alignment"]
    assert alignment["master_frames"] == max(
        item["aligned_frames"] for item in alignment["tracks"]
    )
    for item in alignment["tracks"]:
        assert item["detected"] is True
        assert abs(item["alignment_error_samples"]) <= 1

    unmask_channels = report["unmasking"]["channels"]
    assert {item["source_group"] for item in unmask_channels} == {"铜管", "合唱"}
    for item in unmask_channels:
        assert item["detected"] is True
        assert item["attenuation_db"] >= report["unmasking"]["min_attenuation_db"]
        assert item["clean_energy_loss_db"] <= 1.0
        assert item["modified_sample_count"] > 0
        for interval in item["modified_intervals"]:
            assert interval["samples"] == interval["end"] - interval["start"]

    assert report["balance"]["max_ratio_deviation"] <= 0.02
    assert report["master"]["max_ratio_deviation_after_limiter"] <= 0.02
    assert abs(report["master"]["loudness_error_lu"]) <= 0.5
    assert report["master"]["true_peak_dbtp_after_limiter"] <= report["master"]["target_true_peak_dbtp"]
    assert report["compliance"]["overall"] is True

    output = Path(report["output_path"])
    info = sf.info(str(output))
    assert info.channels == 2
    assert info.frames == alignment["master_frames"]
    assert info.samplerate == report["sample_rate"]
    assert info.subtype == "PCM_24"
    audio, rate = sf.read(str(output), dtype="float64", always_2d=True)
    assert abs(integrated_loudness(audio, rate) - (-18.0)) <= 0.5
    assert true_peak_dbtp(audio) <= -1.0
    assert report["snapshot"]["commits"] == report["snapshot"]["total_blocks"]


def test_unmask_only_touches_detected_targets_inside_windows(demo_root: Path) -> None:
    manifest = load_manifest(demo_root / "manifest.json")
    stems, references = _load_sources(manifest)
    alignment = align_stems(manifest, stems, references)
    unmasked = unmask_stems(manifest, alignment)
    target_roles = {relation.target_group for relation in manifest.masking}
    for role in stems:
        before = alignment.stems[role].audio
        after = unmasked.stems[role]
        if role not in target_roles:
            assert np.array_equal(before, after)
            continue
        gate = window_gate(
            len(before),
            manifest.sample_rate,
            list(manifest.unmask_windows_seconds),
            fade_seconds=0.006,
        )
        outside = gate == 0
        assert np.array_equal(before[outside], after[outside])

    for channel in unmasked.channels:
        assert channel.attenuation_db >= manifest.min_attenuation_db
        assert channel.clean_loss_db <= 1.0
        assert channel.modified_sample_count > 0


def test_synthesis_is_byte_deterministic(tmp_path: Path) -> None:
    first = tmp_path / "first"
    second = tmp_path / "second"
    create_demo_project(first, seed=99117, duration_seconds=2.0, force=True)
    create_demo_project(second, seed=99117, duration_seconds=2.0, force=True)
    assert (first / "manifest.json").read_bytes() == (second / "manifest.json").read_bytes()
    for path in sorted((first / "audio").glob("*.wav")):
        mirror = second / "audio" / path.name
        assert hashlib.sha256(path.read_bytes()).digest() == hashlib.sha256(
            mirror.read_bytes()
        ).digest()


def test_render_and_report_repeat_byte_identically(rendered: dict) -> None:
    root = rendered["root"]
    manifest_path = Path(rendered["report"]["manifest"]["path"])
    report_path = root / "report.json"
    output_path = root / "master.wav"
    first_report = report_path.read_bytes()
    first_master = output_path.read_bytes()
    second = render_project(
        RenderConfig(
            manifest_path=manifest_path,
            output_path=output_path,
            report_path=report_path,
            snapshot_path=root / "snapshot.json",
            block_frames=16384,
        )
    )
    assert second["status"] == "complete"
    assert report_path.read_bytes() == first_report
    assert output_path.read_bytes() == first_master

