from __future__ import annotations

import hashlib
import json
from pathlib import Path

import numpy as np
from scipy.io import wavfile

from scene_audio.render import render_scene


def test_report_alignment_reverb_spatial_and_master_compliance(
    full_render: tuple[Path, dict],
) -> None:
    out, report = full_render
    assert report["status"] == "complete"
    assert report["total_samples"] == 288000
    assert len(report["alignment"]) == 5
    for alignment in report["alignment"]:
        assert alignment["status"] == "OK"
        assert alignment["placement_error_samples"] <= 1.0 + 1e-12
        assert isinstance(alignment["output_offset_samples"], int)
        assert isinstance(alignment["source_offset_integer_samples"], int)

    master = report["master"]
    assert abs(master["integrated_loudness_lufs"] - (-23.0)) <= 0.5
    assert master["true_peak_dbtp"] <= -1.0 + 1e-9
    assert master["true_peak_compliant"] is True
    assert abs(sum(item["ratio"] for item in master["event_contributions"]) - 1.0) < 1e-12

    sample_rate, samples = wavfile.read(out / "master.wav")
    assert sample_rate == 48000
    assert samples.shape == (288000, 2)
    decoded = samples.astype(np.float64) / float(2**31)
    assert float(np.max(np.abs(decoded))) <= 10.0 ** (-1.0 / 20.0) + 1e-12

    digest = hashlib.sha256((out / "master.wav").read_bytes()).hexdigest()
    assert digest == master["sha256"] == report["hashes"]["master_sha256"]

    assert report["spatial_summary"]["ild_rate_compliant"] is True
    assert report["spatial_summary"]["itd_rate_compliant"] is True
    assert report["reverb_summary"]["max_rt60_s"] > report["reverb_summary"]["min_rt60_s"]
    assert report["frames"]
    for frame in report["frames"]:
        assert {"sample", "time", "layer_count", "events"} <= set(frame)
        assert len(frame["events"]) == 5
        for event in frame["events"]:
            assert {
                "event_id",
                "occlusion_layers",
                "rt60",
                "wet_dry",
                "ild_db",
                "itd_us",
            } <= set(event)
            assert isinstance(event["occlusion_layers"], int)
            assert event["rt60"] > 0.0
            assert 0.0 <= event["wet_dry"] <= 1.0

    serialized = json.dumps(report, sort_keys=True)
    assert "generated_at" not in serialized
    assert str(out) not in serialized
    assert "\\\\" not in serialized


def test_rendering_is_byte_deterministic(
    demo_scene: Path,
    full_render: tuple[Path, dict],
    tmp_path: Path,
) -> None:
    out, reference_report = full_render
    second_out = tmp_path / "second"
    result = render_scene(demo_scene, second_out)
    assert result.exit_code == 0
    assert result.report == reference_report
    assert (out / "master.wav").read_bytes() == (second_out / "master.wav").read_bytes()


def test_verify_reference_mismatch_writes_structured_difference(
    demo_scene: Path,
    full_render: tuple[Path, dict],
    tmp_path: Path,
) -> None:
    reference_out, _ = full_render
    sample_rate, samples = wavfile.read(reference_out / "master.wav")
    altered = samples.copy()
    altered[12345, 1] += 256
    altered_path = tmp_path / "altered_reference.wav"
    wavfile.write(str(altered_path), sample_rate, altered)
    out = tmp_path / "verified"
    result = render_scene(
        demo_scene,
        out,
        verify_reference=altered_path,
    )
    assert result.exit_code == 3
    assert result.error is not None
    assert result.error["code"] == "E_RESUME_OUTPUT_MISMATCH"
    difference = json.loads(
        (out / "difference_report.json").read_text(encoding="utf-8")
    )
    assert difference["status"] == "mismatch"
    sample_differences = [
        item
        for item in difference["first_differences"]
        if item["kind"] == "sample"
    ]
    assert sample_differences
    first = sample_differences[0]
    assert {"block_index", "sample", "channel"} <= set(first)
    assert first["sample"] == 12345
    assert first["channel"] == 1


def test_detection_failure_is_quality_warning_and_render_continues(
    tmp_path: Path,
) -> None:
    assets = tmp_path / "assets"
    assets.mkdir()
    sample_rate = 48000
    frame_count = 24000
    mono = np.zeros(frame_count, dtype=np.float64)
    time = np.arange(4800, dtype=np.float64) / sample_rate
    mono[:4800] = 0.25 * np.sin(2.0 * np.pi * 330.0 * time)
    wavfile.write(
        str(assets / "dialogue.wav"),
        sample_rate,
        np.rint(mono * 32767.0).astype(np.int16),
    )
    scene = {
        "version": 1,
        "name": "detection_failure_demo",
        "duration": 0.5,
        "output": {
            "sample_rate": sample_rate,
            "channels": 2,
            "block_frames": 4096,
            "report_frame_stride": 2400,
            "target_loudness_lufs": -23.0,
            "true_peak_limit_dbtp": -1.0,
        },
        "camera": {
            "up": [0.0, 0.0, 1.0],
            "path": [
                {
                    "time": 0.0,
                    "position": [0.0, 0.0, 1.7],
                    "forward": [0.0, 1.0, 0.0],
                },
                {
                    "time": 0.5,
                    "position": [0.0, 0.5, 1.7],
                    "forward": [0.0, 1.0, 0.0],
                },
            ],
        },
        "tracks": [
            {
                "name": "dialogue",
                "path": "assets/dialogue.wav",
                "sample_rate": sample_rate,
                "channels": 1,
                "events": [
                    {
                        "id": "AUD_001",
                        "type": "dialogue",
                        "start_time": 0.0,
                        "duration": 0.1,
                        "source_position": [1.0, 1.0, 1.6],
                        "gain_db": 0.0,
                        "detection": {"threshold": 0.05, "search_ms": 5.0},
                    },
                    {
                        "id": "MISSING_001",
                        "type": "effect",
                        "start_time": 0.25,
                        "duration": 0.1,
                        "source_position": [-1.0, 1.0, 1.2],
                        "gain_db": 0.0,
                        "detection": {"threshold": 0.05, "search_ms": 20.0},
                    },
                ],
            }
        ],
        "reverb": {
            "blend_width": 0.05,
            "regions": [
                {
                    "start_time": 0.0,
                    "end_time": 0.5,
                    "rt60": 0.5,
                    "wet_dry": 0.15,
                }
            ],
        },
        "spatial_limits": {
            "max_ild_db": 10.0,
            "max_itd_us": 620.0,
            "max_ild_rate_db_per_s": 90.0,
            "max_itd_rate_us_per_s": 6000.0,
        },
    }
    scene_path = tmp_path / "scene.json"
    scene_path.write_text(json.dumps(scene), encoding="utf-8")
    out = tmp_path / "out"
    result = render_scene(scene_path, out)
    assert result.exit_code == 0
    assert result.report is not None
    warnings = result.report["validation"]["quality_warnings"]
    assert any(
        warning["code"] == "DETECTION_FAILED"
        and warning["event_id"] == "MISSING_001"
        for warning in warnings
    )
    missing = next(
        item
        for item in result.report["alignment"]
        if item["event_id"] == "MISSING_001"
    )
    assert missing["status"] == "DETECTION_FAILED"
    assert missing["placement_error_samples"] <= 1.0
    assert (out / "master.wav").exists()

