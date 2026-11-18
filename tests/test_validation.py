from __future__ import annotations

import json
import subprocess
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf

from orchestral_mastering.errors import BusinessError
from orchestral_mastering.manifest import load_manifest

from conftest import absolutize_manifest, python_executable, write_manifest


def _base_manifest(demo_root: Path, tmp_path: Path) -> tuple[Path, dict]:
    source = demo_root / "manifest.json"
    destination = tmp_path / "manifest.json"
    manifest = absolutize_manifest(source, destination)
    return destination, manifest


def test_missing_file_reports_structured_business_error(
    demo_root: Path, tmp_path: Path, cli_env: dict[str, str]
) -> None:
    manifest_path, manifest = _base_manifest(demo_root, tmp_path)
    manifest["tracks"][0]["path"] = str(tmp_path / "does_not_exist.wav")
    write_manifest(manifest, manifest_path)
    with pytest.raises(BusinessError) as raised:
        load_manifest(manifest_path)
    issue = raised.value.issues[0]
    assert issue.code == "E_MISSING_FILE"
    assert issue.channel == "弦乐组"
    assert issue.group == "弦乐"

    completed = subprocess.run(
        [
            python_executable(),
            "-m",
            "orchestral_mastering",
            "validate",
            "--manifest",
            str(manifest_path),
        ],
        cwd=Path(__file__).resolve().parents[1],
        env=cli_env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert completed.returncode == 2
    payload = json.loads(completed.stderr)
    assert payload["ok"] is False
    assert payload["errors"][0]["code"] == "E_MISSING_FILE"
    assert payload["errors"][0]["channel"] == "弦乐组"


def test_format_mismatch_detects_sample_rate(
    demo_root: Path, tmp_path: Path, cli_env: dict[str, str]
) -> None:
    manifest_path, manifest = _base_manifest(demo_root, tmp_path)
    source = Path(manifest["tracks"][0]["path"])
    data, rate = sf.read(str(source), dtype="float32")
    bad_path = tmp_path / "wrong_rate.wav"
    sf.write(str(bad_path), data, 44100, format="WAV", subtype="PCM_24")
    manifest["tracks"][0]["path"] = str(bad_path)
    write_manifest(manifest, manifest_path)

    with pytest.raises(BusinessError) as raised:
        load_manifest(manifest_path)
    mismatches = [
        issue
        for issue in raised.value.issues
        if issue.code == "E_FORMAT_MISMATCH" and issue.channel == "弦乐组"
    ]
    assert mismatches
    details = mismatches[0].details["mismatches"]
    assert details["sample_rate"] == {"declared": 48000, "actual": 44100}

    completed = subprocess.run(
        [
            python_executable(),
            "-m",
            "orchestral_mastering",
            "validate",
            "--manifest",
            str(manifest_path),
        ],
        cwd=Path(__file__).resolve().parents[1],
        env=cli_env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert completed.returncode == 2
    payload = json.loads(completed.stderr)
    assert any(item["code"] == "E_FORMAT_MISMATCH" for item in payload["errors"])


@pytest.mark.parametrize(
    ("mutation", "channel"),
    [
        (lambda manifest: manifest["tracks"][1].update({"group": "弦乐"}), "木管组"),
        (lambda manifest: manifest["tracks"][0].update({"target_energy_ratio": 0.95}), "manifest"),
        (
            lambda manifest: manifest["tracks"][1].update(
                {"onset_reference_seconds": manifest["tracks"][0]["onset_reference_seconds"]}
            ),
            "木管组",
        ),
        (lambda manifest: manifest["tracks"][5].update({"group": "未知声部"}), "指挥提示"),
    ],
)
def test_invalid_manifest_rules(
    demo_root: Path,
    tmp_path: Path,
    mutation,
    channel: str,
) -> None:
    manifest_path, manifest = _base_manifest(demo_root, tmp_path)
    mutation(manifest)
    write_manifest(manifest, manifest_path)
    with pytest.raises(BusinessError) as raised:
        load_manifest(manifest_path)
    assert any(issue.code == "E_INVALID_MANIFEST" for issue in raised.value.issues)
    assert any(issue.channel == channel for issue in raised.value.issues)


def test_detection_failure_is_isolated(demo_root: Path) -> None:
    from orchestral_mastering.alignment import align_stems
    from orchestral_mastering.pipeline import _load_sources

    manifest = load_manifest(demo_root / "manifest.json")
    stems, references = _load_sources(manifest)
    baseline = align_stems(manifest, stems, references)
    silent = dict(stems)
    silent["木管"] = np.zeros_like(stems["木管"])
    changed = align_stems(manifest, silent, references)
    failed = [
        item
        for item in changed.warnings
        if item["code"] == "DETECTION_FAILED" and item["channel"] == "木管组"
    ]
    assert failed
    for role in stems:
        if role == "木管":
            assert changed.stems[role].shift_samples == 0
        else:
            assert changed.stems[role].shift_samples == baseline.stems[role].shift_samples

