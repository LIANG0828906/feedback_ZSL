from __future__ import annotations

import struct

import numpy as np
import pytest

from mastering.wavio import encode_wav, inspect_wav, quantize_samples, read_wav, write_wav_atomic


@pytest.mark.parametrize("bits", [16, 24, 32])
def test_wav_roundtrip_and_header_consistency(tmp_path, bits):
    frames = 17
    channels = 2
    samples = np.linspace(-1.0, 1.0, frames * channels, dtype=np.float64).reshape(
        frames, channels
    )
    path = tmp_path / f"roundtrip_{bits}.wav"
    write_wav_atomic(path, samples, 44100, bits)

    info = inspect_wav(path)
    assert info.sample_rate == 44100
    assert info.channels == channels
    assert info.bits_per_sample == bits
    assert info.frames == frames
    assert info.data_bytes == frames * channels * (bits // 8)
    assert info.format_tag == (3 if bits == 32 else 1)

    raw = path.read_bytes()
    riff, riff_size, wave = struct.unpack("<4sI4s", raw[:12])
    assert riff == b"RIFF"
    assert wave == b"WAVE"
    assert riff_size == len(raw) - 8
    data_index = raw.index(b"data")
    data_size = struct.unpack("<I", raw[data_index + 4 : data_index + 8])[0]
    assert data_size == info.data_bytes
    assert data_index + 8 + data_size <= len(raw)
    assert len(raw) - (data_index + 8 + data_size) in (0, 1)

    decoded, decoded_info = read_wav(path)
    assert decoded_info == info
    assert decoded.shape == (frames, channels)
    assert np.max(np.abs(decoded - quantize_samples(samples, bits))) <= 1e-12


def test_24_bit_is_little_endian_three_byte_pcm(tmp_path):
    samples = np.array([[1.0, -1.0], [0.5, 0.0]], dtype=np.float64)
    path = tmp_path / "little_endian.wav"
    write_wav_atomic(path, samples, 48000, 24)
    raw = path.read_bytes()
    data_index = raw.index(b"data") + 8
    first_positive = raw[data_index : data_index + 3]
    first_negative = raw[data_index + 3 : data_index + 6]
    assert list(first_positive) == [0xFF, 0xFF, 0x7F]
    assert list(first_negative) == [0x00, 0x00, 0x80]


def test_encoding_is_byte_deterministic():
    rng = np.random.default_rng(7)
    samples = rng.uniform(-1.2, 1.2, (1000, 2))
    for bits in (16, 24, 32):
        first = encode_wav(samples, 48000, bits)[0]
        second = encode_wav(samples, 48000, bits)[0]
        assert first == second


def test_integer_quantization_clamps_and_rounds():
    values = np.array([[-2.0], [-1.0], [0.0], [1.0], [2.0]])
    quantized = quantize_samples(values, 16)
    assert quantized[0, 0] == -1.0
    assert quantized[1, 0] == -1.0
    assert quantized[2, 0] == 0.0
    assert quantized[3, 0] == 32767 / 32768
    assert quantized[4, 0] == 32767 / 32768
