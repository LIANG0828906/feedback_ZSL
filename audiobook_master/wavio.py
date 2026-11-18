"""Minimal deterministic PCM WAV reader/writer.

Only integer PCM is accepted.  The writer emits a canonical 44-byte RIFF/WAVE
header so frame count, block alignment and data length are self-consistent.
"""

from __future__ import annotations

import os
import struct
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import BinaryIO

import numpy as np

from .errors import BusinessError
from .util import _fsync_directory


@dataclass(slots=True, frozen=True)
class WavInfo:
    sample_rate: int
    channels: int
    bit_depth: int
    frames: int
    data_bytes: int
    header_bytes: int
    file_size: int


@dataclass(slots=True)
class WavAudio:
    samples: np.ndarray
    info: WavInfo


def read_wav(path: Path, *, field_path: str | None = None) -> WavAudio:
    try:
        data = path.read_bytes()
    except OSError as exc:
        raise BusinessError(
            "E_INPUT_INVALID", f"cannot read WAV package: {exc}", field_path
        ) from exc
    if len(data) < 12 or data[:4] != b"RIFF" or data[8:12] != b"WAVE":
        raise BusinessError(
            "E_INPUT_INVALID", "file is not a RIFF/WAVE stream", field_path
        )
    riff_size = struct.unpack_from("<I", data, 4)[0]
    if riff_size + 8 != len(data):
        raise BusinessError(
            "E_INPUT_INVALID",
            f"RIFF length is {riff_size + 8}, file length is {len(data)}",
            field_path,
        )

    offset = 12
    fmt: tuple[int, int, int, int, int, int] | None = None
    data_offset: int | None = None
    data_size: int | None = None
    while offset + 8 <= len(data):
        chunk_id = data[offset : offset + 4]
        chunk_size = struct.unpack_from("<I", data, offset + 4)[0]
        body = offset + 8
        end = body + chunk_size
        if end > len(data):
            raise BusinessError(
                "E_INPUT_INVALID", "truncated WAV chunk", field_path
            )
        if chunk_id == b"fmt ":
            if chunk_size < 16:
                raise BusinessError(
                    "E_INPUT_INVALID", "short fmt chunk", field_path
                )
            fmt = struct.unpack_from("<HHIIHH", data, body)
        elif chunk_id == b"data":
            data_offset = body
            data_size = chunk_size
            break
        offset = end + (chunk_size & 1)

    if fmt is None or data_offset is None or data_size is None:
        raise BusinessError(
            "E_INPUT_INVALID", "WAV requires fmt and data chunks", field_path
        )
    audio_format, channels, sample_rate, byte_rate, block_align, bits = fmt
    if audio_format not in (1, 0xFFFE):
        raise BusinessError(
            "E_INPUT_INVALID",
            f"unsupported WAV format code {audio_format}",
            field_path,
        )
    if channels < 1 or sample_rate < 1:
        raise BusinessError(
            "E_INPUT_INVALID", "invalid channel count or sample rate", field_path
        )
    if bits not in (16, 24, 32):
        raise BusinessError(
            "E_INPUT_INVALID",
            f"unsupported PCM bit depth {bits}",
            field_path,
        )
    bytes_per_sample = bits // 8
    expected_align = channels * bytes_per_sample
    if block_align != expected_align:
        raise BusinessError(
            "E_INPUT_INVALID",
            f"block alignment {block_align} does not match {expected_align}",
            field_path,
        )
    if byte_rate != sample_rate * block_align:
        raise BusinessError(
            "E_INPUT_INVALID",
            "WAV byte rate is inconsistent with sample rate and block alignment",
            field_path,
        )
    if data_size % block_align != 0:
        raise BusinessError(
            "E_INPUT_INVALID", "data chunk is not frame aligned", field_path
        )

    raw = data[data_offset : data_offset + data_size]
    frames = data_size // block_align
    samples = _decode_pcm(raw, bits, channels)
    info = WavInfo(
        sample_rate=sample_rate,
        channels=channels,
        bit_depth=bits,
        frames=frames,
        data_bytes=data_size,
        header_bytes=data_offset,
        file_size=len(data),
    )
    return WavAudio(samples=samples, info=info)


