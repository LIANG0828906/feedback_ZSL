from __future__ import annotations

import hashlib
import json
import signal
import struct
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

import numpy as np
import pytest

from audiobook_master import generate_packages, inspect_packages, render_book
from audiobook_master.dsp import (
    integrated_loudness,
    noise_floor_dbfs,
    true_peak_dbtp,
)
from audiobook_master.errors import BusinessError
from audiobook_master.wavio import read_wav

PROJECT_ROOT = Path(__file__).resolve().parents[1]


def _write_json(path: Path, value: Any) -> None:
    path.write_text(
        json.dumps(value, indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )


def _make_book(
    root: Path,
    *,
    target: float = -18.0,
    noise_limit: float = 6.0,
    true_peak: float = -1.0,
    durations: tuple[float, ...] = (1.7, 1.5, 1.9),
    batches: tuple[str, ...] = ("light", "compressed", "mic_b"),
    block_frames: int = 4096,
    generate: bool = True,
) -> tuple[Path, Path]:
    audio_dir = root / "audio"
    audio_dir.mkdir(parents=True, exist_ok=True)
    chapters = [
        {
            "id": f"ch{index + 1:02d}",
            "expected_duration_seconds": duration,
        }
        for index, duration in enumerate(durations)
    ]
    rules = {
        "sample_rate": 48000,
        "bit_depth": 24,
        "target_loudness_lufs": target,
        "relative_gate_lu": -10.0,
        "true_peak_dbtp": true_peak,
        "max_noise_floor_rise_db": noise_limit,
        "boundary_tolerance_db": 0.5,
        "block_frames": block_frames,
        "chapters": chapters,
    }
    manifest = {
        "chapters": [
            {
                "id": f"ch{index + 1:02d}",
                "packages": [
                    {
                        "file": f"audio/ch{index + 1:02d}.wav",
                        "channels": 2,
                        "sample_rate": 48000,
                        "batch": batch,
                    }
                ],
            }
            for index, batch in enumerate(batches)
        ]
    }
    rules_path = root / "rules.json"
    manifest_path = root / "manifest.json"
    _write_json(rules_path, rules)
    _write_json(manifest_path, manifest)
    if generate:
        generate_packages(rules_path, manifest_path)
    return rules_path, manifest_path


def _file_hash(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _all_keys(value: Any) -> set[str]:
    keys: set[str] = set()
    if isinstance(value, dict):
        for key, child in value.items():
            keys.add(str(key))
            keys.update(_all_keys(child))
    elif isinstance(value, list):
        for child in value:
            keys.update(_all_keys(child))
    return keys


def _all_strings(value: Any) -> list[str]:
    strings: list[str] = []
    if isinstance(value, str):
        strings.append(value)
    elif isinstance(value, dict):
        for child in value.values():
            strings.extend(_all_strings(child))
    elif isinstance(value, list):
        for child in value:
            strings.extend(_all_strings(child))
    return strings


def test_generation_is_deterministic_and_inspectable(tmp_path: Path) -> None:
    rules_path, manifest_path = _make_book(tmp_path)
    first_hashes = {
        path.name: _file_hash(path) for path in (tmp_path / "audio").glob("*.wav")
    }
    generated = generate_packages(rules_path, manifest_path)
    second_hashes = {
        path.name: _file_hash(path) for path in (tmp_path / "audio").glob("*.wav")
    }
    assert first_hashes == second_hashes
    assert len(generated["packages"]) == 3

    inspected = inspect_packages(rules_path, manifest_path)
    assert inspected["ok"] is True
    expected_frames = [round(value * 48000) for value in (1.7, 1.5, 1.9)]
    actual_frames = [
        chapter["packages"][0]["frames"] for chapter in inspected["chapters"]
    ]
    assert actual_frames == expected_frames
    for chapter in inspected["chapters"]:
        package = chapter["packages"][0]
        assert package["channels"] == 2
        assert package["sample_rate"] == 48000
        assert package["true_peak_dbtp"] > -120.0
        assert package["sha256"] == first_hashes[Path(package["file"]).name]


def test_config_errors_have_paths_and_no_master(tmp_path: Path) -> None:
    rules_path, manifest_path = _make_book(tmp_path, generate=False)
    rules = json.loads(rules_path.read_text())
    manifest = json.loads(manifest_path.read_text())
    master = tmp_path / "must-not-exist.wav"
    report = tmp_path / "report.json"

    invalid_cases: list[tuple[dict[str, Any], dict[str, Any], str]] = []
    bad_target = json.loads(json.dumps(rules))
    bad_target["target_loudness_lufs"] = 0.0
    invalid_cases.append(
        (bad_target, manifest, "rules.target_loudness_lufs")
    )
    bad_peak = json.loads(json.dumps(rules))
    bad_peak["true_peak_dbtp"] = 0.5
    invalid_cases.append((bad_peak, manifest, "rules.true_peak_dbtp"))

    bad_rate = json.loads(json.dumps(manifest))
    bad_rate["chapters"][0]["packages"] = [
        {
            "file": "audio/a.wav",
            "channels": 2,
            "sample_rate": 48000,
            "batch": "light",
            "duration_seconds": 0.85,
        },
        {
            "file": "audio/b.wav",
            "channels": 2,
            "sample_rate": 44100,
            "batch": "light",
            "duration_seconds": 0.85,
        },
    ]
    invalid_cases.append(
        (
            rules,
            bad_rate,
            "manifest.chapters[0].packages[1].sample_rate",
        )
    )

    for bad_rules, bad_manifest, expected_path in invalid_cases:
        case_dir = tmp_path / f"case-{len(list(tmp_path.glob('case-*')))}"
        case_dir.mkdir()
        case_rules = case_dir / "rules.json"
        case_manifest = case_dir / "manifest.json"
        _write_json(case_rules, bad_rules)
        _write_json(case_manifest, bad_manifest)
        with pytest.raises(BusinessError) as caught:
            render_book(
                case_rules,
                case_manifest,
                master,
                report,
                case_dir / "state",
            )
        assert caught.value.code == "E_CONFIG_INVALID"
        assert caught.value.path == expected_path
        assert not master.exists()

    # A WAV whose actual frame count disagrees with the rules must also be
    # rejected before the master is touched.
    valid_rules_path, valid_manifest_path = _make_book(tmp_path / "duration")
    changed_rules = json.loads(valid_rules_path.read_text())
    changed_rules["chapters"][0]["expected_duration_seconds"] = 1.9
    _write_json(valid_rules_path, changed_rules)
    with pytest.raises(BusinessError) as caught:
        render_book(
            valid_rules_path,
            valid_manifest_path,
            master,
            report,
            tmp_path / "duration-state",
        )
    assert caught.value.code == "E_CONFIG_INVALID"
    assert caught.value.path == "manifest.chapters[0].packages[0].expected_duration_seconds"
    assert not master.exists()


def test_full_render_meets_all_invariants(tmp_path: Path) -> None:
    rules_path, manifest_path = _make_book(tmp_path)
    master_path = tmp_path / "book.wav"
    report_path = tmp_path / "report.json"
    result = render_book(
        rules_path,
        manifest_path,
        master_path,
        report_path,
        tmp_path / "state",
    )
    assert result["status"] == "complete"
    report = json.loads(report_path.read_text())
    assert report["validation"]["all_checks_passed"] is True
    assert abs(report["loudness"]["after_lufs"] - (-18.0)) <= 0.10
    assert len(report["chapters"]) == 3

    chapter_loudness = [row["after_loudness_lufs"] for row in report["chapters"]]
    assert max(chapter_loudness) - min(chapter_loudness) <= 0.05
    for chapter in report["chapters"]:
        assert chapter["noise_floor_rise_db"] <= 6.0
        assert chapter["true_peak_after_dbtp"] <= -1.0 + 1e-6
    assert sum(
        chapter["peak_protection_samples"] for chapter in report["chapters"]
    ) > 0
    for boundary in report["boundaries"]:
        assert abs(boundary["delta_db"]) <= 0.5 + 1e-6
        assert boundary["within_tolerance"] is True

    audio = read_wav(master_path)
    expected_frames = sum(
        round(value * 48000) for value in (1.7, 1.5, 1.9)
    )
    assert audio.info.frames == expected_frames
    assert audio.info.header_bytes == 44
    assert audio.info.data_bytes == expected_frames * 2 * 3
    assert audio.info.file_size == 44 + audio.info.data_bytes
    assert np.max(np.abs(audio.samples)) <= 10.0 ** (-1.0 / 20.0) + 1e-12
    assert true_peak_dbtp(audio.samples) <= -1.0 + 1e-6
    assert (
        abs(
            integrated_loudness(
                audio.samples, 48000, relative_gate_lu=-10.0
            )
            - report["loudness"]["after_lufs"]
        )
        <= 1e-6
    )
    for chapter in report["chapters"]:
        segment = audio.samples[
            chapter["start_frame"] : chapter["end_frame"]
        ]
        assert (
            abs(
                noise_floor_dbfs(segment, 48000)
                - chapter["noise_floor_after_dbfs"]
            )
            <= 1e-6
        )

    # Read the canonical RIFF header independently.
    raw = master_path.read_bytes()
    assert struct.unpack_from("<I", raw, 4)[0] + 8 == len(raw)
    assert struct.unpack_from("<I", raw, 40)[0] == len(raw) - 44
    assert not any(key.lower().endswith("timestamp") for key in _all_keys(report))
    assert not any(value.startswith("/") for value in _all_strings(report))


def test_interrupt_resume_and_corrupt_snapshot_are_byte_identical(
    tmp_path: Path,
) -> None:
    rules_path, manifest_path = _make_book(tmp_path)
    baseline = tmp_path / "baseline.wav"
    render_book(
        rules_path,
        manifest_path,
        baseline,
        tmp_path / "baseline.json",
        tmp_path / "baseline-state",
    )

    state_dir = tmp_path / "resume-state"
    interrupted_master = tmp_path / "interrupted.wav"
    interrupted_report_path = tmp_path / "interrupted.json"
    interrupted = render_book(
        rules_path,
        manifest_path,
        interrupted_master,
        interrupted_report_path,
        state_dir,
        interrupt_after_blocks=5,
    )
    assert interrupted["status"] == "interrupted"
    assert interrupted["business_code"] == "E_INTERRUPTED"
    assert not interrupted_master.exists()
    interrupted_report = json.loads(interrupted_report_path.read_text())
    assert interrupted_report["status"] == "interrupted"
    snapshot = json.loads((state_dir / "snapshot.json").read_text())
    assert snapshot["payload"]["completed_blocks"] == 5
    assert snapshot["payload"]["rule_hash"]
    assert snapshot["payload"]["input_digest"]

    # An uncommitted tail is legal after a hard kill and must be truncated.
    with (state_dir / "render.pcm").open("ab") as handle:
        handle.write(b"\x7f" * 317)

    resumed = tmp_path / "resumed.wav"
    resumed_result = render_book(
        rules_path,
        manifest_path,
        resumed,
        tmp_path / "resumed.json",
        state_dir,
    )
    assert resumed_result["recovery"]["source"] == "resumed"
    assert resumed_result["recovery"]["resumed_blocks"] == 5
    assert _file_hash(baseline) == _file_hash(resumed)

    (state_dir / "snapshot.json").write_text("{corrupt", encoding="utf-8")
    recovered = tmp_path / "recovered.wav"
    recovered_result = render_book(
        rules_path,
        manifest_path,
        recovered,
        tmp_path / "recovered.json",
        state_dir,
    )
    assert (
        recovered_result["recovery"]["source"]
        == "recovered_from_corrupt_snapshot"
    )
    assert _file_hash(baseline) == _file_hash(recovered)


def test_resume_mismatch_discards_state_and_rerenders(tmp_path: Path) -> None:
    rules_path, manifest_path = _make_book(tmp_path)
    state_dir = tmp_path / "state"
    render_book(
        rules_path,
        manifest_path,
        tmp_path / "first.wav",
        tmp_path / "first.json",
        state_dir,
        interrupt_after_blocks=4,
    )
    rules = json.loads(rules_path.read_text())
    rules["target_loudness_lufs"] = -19.0
    _write_json(rules_path, rules)

    mismatch_master = tmp_path / "mismatch.wav"
    mismatch_report_path = tmp_path / "mismatch.json"
    result = render_book(
        rules_path,
        manifest_path,
        mismatch_master,
        mismatch_report_path,
        state_dir,
    )
    assert result["business_code"] == "E_RESUME_MISMATCH"
    report = json.loads(mismatch_report_path.read_text())
    assert report["recovery"]["source"] == "restarted_after_mismatch"
    assert report["recovery"]["resumed_blocks"] == 0
    assert report["loudness"]["target_lufs"] == -19.0

    fresh_master = tmp_path / "fresh.wav"
    render_book(
        rules_path,
        manifest_path,
        fresh_master,
        tmp_path / "fresh.json",
        tmp_path / "fresh-state",
    )
    assert _file_hash(mismatch_master) == _file_hash(fresh_master)


def test_sigterm_is_cooperative_and_resumable(tmp_path: Path) -> None:
    rules_path, manifest_path = _make_book(
        tmp_path,
        durations=(2.4, 2.2, 2.6, 2.3),
        batches=("light", "compressed", "mic_b", "normal"),
        block_frames=512,
    )
    master_path = tmp_path / "signal.wav"
    report_path = tmp_path / "signal.json"
    state_dir = tmp_path / "signal-state"
    command = [
        sys.executable,
        "-m",
        "audiobook_master",
        "render",
        "--rules",
        str(rules_path),
        "--manifest",
        str(manifest_path),
        "--out-master",
        str(master_path),
        "--out-report",
        str(report_path),
        "--state-dir",
        str(state_dir),
    ]
    process = subprocess.Popen(
        command,
        cwd=PROJECT_ROOT,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    deadline = time.monotonic() + 30.0
    snapshot_path = state_dir / "snapshot.json"
    while time.monotonic() < deadline and not snapshot_path.exists():
        if process.poll() is not None:
            break
        time.sleep(0.02)
    assert snapshot_path.exists(), process.communicate(timeout=5)
    process.send_signal(signal.SIGTERM)
    stdout, stderr = process.communicate(timeout=40)
    assert process.returncode == 0, (stdout, stderr)
    assert not master_path.exists()
    interrupted_report = json.loads(report_path.read_text())
    assert interrupted_report["status"] == "interrupted"
    assert interrupted_report["business_code"] == "E_INTERRUPTED"
    assert interrupted_report["progress"]["completed_blocks"] >= 1

    resumed = render_book(
        rules_path,
        manifest_path,
        master_path,
        report_path,
        state_dir,
    )
    assert resumed["status"] == "complete"
    assert resumed["validation"]["all_checks_passed"] is True

    baseline = tmp_path / "signal-baseline.wav"
    render_book(
        rules_path,
        manifest_path,
        baseline,
        tmp_path / "signal-baseline.json",
        tmp_path / "signal-baseline-state",
    )
    assert _file_hash(master_path) == _file_hash(baseline)
