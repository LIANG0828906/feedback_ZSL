from __future__ import annotations

import hashlib
import os
import wave
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .constants import E_BIT_DEPTH, E_CHANNEL_COUNT, E_SAMPLE_RATE, E_WAV_READ
from .errors import Diagnostic, DiagnosticError

PCM_HEADER_BYTES = 44


@dataclass(frozen=True)
class WavInfo:
    sample_rate: int
    bit_depth: int
    channel_count: int
    frames: int


@dataclass(frozen=True)
class WavData:
    samples: np.ndarray
    sample_rate: int
    bit_depth: int


def inspect_wav(path: Path) -> WavInfo:
    try:
        with wave.open(str(path), "rb") as handle:
            if handle.getcomptype() != "NONE":
                raise wave.Error("compressed WAV is not supported")
            return WavInfo(
                sample_rate=handle.getframerate(),
                bit_depth=handle.getsampwidth() * 8,
                channel_count=handle.getnchannels(),
                frames=handle.getnframes(),
            )
    except (OSError, EOFError, wave.Error) as exc:
        raise DiagnosticError([Diagnostic(E_WAV_READ, "manifest", f"无法读取 WAV 头: {exc}")])


def validate_wav_header(
    path: Path,
    channel_id: str,
    expected_rate: int,
    expected_bit_depth: int,
) -> WavInfo:
    try:
        info = inspect_wav(path)
    except DiagnosticError as exc:
        raise DiagnosticError(
            [Diagnostic(E_WAV_READ, channel_id, exc.diagnostics[0].message)]
        ) from None
    problems: list[Diagnostic] = []
    if info.sample_rate != expected_rate:
        problems.append(
            Diagnostic(
                E_SAMPLE_RATE,
                channel_id,
                f"实际采样率 {info.sample_rate} Hz，清单要求 {expected_rate} Hz",
            )
        )
    if info.bit_depth != expected_bit_depth:
        problems.append(
            Diagnostic(
                E_BIT_DEPTH,
                channel_id,
                f"实际位深 {info.bit_depth} bit，清单要求 {expected_bit_depth} bit",
            )
        )
    if info.channel_count != 1:
        problems.append(
            Diagnostic(
                E_CHANNEL_COUNT,
                channel_id,
                f"实际声道数 {info.channel_count}，清单要求单声道",
            )
        )
    if problems:
        raise DiagnosticError(problems)
    return info


def read_wav(path: Path, channel_id: str) -> WavData:
    try:
        with wave.open(str(path), "rb") as handle:
            channels = handle.getnchannels()
            width = handle.getsampwidth()
            rate = handle.getframerate()
            raw = handle.readframes(handle.getnframes())
    except (OSError, EOFError, wave.Error) as exc:
        raise DiagnosticError(
            [Diagnostic(E_WAV_READ, channel_id, f"无法解码 WAV: {exc}")]
        ) from None
    if channels != 1:
        raise DiagnosticError(
            [Diagnostic(E_CHANNEL_COUNT, channel_id, f"实际声道数 {channels}")]
        )
    if width == 2:
        values = np.frombuffer(raw, dtype="<i2").astype(np.float64) / 32768.0
    elif width == 3:
        octets = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3).astype(np.int32)
        values = octets[:, 0] | (octets[:, 1] << 8) | (octets[:, 2] << 16)
        values = np.where(values & 0x800000, values - 0x1000000, values)
        values = values.astype(np.float64) / 8388608.0
    elif width == 4:
        values = np.frombuffer(raw, dtype="<i4").astype(np.float64) / 2147483648.0
    else:
        raise DiagnosticError(
            [Diagnostic(E_BIT_DEPTH, channel_id, f"不支持的 PCM 位深 {width * 8} bit")]
        )
    return WavData(samples=values, sample_rate=rate, bit_depth=width * 8)


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def bytes_sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def wav_header(sample_rate: int, bit_depth: int, channel_count: int, frames: int) -> bytes:
    if bit_depth % 8 or bit_depth not in (16, 24, 32):
        raise ValueError("bit depth must be 16, 24 or 32")
    bytes_per_sample = bit_depth // 8
    block_align = bytes_per_sample * channel_count
    data_bytes = frames * block_align
    if data_bytes > 0xFFFFFFFF - 36:
        raise ValueError("WAV exceeds RIFF32 size limit")
    import struct

    return (
        b"RIFF"
        + struct.pack("<I", 36 + data_bytes)
        + b"WAVE"
        + b"fmt "
        + struct.pack("<IHHIIHH", 16, 1, channel_count, sample_rate, sample_rate * block_align, block_align, bit_depth)
        + b"data"
        + struct.pack("<I", data_bytes)
    )


def quantize_pcm(samples: np.ndarray, bit_depth: int) -> bytes:
    scale = float(1 << (bit_depth - 1))
    clipped = np.clip(np.asarray(samples, dtype=np.float64), -1.0, 1.0 - 1.0 / scale)
    integers = np.rint(clipped * scale).astype(np.int64)
    if bit_depth == 16:
        return integers.astype("<i2").tobytes(order="C")
    if bit_depth == 24:
        unsigned = integers.astype(np.int64) & 0xFFFFFF
        octets = np.empty((unsigned.size, 3), dtype=np.uint8)
        octets[:, 0] = unsigned & 0xFF
        octets[:, 1] = (unsigned >> 8) & 0xFF
        octets[:, 2] = (unsigned >> 16) & 0xFF
        return octets.tobytes(order="C")
    return integers.astype("<i4").tobytes(order="C")


def write_wav(path: Path, samples: np.ndarray, sample_rate: int, bit_depth: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    frames = int(samples.shape[0])
    header = wav_header(sample_rate, bit_depth, 1, frames)
    payload = quantize_pcm(samples, bit_depth)
    temp = path.with_name(path.name + ".tmp")
    with temp.open("wb") as handle:
        handle.write(header)
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temp, path)


def read_bytes(path: Path) -> bytes:
    return path.read_bytes()

