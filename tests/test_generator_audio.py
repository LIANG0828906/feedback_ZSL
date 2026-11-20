from __future__ import annotations

from pathlib import Path

import numpy as np
import pytest

from repair_app.audio import inspect_wav, read_wav, sha256_file
from repair_app.errors import AppError
from repair_app.generator import generate_sample_file


@pytest.mark.parametrize(
    ("rate", "channels"),
    [(8_000, 1), (16_000, 2), (44_100, 1), (48_000, 2)],
)
def test_generator_is_byte_deterministic(tmp_path: Path, rate: int, channels: int) -> None:
    first = tmp_path / f"first-{rate}-{channels}.wav"
    second = tmp_path / f"second-{rate}-{channels}.wav"
    generate_sample_file(first, sample_rate=rate, channels=channels, seconds=0.5, seed=987)
    generate_sample_file(second, sample_rate=rate, channels=channels, seconds=0.5, seed=987)
    assert first.read_bytes() == second.read_bytes()
    assert sha256_file(first) == sha256_file(second)
    samples, info = read_wav(first)
    assert info.sample_rate == rate
    assert info.channels == channels
    assert samples.dtype == np.int16
    assert samples.shape == (int(round(0.5 * rate)), channels)


def test_generator_rejects_invalid_sample_rate(tmp_path: Path) -> None:
    with pytest.raises(AppError) as error:
        generate_sample_file(tmp_path / "bad.wav", sample_rate=4_000)
    assert error.value.code == "E_INVALID_INPUT"


def test_wav_header_and_size_are_consistent(tmp_path: Path) -> None:
    path = tmp_path / "sample.wav"
    generate_sample_file(path, sample_rate=48_000, channels=2, seconds=0.25, seed=7)
    info = inspect_wav(path)
    assert path.stat().st_size == 44 + info.frames * info.channels * 2
    data = path.read_bytes()
    assert data[:4] == b"RIFF"
    assert data[8:12] == b"WAVE"
    assert data[36:40] == b"data"

