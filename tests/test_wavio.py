from __future__ import annotations

import struct
from pathlib import Path

import numpy as np
import pytest

from broadcast_loudness.errors import FormatUnsupportedError
from broadcast_loudness.synth import synthesize_signal
from broadcast_loudness.wavio import make_wav_bytes, read_wav, write_wav


@pytest.mark.parametrize("bit_depth", [16, 24])
@pytest.mark.parametrize("channels", [1, 2, 6])
def test_pcm_round_trip(tmp_path: Path, bit_depth: int, channels: int) -> None:
    samples = synthesize_signal(3, channels, 0.8)
    first = tmp_path / "first.wav"
    second = tmp_path / "second.wav"
    write_wav(first, samples, bit_depth)
    write_wav(second, samples, bit_depth)
    assert first.read_bytes() == second.read_bytes()
    audio = read_wav(first)
    assert audio.sample_rate == 48_000
    assert audio.bit_depth == bit_depth
    assert audio.channels == channels
    assert audio.frame_count == samples.shape[0]
    assert (
        audio.layout
        == {
            1: ("M",),
            2: ("L", "R"),
            6: ("L", "R", "C", "LFE", "BL", "BR"),
        }[channels]
    )
    assert np.max(np.abs(audio.samples - samples)) < (
        2.0 / 32768.0 if bit_depth == 16 else 2.0 / 8388608.0
    )
    start, end = 4800, 19200
    assert audio.content_hash(start, end) == audio.content_hash(start, end)
    assert audio.sync_marker(start, end).endswith(f"{start:016d}:{end:016d}")


@pytest.mark.parametrize(
    ("offset", "value", "code"),
    [
        (24, 44_100, "E_FORMAT_UNSUPPORTED"),
        (34, 32, "E_FORMAT_UNSUPPORTED"),
    ],
)
def test_unsupported_header_returns_stable_code(
    tmp_path: Path,
    offset: int,
    value: int,
    code: str,
) -> None:
    payload = bytearray(make_wav_bytes(np.zeros((4800, 2)), 24))
    struct.pack_into("<H", payload, offset, value)
    path = tmp_path / "unsupported.wav"
    path.write_bytes(payload)
    with pytest.raises(FormatUnsupportedError) as error:
        read_wav(path)
    assert error.value.code == code


def test_rejects_seven_channels(tmp_path: Path) -> None:
    payload = bytearray(make_wav_bytes(np.zeros((4800, 2)), 16))
    struct.pack_into("<H", payload, 22, 7)
    path = tmp_path / "seven.wav"
    path.write_bytes(payload)
    with pytest.raises(FormatUnsupportedError) as error:
        read_wav(path)
    assert error.value.code == "E_FORMAT_UNSUPPORTED"
