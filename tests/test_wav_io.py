from __future__ import annotations

import struct
from pathlib import Path

import numpy as np
import pytest

from game_audio_restorer.errors import BusinessError, ErrorCode
from game_audio_restorer.wav_io import encode_wav, read_wav


@pytest.mark.parametrize("sample_rate", [8000, 16000, 44100, 48000])
@pytest.mark.parametrize("channels", [1, 2])
def test_read_supported_layouts(tmp_path: Path, sample_rate: int, channels: int) -> None:
    frames = 400
    source = np.linspace(-0.5, 0.5, frames * channels, dtype=np.float64).reshape(
        frames, channels
    )
    path = tmp_path / f"{sample_rate}_{channels}.wav"
    path.write_bytes(encode_wav(source, sample_rate))
    audio = read_wav(path)
    assert audio.sample_rate == sample_rate
    assert audio.channels == channels
    assert audio.frames == frames
    assert np.max(np.abs(audio.samples - source)) <= 1.0 / 32768.0


def _expect(path: Path, field: str) -> BusinessError:
    with pytest.raises(BusinessError) as caught:
        read_wav(path)
    error = caught.value
    assert error.code == ErrorCode.WAV_INVALID
    assert error.details["location"]["field"] == field
    assert "byte_offset" in error.details["location"]
    return error


def test_riff_length_mismatch(tmp_path: Path) -> None:
    path = tmp_path / "riff.wav"
    raw = bytearray(encode_wav(np.zeros((10, 1)), 8000))
    struct.pack_into("<I", raw, 4, len(raw) - 4)
    path.write_bytes(raw)
    _expect(path, "riff_size")


def test_illegal_channel_count(tmp_path: Path) -> None:
    path = tmp_path / "channels.wav"
    raw = bytearray(encode_wav(np.zeros((10, 2)), 16000))
    struct.pack_into("<H", raw, 22, 3)
    path.write_bytes(raw)
    _expect(path, "channels")


def test_sample_rate_out_of_range(tmp_path: Path) -> None:
    path = tmp_path / "rate.wav"
    raw = bytearray(encode_wav(np.zeros((10, 1)), 8000))
    struct.pack_into("<I", raw, 24, 7999)
    struct.pack_into("<I", raw, 28, 7999 * 2)
    path.write_bytes(raw)
    _expect(path, "sample_rate")


def test_non_pcm_and_bit_depth(tmp_path: Path) -> None:
    path = tmp_path / "format.wav"
    raw = bytearray(encode_wav(np.zeros((10, 1)), 8000))
    struct.pack_into("<H", raw, 20, 3)
    path.write_bytes(raw)
    _expect(path, "audio_format")

    path2 = tmp_path / "bits.wav"
    raw = bytearray(encode_wav(np.zeros((10, 1)), 8000))
    struct.pack_into("<H", raw, 34, 8)
    path2.write_bytes(raw)
    _expect(path2, "bits_per_sample")


def test_byte_rate_mismatch(tmp_path: Path) -> None:
    path = tmp_path / "byterate.wav"
    raw = bytearray(encode_wav(np.zeros((10, 2)), 16000))
    struct.pack_into("<I", raw, 28, 12345)
    path.write_bytes(raw)
    _expect(path, "byte_rate")


def test_truncated_data_chunk(tmp_path: Path) -> None:
    path = tmp_path / "truncated.wav"
    raw = bytearray(encode_wav(np.zeros((100, 2)), 16000))
    raw = raw[:-37]
    struct.pack_into("<I", raw, 4, len(raw) - 8)
    path.write_bytes(raw)
    _expect(path, "chunk_size")


def test_data_length_not_whole_frame(tmp_path: Path) -> None:
    path = tmp_path / "odddata.wav"
    raw = bytearray(encode_wav(np.zeros((100, 2)), 16000))
    data_size_offset = raw.index(b"data") + 4
    declared = struct.unpack_from("<I", raw, data_size_offset)[0]
    struct.pack_into("<I", raw, data_size_offset, declared - 2)
    struct.pack_into("<I", raw, 4, len(raw) - 8 - 2)
    raw = raw[:-2]
    path.write_bytes(raw)
    _expect(path, "data_size")


def test_missing_file(tmp_path: Path) -> None:
    _expect(tmp_path / "missing.wav", "file")
