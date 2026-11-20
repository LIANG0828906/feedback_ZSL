"""RIFF/WAVE PCM reader and deterministic writer."""

from __future__ import annotations

import hashlib
import os
import struct
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import FormatUnsupportedError
from .params import SAMPLE_RATE, channel_layout

_PCM = 1
_WAVE_FORMAT_EXTENSIBLE = 0xFFFE


@dataclass(frozen=True)
class AudioBuffer:
    samples: np.ndarray
    sample_rate: int
    bit_depth: int
    source_sha256: str
    raw_pcm: bytes
    filename: str

    @property
    def channels(self) -> int:
        return int(self.samples.shape[1])

    @property
    def frame_count(self) -> int:
        return int(self.samples.shape[0])

    @property
    def layout(self) -> tuple[str, ...]:
        return channel_layout(self.channels)

    @property
    def block_align(self) -> int:
        return self.channels * (self.bit_depth // 8)

    def window_bytes(self, start_sample: int, end_sample: int) -> bytes:
        if not 0 <= start_sample <= end_sample <= self.frame_count:
            raise ValueError("sample window outside audio")
        begin = start_sample * self.block_align
        end = end_sample * self.block_align
        return self.raw_pcm[begin:end]

    def content_hash(self, start_sample: int, end_sample: int) -> str:
        h = hashlib.sha256()
        h.update(b"BROADCAST-LOUDNESS-WINDOW-v1\0")
        h.update(
            struct.pack("<IIII", self.sample_rate, self.bit_depth, self.channels, 1)
        )
        h.update(",".join(self.layout).encode("ascii"))
        h.update(b"\0")
        h.update(struct.pack("<QQ", start_sample, end_sample))
        h.update(self.window_bytes(start_sample, end_sample))
        return h.hexdigest()

    def sync_marker(self, start_sample: int, end_sample: int) -> str:
        return f"W1:{self.source_sha256[:16]}:{start_sample:016d}:{end_sample:016d}"


def _read_u32(data: bytes, offset: int) -> int:
    return struct.unpack_from("<I", data, offset)[0]


def _decode_24bit(raw: bytes) -> np.ndarray:
    triples = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3).astype(np.int32)
    value = triples[:, 0] | (triples[:, 1] << 8) | (triples[:, 2] << 16)
    value = np.where(value & 0x800000, value - 0x1000000, value)
    return value.astype(np.float64) / 8_388_608.0


