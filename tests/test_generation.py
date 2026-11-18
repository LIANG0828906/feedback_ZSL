from __future__ import annotations

import hashlib
from pathlib import Path

import numpy as np
import soundfile as sf

from choir_master.generate import init_workspace, mutation_window


def _sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def test_generation_is_byte_deterministic(workspace: Path, tmp_path: Path) -> None:
    second = tmp_path / "second"
    init_workspace(
        second,
        seed=235,
        duration_seconds=4.0,
        block_frames=12000,
        target_loudness_lufs=-20.0,
        true_peak_limit_dbtp=-1.0,
    )
    first_wavs = sorted((workspace / "audio").glob("*.wav"))
    second_wavs = sorted((second / "audio").glob("*.wav"))
    assert len(first_wavs) == len(second_wavs) == 8
    assert [path.name for path in first_wavs] == [path.name for path in second_wavs]
    assert [_sha(path) for path in first_wavs] == [_sha(path) for path in second_wavs]
    assert _sha(workspace / "project.json") == _sha(second / "project.json")


def test_generated_material_matches_declared_truth(workspace: Path) -> None:
    with open(workspace / "project.json", "r", encoding="utf-8") as handle:
        manifest = __import__("json").load(handle)
    truth = manifest["generation"]["mutation_intervals_s"][0]
    assert truth == [round(value, 9) for value in mutation_window(235, 4.0)]
    tenor_a, sample_rate = sf.read(workspace / "audio/ten_a.wav")
    tenor_b, sample_rate_b = sf.read(workspace / "audio/ten_b.wav")
    assert sample_rate == sample_rate_b == 48000
    assert tenor_a.shape == tenor_b.shape
    assert np.any(tenor_a != 0.0)
    # Mic B is a deterministic attenuation and integer-delay view of mic A.
    ten_b = next(item for item in manifest["microphones"] if item["name"] == "ten_b")
    b_gain = 10.0 ** (ten_b["position"]["gain_db"] / 20.0)
    delay = int(round(ten_b["position"]["delay_ms"] * 48.0))
    assert np.allclose(tenor_b[delay:], b_gain * tenor_a[:-delay], atol=2e-7)

