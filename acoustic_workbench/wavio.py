"""Deterministic 16-bit PCM WAV reading and atomic writing."""

from __future__ import annotations

import math
import os
import struct
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Optional, Sequence

import numpy as np

from .codes import AcousticError, E_IO, E_IR_INVALID
from .util import sha256_bytes, sha256_file


@dataclass
class WavData:
    samples: np.ndarray
    sample_rate: int
    bits_per_sample: int
    audio_format: int
    channel_mask: Optional[int]
    raw_sha256: str
    path: str

    @property
    def frame_count(self) -> int:
        return int(self.samples.shape[0])

    @property
    def channel_count(self) -> int:
        return int(self.samples.shape[1])


def _read_chunks(data: bytes) -> list[tuple[bytes, bytes]]:
    if len(data) < 12 or data[:4] != b"RIFF" or data[8:12] != b"WAVE":
        raise AcousticError(
            E_IR_INVALID,
            "IR is not a RIFF/WAVE file.",
            injection_point="wav:header",
        )
    chunks: list[tuple[bytes, bytes]] = []
    offset = 12
    while offset + 8 <= len(data):
        chunk_id = data[offset : offset + 4]
        chunk_size = struct.unpack_from("<I", data, offset + 4)[0]
        start = offset + 8
        end = start + chunk_size
        if end > len(data):
            raise AcousticError(
                E_IR_INVALID,
                "WAV chunk exceeds file length.",
                injection_point=f"wav:chunk:{chunk_id.decode('ascii', 'replace')}",
            )
        chunks.append((chunk_id, data[start:end]))
        offset = end + (chunk_size & 1)
    return chunks


def read_wav_16(path: str | os.PathLike[str]) -> WavData:
    try:
        raw = Path(path).read_bytes()
    except OSError as exc:
        raise AcousticError(
            E_IO,
            f"Cannot read WAV file: {exc}",
            injection_point=f"wav:{path}",
        ) from exc
    chunks = _read_chunks(raw)
    fmt = next((payload for chunk_id, payload in chunks if chunk_id == b"fmt "), None)
    data = next((payload for chunk_id, payload in chunks if chunk_id == b"data"), None)
    if fmt is None or data is None:
        raise AcousticError(
            E_IR_INVALID,
            "WAV must contain fmt and data chunks.",
            injection_point="wav:chunks",
        )
    if len(fmt) < 16:
        raise AcousticError(
            E_IR_INVALID,
            "WAV fmt chunk is too short.",
            injection_point="wav:fmt",
        )
    audio_format, channels, sample_rate, _, block_align, bits = struct.unpack_from(
        "<HHIIHH", fmt, 0
    )
    channel_mask: Optional[int] = None
    if audio_format == 0xFFFE:
        if len(fmt) < 40:
            raise AcousticError(
                E_IR_INVALID,
                "Extensible WAV fmt chunk is too short.",
                injection_point="wav:fmt:extensible",
            )
        valid_bits = struct.unpack_from("<H", fmt, 18)[0]
        channel_mask = struct.unpack_from("<I", fmt, 20)[0]
        subformat = struct.unpack_from("<H", fmt, 24)[0]
        if valid_bits != 16 or subformat != 1:
            raise AcousticError(
                E_IR_INVALID,
                "Only 16-bit integer PCM IRs are supported.",
                injection_point="wav:fmt:subformat",
            )
        audio_format = subformat
    if audio_format != 1 or bits != 16:
        raise AcousticError(
            E_IR_INVALID,
            "Only 16-bit PCM WAV input is supported.",
            injection_point="wav:fmt",
            details={"audio_format": audio_format, "bits_per_sample": bits},
        )
    if channels <= 0 or block_align != channels * 2:
        raise AcousticError(
            E_IR_INVALID,
            "Invalid WAV channel count or block alignment.",
            injection_point="wav:fmt:block_align",
        )
    if len(data) % block_align:
        raise AcousticError(
            E_IR_INVALID,
            "WAV data chunk is not an integral number of frames.",
            injection_point="wav:data",
        )
    integers = np.frombuffer(data, dtype="<i2").reshape(-1, channels)
    samples = integers.astype(np.float64) / 32768.0
    return WavData(
        samples=samples,
        sample_rate=sample_rate,
        bits_per_sample=bits,
        audio_format=audio_format,
        channel_mask=channel_mask,
        raw_sha256=sha256_bytes(raw),
        path=str(path),
    )


_PCM_SUBFORMAT = struct.pack("<IHH8s", 1, 0, 0x0010, b"\x80\x00\x00\xaa\x00\x38\x9b\x71")


def wav_bytes_16(samples: np.ndarray, sample_rate: int, channel_mask: int = 0x3F) -> bytes:
    array = np.asarray(samples)
    if array.ndim != 2:
        raise ValueError("WAV samples must be a two-dimensional array")
    if not np.isfinite(array).all():
        raise ValueError("WAV samples contain non-finite values")
    channels = int(array.shape[1])
    if channels <= 0:
        raise ValueError("WAV must have at least one channel")
    if array.dtype.kind == "f":
        integers = np.clip(np.rint(array * 32767.0), -32768, 32767).astype("<i2")
    else:
        integers = np.clip(array, -32768, 32767).astype("<i2")
    payload = integers.tobytes(order="C")
    bits = 16
    block_align = channels * 2
    byte_rate = sample_rate * block_align
    fmt = struct.pack(
        "<HHIIHHHHI",
        0xFFFE,
        channels,
        sample_rate,
        byte_rate,
        block_align,
        bits,
        22,
        bits,
        channel_mask & 0xFFFFFFFF,
    ) + _PCM_SUBFORMAT
    data_chunk = b"data" + struct.pack("<I", len(payload)) + payload
    if len(payload) & 1:
        data_chunk += b"\x00"
    fmt_chunk = b"fmt " + struct.pack("<I", len(fmt)) + fmt
    riff_size = 4 + len(fmt_chunk) + len(data_chunk)
    return b"RIFF" + struct.pack("<I", riff_size) + b"WAVE" + fmt_chunk + data_chunk


def write_wav_16_atomic(
    path: str | os.PathLike[str],
    samples: np.ndarray,
    sample_rate: int,
    channel_mask: int = 0x3F,
) -> str:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    raw = wav_bytes_16(samples, sample_rate, channel_mask)
    fd, temp_name = tempfile.mkstemp(prefix=f".{target.name}.", dir=str(target.parent))
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(raw)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, target)
    except BaseException:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise
    return sha256_bytes(raw)

