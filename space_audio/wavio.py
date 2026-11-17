"""Minimal deterministic IEEE-float32 / PCM WAV reader and writer.

Only the standard library is used so that rendered bytes do not depend on
third-party WAV encoding behaviour.
"""

from __future__ import annotations

import struct
from pathlib import Path

import numpy as np


def _riff_chunk(chunk_id: bytes, payload: bytes) -> bytes:
    return chunk_id + struct.pack("<I", len(payload)) + payload


def write_wav(path: str | Path, data: np.ndarray, sample_rate: int) -> None:
    """Write mono (n,) or stereo (n, 2) float data as IEEE float32 WAV."""
    array = np.asarray(data, dtype=np.float32)
    if array.ndim == 1:
        channels = 1
        flat = array
    elif array.ndim == 2 and array.shape[1] == 2:
        channels = 2
        flat = array.reshape(-1)
    else:
        raise ValueError("write_wav expects mono (n,) or stereo (n, 2) data")
    byte_rate = sample_rate * channels * 4
    block_align = channels * 4
    fmt = struct.pack(
        "<HHIIHH",
        3,  # WAVE_FORMAT_IEEE_FLOAT
        channels,
        sample_rate,
        byte_rate,
        block_align,
        32,
    )
    payload = flat.tobytes()
    header = b"RIFF" + struct.pack("<I", 4 + (8 + len(fmt)) + (8 + len(payload))) + b"WAVE"
    out = header + _riff_chunk(b"fmt ", fmt) + _riff_chunk(b"data", payload)
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_bytes(out)
    tmp.replace(path)


def read_wav(path: str | Path) -> tuple[np.ndarray, int]:
    """Read a WAV file, returning float64 data and sample rate."""
    path = Path(path)
    raw_bytes = path.read_bytes()
    if raw_bytes[:4] != b"RIFF" or raw_bytes[8:12] != b"WAVE":
        raise ValueError(f"not a RIFF/WAVE file: {path}")
    offset = 12
    fmt_tag = channels = sample_rate = sample_width = None
    data = b""
    while offset + 8 <= len(raw_bytes):
        chunk_id = raw_bytes[offset : offset + 4]
        chunk_size = struct.unpack("<I", raw_bytes[offset + 4 : offset + 8])[0]
        payload_start = offset + 8
        payload_end = payload_start + chunk_size
        if chunk_id == b"fmt ":
            fmt_tag, channels, sample_rate, _byte_rate, _block_align, bits = struct.unpack(
                "<HHIIHH", raw_bytes[payload_start : payload_start + 16]
            )
            sample_width = bits // 8
        elif chunk_id == b"data":
            data = raw_bytes[payload_start:payload_end]
        offset = payload_end + (chunk_size & 1)
    if fmt_tag is None or not data:
        raise ValueError(f"missing fmt/data chunks in {path}")
    if sample_width == 4:
        is_float = fmt_tag == 3
        if is_float:
            audio = np.frombuffer(data, dtype="<f4").astype(np.float64)
        else:
            audio = (np.frombuffer(data, dtype="<i4").astype(np.float64)) / 2147483648.0
    elif sample_width == 2:
        audio = (np.frombuffer(data, dtype="<i2").astype(np.float64)) / 32768.0
    elif sample_width == 3:
        padded = np.zeros((len(data) // 3, 4), dtype=np.uint8)
        bytes_array = np.frombuffer(data, dtype=np.uint8).reshape(-1, 3)
        padded[:, :3] = bytes_array
        ints = np.frombuffer(padded.tobytes(), dtype="<i4")
        audio = ints.astype(np.float64) / 8388608.0
    else:
        raise ValueError(f"unsupported sample width {sample_width} in {path}")
    if channels > 1:
        audio = audio.reshape(-1, channels)
    return audio, sample_rate

