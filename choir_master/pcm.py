"""Deterministic PCM encoding and RIFF/WAVE I/O.

The renderer deliberately owns the WAV writer so that output bytes do not
depend on libsndfile version defaults or metadata.
"""

from __future__ import annotations

import os
import struct
from pathlib import Path

import numpy as np


def _atomic_write(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_name(f".{path.name}.tmp-{os.getpid()}")
    with open(temp, "wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temp, path)
    try:
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except OSError:
        pass


def float_to_pcm(audio: np.ndarray, bit_depth: int) -> np.ndarray:
    if bit_depth not in (16, 24):
        raise ValueError("bit_depth must be 16 or 24")
    scale = 1 << (bit_depth - 1)
    clipped = np.clip(np.asarray(audio, dtype=np.float64), -1.0, 1.0 - 1.0 / scale)
    return np.rint(clipped * scale).astype(np.int32)


def pcm_to_float(pcm: np.ndarray, bit_depth: int) -> np.ndarray:
    return np.asarray(pcm, dtype=np.float64) / float(1 << (bit_depth - 1))


def encode_pcm(audio: np.ndarray, bit_depth: int) -> bytes:
    """Encode float audio shaped (frames,) or (frames, channels) as PCM."""
    pcm = float_to_pcm(audio, bit_depth)
    if pcm.ndim == 1:
        pcm = pcm[:, None]
    if bit_depth == 16:
        return pcm.astype("<i2").tobytes(order="C")
    unsigned = pcm.astype(np.int64)
    # ndarray.tofile-like memory order: sample0 byte0/1/2, sample1 byte0/1/2...
    raw = np.empty((unsigned.shape[0], unsigned.shape[1], 3), dtype=np.uint8)
    raw[:, :, 0] = unsigned & 0xFF
    raw[:, :, 1] = (unsigned >> 8) & 0xFF
    raw[:, :, 2] = (unsigned >> 16) & 0xFF
    return raw.tobytes(order="C")


def decode_pcm(data: bytes, bit_depth: int, channels: int) -> np.ndarray:
    if bit_depth == 16:
        pcm = np.frombuffer(data, dtype="<i2").astype(np.int32)
    elif bit_depth == 24:
        raw = np.frombuffer(data, dtype=np.uint8)
        if raw.size % 3:
            raise ValueError("invalid 24-bit PCM byte count")
        triples = raw.reshape(-1, 3).astype(np.int32)
        pcm = triples[:, 0] | (triples[:, 1] << 8) | (triples[:, 2] << 16)
        pcm = np.where(pcm & 0x800000, pcm - 0x1000000, pcm)
    else:
        raise ValueError("bit_depth must be 16 or 24")
    return pcm.reshape(-1, channels)


def wav_bytes(
    pcm_bytes: bytes,
    *,
    sample_rate: int,
    channels: int,
    bit_depth: int,
) -> bytes:
    bytes_per_sample = bit_depth // 8
    block_align = channels * bytes_per_sample
    data_size = len(pcm_bytes)
    if data_size > 0xFFFFFFFF - 36:
        raise ValueError("WAV data exceeds RIFF 32-bit size limit")
    header = struct.pack(
        "<4sI4s4sIHHIIHH4sI",
        b"RIFF",
        36 + data_size,
        b"WAVE",
        b"fmt ",
        16,
        1,
        channels,
        sample_rate,
        sample_rate * block_align,
        block_align,
        bit_depth,
        b"data",
        data_size,
    )
    return header + pcm_bytes


def write_wav(
    path: str | Path,
    audio: np.ndarray,
    *,
    sample_rate: int,
    bit_depth: int,
) -> None:
    array = np.asarray(audio, dtype=np.float64)
    channels = 1 if array.ndim == 1 else int(array.shape[1])
    payload = encode_pcm(array, bit_depth)
    _atomic_write(
        Path(path),
        wav_bytes(
            payload,
            sample_rate=sample_rate,
            channels=channels,
            bit_depth=bit_depth,
        ),
    )


def write_wav_from_blocks(
    path: str | Path,
    blocks: list[Path],
    *,
    sample_rate: int,
    channels: int,
    bit_depth: int,
) -> int:
    """Assemble a WAV atomically from raw interleaved PCM block files."""
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    temp = destination.with_name(f".{destination.name}.tmp-{os.getpid()}")
    data_size = sum(block.stat().st_size for block in blocks)
    block_align = channels * (bit_depth // 8)
    if data_size % block_align:
        raise ValueError("PCM block sizes do not align to complete frames")
    header = wav_bytes(
        b"",
        sample_rate=sample_rate,
        channels=channels,
        bit_depth=bit_depth,
    )
    # Replace the zero data size in the header with the final payload size.
    header = header[:4] + struct.pack("<I", 36 + data_size) + header[8:]
    header = header[:-4] + struct.pack("<I", data_size)
    with open(temp, "wb") as output:
        output.write(header)
        for block in blocks:
            with open(block, "rb") as source:
                while True:
                    chunk = source.read(1024 * 1024)
                    if not chunk:
                        break
                    output.write(chunk)
        output.flush()
        os.fsync(output.fileno())
    os.replace(temp, destination)
    try:
        directory_fd = os.open(destination.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except OSError:
        pass
    return data_size // block_align