def _decode_pcm(raw: bytes, bits: int, channels: int) -> np.ndarray:
    if bits == 16:
        integers = np.frombuffer(raw, dtype="<i2").reshape(-1, channels)
        return integers.astype(np.float64) / 32768.0
    if bits == 32:
        integers = np.frombuffer(raw, dtype="<i4").reshape(-1, channels)
        return integers.astype(np.float64) / 2147483648.0

    octets = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3)
    unsigned = (
        octets[:, 0].astype(np.int32)
        | (octets[:, 1].astype(np.int32) << 8)
        | (octets[:, 2].astype(np.int32) << 16)
    )
    signed = np.where(unsigned & 0x800000, unsigned - 0x1000000, unsigned)
    return signed.reshape(-1, channels).astype(np.float64) / 8388608.0


def quantize_pcm(samples: np.ndarray, bit_depth: int) -> bytes:
    if bit_depth not in (16, 24, 32):
        raise ValueError(f"unsupported bit depth {bit_depth}")
    values = np.asarray(samples, dtype=np.float64)
    if values.ndim != 2:
        raise ValueError("samples must be a frames x channels array")
    if not np.all(np.isfinite(values)):
        raise ValueError("samples contain non-finite values")
    scale = 1 << (bit_depth - 1)
    integers = np.rint(values * scale)
    integers = np.clip(integers, -scale, scale - 1).astype(np.int64)

    if bit_depth == 16:
        return integers.astype("<i2").tobytes(order="C")
    if bit_depth == 32:
        return integers.astype("<i4").tobytes(order="C")

    unsigned = (integers & 0xFFFFFF).astype(np.uint32)
    packed = np.empty((unsigned.shape[0], unsigned.shape[1], 3), dtype=np.uint8)
    packed[..., 0] = unsigned & 0xFF
    packed[..., 1] = (unsigned >> 8) & 0xFF
    packed[..., 2] = (unsigned >> 16) & 0xFF
    return packed.tobytes(order="C")


def decode_pcm_bytes(raw: bytes, bit_depth: int, channels: int) -> np.ndarray:
    if bit_depth not in (16, 24, 32):
        raise ValueError(f"unsupported bit depth {bit_depth}")
    frame_bytes = channels * (bit_depth // 8)
    if len(raw) % frame_bytes != 0:
        raise ValueError("PCM byte count is not frame aligned")
    return _decode_pcm(raw, bit_depth, channels)


def wav_header(sample_rate: int, channels: int, bit_depth: int, data_bytes: int) -> bytes:
    bytes_per_sample = bit_depth // 8
    block_align = channels * bytes_per_sample
    byte_rate = sample_rate * block_align
    padding = data_bytes & 1
    if data_bytes + padding > 0xFFFFFFFF - 36:
        raise ValueError("PCM data is too large for a canonical RIFF WAV")
    return struct.pack(
        "<4sI4s4sIHHIIHH4sI",
        b"RIFF",
        36 + data_bytes + padding,
        b"WAVE",
        b"fmt ",
        16,
        1,
        channels,
        sample_rate,
        byte_rate,
        block_align,
        bit_depth,
        b"data",
        data_bytes,
    )


def write_wav(
    path: Path,
    samples: np.ndarray,
    *,
    sample_rate: int,
    bit_depth: int,
) -> bytes:
    values = np.asarray(samples, dtype=np.float64)
    if values.ndim != 2:
        raise ValueError("samples must be a frames x channels array")
    payload = quantize_pcm(values, bit_depth)
    header = wav_header(sample_rate, values.shape[1], bit_depth, len(payload))
    padding = b"\x00" if len(payload) & 1 else b""
    atomic_write(path, header + payload + padding)
    return payload


def write_wav_from_pcm(
    path: Path,
    pcm: bytes,
    *,
    sample_rate: int,
    channels: int,
    bit_depth: int,
) -> None:
    header = wav_header(sample_rate, channels, bit_depth, len(pcm))
    padding = b"\x00" if len(pcm) & 1 else b""
    atomic_write(path, header + pcm + padding)


def atomic_write(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.", suffix=".tmp", dir=path.parent
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb", closefd=True) as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        _fsync_directory(path.parent)
    finally:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass


def write_pcm_stream(handle: BinaryIO, samples: np.ndarray, bit_depth: int) -> None:
    handle.write(quantize_pcm(samples, bit_depth))
