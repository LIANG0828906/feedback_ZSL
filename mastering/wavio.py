from __future__ import annotations

import hashlib
import math
import os
import struct
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import MasteringIssue, issue


_PCM_FORMAT = 1
_FLOAT_FORMAT = 3
_EXTENSIBLE_FORMAT = 0xFFFE


@dataclass(frozen=True)
class WavInfo:
    sample_rate: int
    channels: int
    bit_depth: int
    frames: int
    audio_format: int

    @property
    def block_align(self) -> int:
        return self.channels * ((self.bit_depth + 7) // 8)


def _read_chunks(raw: bytes) -> tuple[bytes, list[tuple[bytes, bytes]]]:
    if len(raw) < 12 or raw[:4] != b"RIFF" or raw[8:12] != b"WAVE":
        raise ValueError("not a RIFF/WAVE file")
    pos = 12
    chunks: list[tuple[bytes, bytes]] = []
    while pos + 8 <= len(raw):
        chunk_id = raw[pos : pos + 4]
        size = struct.unpack_from("<I", raw, pos + 4)[0]
        start = pos + 8
        end = start + size
        if end > len(raw):
            raise ValueError(f"chunk {chunk_id!r} exceeds file length")
        chunks.append((chunk_id, raw[start:end]))
        pos = end + (size & 1)
    return raw[:4], chunks


def _parse_fmt(payload: bytes) -> tuple[int, int, int, int]:
    if len(payload) < 16:
        raise ValueError("fmt chunk is too short")
    audio_format, channels, sample_rate, _byte_rate, block_align, bit_depth = struct.unpack_from(
        "<HHIIHH", payload, 0
    )
    if audio_format == _EXTENSIBLE_FORMAT:
        if len(payload) < 40:
            raise ValueError("WAVE_FORMAT_EXTENSIBLE fmt chunk is too short")
        audio_format = struct.unpack_from("<H", payload, 24)[0]
    if channels <= 0 or sample_rate <= 0 or bit_depth <= 0:
        raise ValueError("invalid fmt values")
    bytes_per_sample = (bit_depth + 7) // 8
    if block_align != channels * bytes_per_sample:
        raise ValueError("block alignment does not match channels and bit depth")
    return audio_format, channels, sample_rate, bit_depth


def probe_wav(path: Path, track: str = "") -> tuple[WavInfo | None, list[MasteringIssue]]:
    try:
        raw = path.read_bytes()
    except FileNotFoundError:
        return None, [issue("TRACK_MISSING", track or str(path), "track file does not exist")]
    except OSError as exc:
        return None, [issue("TRACK_UNREADABLE", track or str(path), f"cannot read track: {exc}")]

    try:
        _, chunks = _read_chunks(raw)
        fmt_payloads = [payload for chunk_id, payload in chunks if chunk_id == b"fmt "]
        data_payloads = [payload for chunk_id, payload in chunks if chunk_id == b"data"]
        if len(fmt_payloads) != 1:
            raise ValueError("expected exactly one fmt chunk")
        if len(data_payloads) != 1:
            raise ValueError("expected exactly one data chunk")
        audio_format, channels, sample_rate, bit_depth = _parse_fmt(fmt_payloads[0])
        if audio_format not in (_PCM_FORMAT, _FLOAT_FORMAT):
            raise ValueError(f"unsupported WAV format {audio_format}")
        if audio_format == _PCM_FORMAT and bit_depth not in (8, 16, 24, 32):
            raise ValueError(f"unsupported PCM bit depth {bit_depth}")
        if audio_format == _FLOAT_FORMAT and bit_depth not in (32, 64):
            raise ValueError(f"unsupported float bit depth {bit_depth}")
        block_align = channels * ((bit_depth + 7) // 8)
        if len(data_payloads[0]) % block_align:
            raise ValueError("data chunk is not an integral number of frames")
        info = WavInfo(
            sample_rate=sample_rate,
            channels=channels,
            bit_depth=bit_depth,
            frames=len(data_payloads[0]) // block_align,
            audio_format=audio_format,
        )
        return info, []
    except (ValueError, struct.error) as exc:
        return None, [issue("INVALID_WAV", track or str(path), str(exc))]


def _decode_pcm(data: bytes, info: WavInfo) -> np.ndarray:
    channels = info.channels
    bits = info.bit_depth
    if info.audio_format == _FLOAT_FORMAT:
        dtype = "<f4" if bits == 32 else "<f8"
        values = np.frombuffer(data, dtype=dtype).astype(np.float64, copy=False)
        return values.reshape(-1, channels).copy()

    width = (bits + 7) // 8
    if bits == 8:
        values = (np.frombuffer(data, dtype=np.uint8).astype(np.float64) - 128.0) / 128.0
    elif bits == 16:
        values = np.frombuffer(data, dtype="<i2").astype(np.float64) / 32768.0
    elif bits == 24:
        packed = np.frombuffer(data, dtype=np.uint8).reshape(-1, 3).astype(np.int32)
        values24 = packed[:, 0] | (packed[:, 1] << 8) | (packed[:, 2] << 16)
        values24 = np.where(values24 & 0x800000, values24 - 0x1000000, values24)
        values = values24.astype(np.float64) / 8388608.0
    elif bits == 32:
        values = np.frombuffer(data, dtype="<i4").astype(np.float64) / 2147483648.0
    else:
        raise ValueError(f"unsupported bit depth {bits}")

    if width == 0:
        raise ValueError("invalid sample width")
    return values.reshape(-1, channels).copy()


def read_wav(path: Path, track: str = "") -> tuple[np.ndarray | None, WavInfo | None, list[MasteringIssue]]:
    info, issues = probe_wav(path, track)
    if info is None:
        return None, None, issues
    try:
        raw = path.read_bytes()
        _, chunks = _read_chunks(raw)
        data = next(payload for chunk_id, payload in chunks if chunk_id == b"data")
        samples = _decode_pcm(data, info)
        if not np.all(np.isfinite(samples)):
            return None, info, [issue("INVALID_WAV", track, "WAV contains non-finite samples")]
        return samples, info, []
    except (ValueError, OSError, StopIteration, struct.error) as exc:
        return None, info, [issue("INVALID_WAV", track, f"cannot decode samples: {exc}")]


def quantize_pcm(samples: np.ndarray, bit_depth: int) -> np.ndarray:
    if bit_depth not in (16, 24, 32):
        raise ValueError("output supports 16, 24, or 32-bit PCM")
    scale = float(1 << (bit_depth - 1))
    clipped = np.clip(samples, -1.0, 1.0 - 1.0 / scale)
    return np.rint(clipped * scale).astype(np.int64)


def encode_pcm(samples: np.ndarray, bit_depth: int) -> bytes:
    ints = quantize_pcm(samples, bit_depth)
    if bit_depth == 16:
        return ints.astype("<i2").tobytes(order="C")
    if bit_depth == 32:
        return ints.astype("<i4").tobytes(order="C")

    unsigned = np.where(ints < 0, ints + (1 << 24), ints).astype(np.uint32).reshape(-1)
    packed = np.empty((unsigned.size, 3), dtype=np.uint8)
    packed[:, 0] = unsigned & 0xFF
    packed[:, 1] = (unsigned >> 8) & 0xFF
    packed[:, 2] = (unsigned >> 16) & 0xFF
    return packed.tobytes(order="C")


def wav_header(sample_rate: int, channels: int, bit_depth: int, frames: int) -> bytes:
    if bit_depth not in (16, 24, 32):
        raise ValueError("output supports 16, 24, or 32-bit PCM")
    block_align = channels * ((bit_depth + 7) // 8)
    data_size = frames * block_align
    if data_size > 0xFFFFFFFF - 36:
        raise ValueError("WAV data exceeds the RIFF 32-bit size limit")
    return struct.pack(
        "<4sI4s4sIHHIIHH4sI",
        b"RIFF",
        36 + data_size,
        b"WAVE",
        b"fmt ",
        16,
        _PCM_FORMAT,
        channels,
        sample_rate,
        sample_rate * block_align,
        block_align,
        bit_depth,
        b"data",
        data_size,
    )


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def atomic_write_bytes(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def atomic_copy(src: Path, dst: Path) -> None:
    dst.parent.mkdir(parents=True, exist_ok=True)
    temporary = dst.with_name(dst.name + ".tmp")
    with src.open("rb") as reader, temporary.open("wb") as writer:
        while True:
            block = reader.read(1024 * 1024)
            if not block:
                break
            writer.write(block)
        writer.flush()
        os.fsync(writer.fileno())
    os.replace(temporary, dst)


def db_to_gain(db: float) -> float:
    return 10.0 ** (db / 20.0)


def gain_to_db(gain: float) -> float:
    return 20.0 * math.log10(max(gain, 1e-15))