def read_wav(path: str | os.PathLike[str]) -> AudioBuffer:
    file_path = Path(path)
    data = file_path.read_bytes()
    if len(data) < 12 or data[:4] != b"RIFF" or data[8:12] != b"WAVE":
        raise FormatUnsupportedError("not a RIFF/WAVE file", filename=file_path.name)

    fmt: bytes | None = None
    pcm_chunks: list[bytes] = []
    offset = 12
    while offset + 8 <= len(data):
        chunk_id = data[offset : offset + 4]
        chunk_size = _read_u32(data, offset + 4)
        body_start = offset + 8
        body_end = body_start + chunk_size
        if body_end > len(data):
            raise FormatUnsupportedError(
                "truncated WAVE chunk", filename=file_path.name
            )
        body = data[body_start:body_end]
        if chunk_id == b"fmt " and fmt is None:
            fmt = body
        elif chunk_id == b"data":
            pcm_chunks.append(body)
        offset = body_end + (chunk_size & 1)

    if fmt is None or len(fmt) < 16 or not pcm_chunks:
        raise FormatUnsupportedError(
            "missing fmt or data chunk", filename=file_path.name
        )

    audio_format, channels, sample_rate, _, block_align, bit_depth = struct.unpack_from(
        "<HHIIHH", fmt, 0
    )
    if audio_format == _WAVE_FORMAT_EXTENSIBLE:
        if len(fmt) < 40 or fmt[24:26] != b"\x01\x00":
            raise FormatUnsupportedError(
                "unsupported WAVE extensible subformat", filename=file_path.name
            )
    elif audio_format != _PCM:
        raise FormatUnsupportedError(
            "only integer PCM WAV is supported",
            filename=file_path.name,
            audio_format=audio_format,
        )

    if sample_rate != SAMPLE_RATE:
        raise FormatUnsupportedError(
            "only 48 kHz is supported",
            filename=file_path.name,
            sample_rate=sample_rate,
        )
    if bit_depth not in (16, 24):
        raise FormatUnsupportedError(
            "only 16-bit or 24-bit PCM is supported",
            filename=file_path.name,
            bit_depth=bit_depth,
        )
    if channels not in range(1, 7):
        raise FormatUnsupportedError(
            "only mono through 5.1 layouts are supported",
            filename=file_path.name,
            channels=channels,
        )
    expected_align = channels * (bit_depth // 8)
    if block_align != expected_align:
        raise FormatUnsupportedError(
            "invalid block alignment",
            filename=file_path.name,
            block_align=block_align,
            expected=expected_align,
        )

    raw = b"".join(pcm_chunks)
    frame_count = len(raw) // block_align
    raw = raw[: frame_count * block_align]
    if bit_depth == 16:
        values = np.frombuffer(raw, dtype="<i2").astype(np.float64) / 32_768.0
    else:
        values = _decode_24bit(raw)
    samples = values.reshape(frame_count, channels).copy()
    samples.flags.writeable = False
    source_hash = hashlib.sha256(data).hexdigest()
    return AudioBuffer(
        samples=samples,
        sample_rate=sample_rate,
        bit_depth=bit_depth,
        source_sha256=source_hash,
        raw_pcm=raw,
        filename=file_path.name,
    )


def quantize_pcm(samples: np.ndarray, bit_depth: int) -> bytes:
    if bit_depth == 16:
        scale = 32_768.0
        values = np.rint(np.clip(samples, -1.0, 32767.0 / 32768.0) * scale).astype(
            "<i2"
        )
        return values.tobytes(order="C")
    if bit_depth == 24:
        values = np.rint(np.clip(samples, -1.0, 8388607.0 / 8388608.0) * 8_388_608.0)
        ints = values.astype(np.int32).reshape(-1)
        ints = np.where(ints < 0, ints + 0x1000000, ints)
        bytes_out = np.empty((ints.size, 3), dtype=np.uint8)
        bytes_out[:, 0] = ints & 0xFF
        bytes_out[:, 1] = (ints >> 8) & 0xFF
        bytes_out[:, 2] = (ints >> 16) & 0xFF
        return bytes_out.tobytes(order="C")
    raise FormatUnsupportedError(
        "only 16-bit or 24-bit PCM is supported", bit_depth=bit_depth
    )


def make_wav_bytes(samples: np.ndarray, bit_depth: int = 24) -> bytes:
    array = np.asarray(samples, dtype=np.float64)
    if array.ndim != 2 or array.shape[1] not in range(1, 7):
        raise FormatUnsupportedError("expected 1 to 6 audio channels")
    channels = int(array.shape[1])
    data = quantize_pcm(array, bit_depth)
    block_align = channels * (bit_depth // 8)
    byte_rate = SAMPLE_RATE * block_align
    fmt = struct.pack(
        "<HHIIHH",
        _PCM,
        channels,
        SAMPLE_RATE,
        byte_rate,
        block_align,
        bit_depth,
    )
    payload = b"fmt " + struct.pack("<I", len(fmt)) + fmt
    payload += b"data" + struct.pack("<I", len(data)) + data
    if len(data) & 1:
        payload += b"\0"
    return b"RIFF" + struct.pack("<I", len(payload) + 4) + b"WAVE" + payload


def write_wav(
    path: str | os.PathLike[str],
    samples: np.ndarray,
    bit_depth: int = 24,
) -> str:
    """Write a deterministic WAV and return the SHA-256 of the file bytes."""
    payload = make_wav_bytes(samples, bit_depth)
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination.with_name(destination.name + ".tmp")
    with temporary.open("wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, destination)
    return hashlib.sha256(payload).hexdigest()
