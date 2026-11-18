from __future__ import annotations

import hashlib
import os
import struct
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np


class WavError(ValueError):
    pass


@dataclass(frozen=True)
class WavHeader:
    sample_rate: int
    channels: int
    bits_per_sample: int
    format_code: int
    block_align: int
    byte_rate: int
    data_offset: int
    data_bytes: int
    frame_count: int

    @property
    def is_pcm(self) -> bool:
        return self.format_code == 1

    @property
    def is_float(self) -> bool:
        return self.format_code == 3

    def as_dict(self) -> dict[str, Any]:
        return {
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "bits_per_sample": self.bits_per_sample,
            "format_code": self.format_code,
            "block_align": self.block_align,
            "byte_rate": self.byte_rate,
            "data_offset": self.data_offset,
            "data_bytes": self.data_bytes,
            "frame_count": self.frame_count,
        }


@dataclass(frozen=True)
class WavData:
    header: WavHeader
    samples: np.ndarray

    @property
    def sample_rate(self) -> int:
        return self.header.sample_rate

    @property
    def channels(self) -> int:
        return self.header.channels

    @property
    def frames(self) -> int:
        return self.header.frame_count


def _parse_fmt(payload: bytes) -> tuple[int, int, int, int, int, int]:
    if len(payload) < 16:
        raise WavError("fmt chunk is shorter than 16 bytes")
    format_code, channels, sample_rate, byte_rate, block_align, bits = struct.unpack_from(
        "<HHIIHH", payload, 0
    )
    if format_code == 0xFFFE:
        if len(payload) < 40:
            raise WavError("WAVE_FORMAT_EXTENSIBLE fmt chunk is incomplete")
        valid_bits = struct.unpack_from("<H", payload, 18)[0]
        subformat = payload[24:40]
        # KSDATAFORMAT_SUBTYPE_PCM/ IEEE_FLOAT start with the format code.
        format_code = struct.unpack_from("<H", subformat, 0)[0]
        bits = valid_bits or bits
    return format_code, channels, sample_rate, byte_rate, block_align, bits


def read_wav_header(path: str | os.PathLike[str]) -> WavHeader:
    """Read and validate the structural RIFF header without decoding samples."""

    p = Path(path)
    file_size = p.stat().st_size
    with p.open("rb") as handle:
        riff = handle.read(12)
        if len(riff) != 12 or riff[:4] != b"RIFF" or riff[8:12] != b"WAVE":
            raise WavError("not a RIFF/WAVE file")
        riff_size = struct.unpack_from("<I", riff, 4)[0]
        if riff_size + 8 > file_size:
            raise WavError("RIFF length exceeds file length")

        fmt_payload: bytes | None = None
        data_offset = -1
        data_bytes = -1
        while handle.tell() + 8 <= file_size:
            chunk_offset = handle.tell()
            chunk_header = handle.read(8)
            if len(chunk_header) != 8:
                raise WavError("truncated chunk header")
            chunk_id, chunk_size = struct.unpack("<4sI", chunk_header)
            payload_offset = handle.tell()
            if payload_offset + chunk_size > file_size:
                raise WavError(f"chunk {chunk_id!r} exceeds file length")
            if chunk_id == b"fmt ":
                fmt_payload = handle.read(chunk_size)
            elif chunk_id == b"data":
                data_offset = payload_offset
                data_bytes = chunk_size
                handle.seek(chunk_size, os.SEEK_CUR)
            else:
                handle.seek(chunk_size, os.SEEK_CUR)
            if chunk_size & 1:
                handle.seek(1, os.SEEK_CUR)
            if handle.tell() <= chunk_offset:
                raise WavError("non-advancing RIFF chunk")

    if fmt_payload is None:
        raise WavError("missing fmt chunk")
    if data_offset < 0:
        raise WavError("missing data chunk")

    format_code, channels, sample_rate, byte_rate, block_align, bits = _parse_fmt(fmt_payload)
    if channels <= 0:
        raise WavError("invalid channel count")
    if sample_rate <= 0:
        raise WavError("invalid sample rate")
    if bits not in (16, 24, 32):
        raise WavError(f"unsupported bit depth: {bits}")
    expected_align = channels * ((bits + 7) // 8)
    if block_align != expected_align:
        raise WavError(
            f"block alignment {block_align} does not match channels/bit depth ({expected_align})"
        )
    if byte_rate != sample_rate * block_align:
        raise WavError("byte rate is inconsistent with sample rate and block alignment")
    if data_bytes % block_align:
        raise WavError("data chunk length is not a whole number of frames")
    if format_code == 1 and bits not in (16, 24):
        raise WavError(f"unsupported integer PCM depth: {bits}")
    if format_code not in (1, 3):
        raise WavError(f"unsupported WAVE format code: {format_code}")
    if format_code == 3 and bits != 32:
        raise WavError("only 32-bit IEEE float WAV is supported")

    return WavHeader(
        sample_rate=sample_rate,
        channels=channels,
        bits_per_sample=bits,
        format_code=format_code,
        block_align=block_align,
        byte_rate=byte_rate,
        data_offset=data_offset,
        data_bytes=data_bytes,
        frame_count=data_bytes // block_align,
    )


def _decode_pcm_bytes(raw: bytes, header: WavHeader) -> np.ndarray:
    if header.format_code == 3:
        values = np.frombuffer(raw, dtype="<f4").astype(np.float64)
        return values.reshape(-1, header.channels)
    if header.bits_per_sample == 16:
        values = np.frombuffer(raw, dtype="<i2").astype(np.float64)
        values /= 32768.0
        return values.reshape(-1, header.channels)
    if header.bits_per_sample == 24:
        packed = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3).astype(np.int32)
        values = packed[:, 0] | (packed[:, 1] << 8) | (packed[:, 2] << 16)
        values = (values ^ 0x800000) - 0x800000
        decoded = values.astype(np.float64) / 8388608.0
        return decoded.reshape(-1, header.channels)
    raise WavError(f"cannot decode PCM depth {header.bits_per_sample}")


def read_wav(path: str | os.PathLike[str]) -> WavData:
    header = read_wav_header(path)
    p = Path(path)
    with p.open("rb") as handle:
        handle.seek(header.data_offset)
        raw = handle.read(header.data_bytes)
    if len(raw) != header.data_bytes:
        raise WavError("truncated data chunk")
    samples = _decode_pcm_bytes(raw, header)
    if not np.all(np.isfinite(samples)):
        raise WavError("WAV contains non-finite samples")
    return WavData(header=header, samples=samples)


def parse_output_format(value: Any) -> tuple[str, int]:
    """Return (wave_format, bits): wave_format is 'pcm' or 'float'."""

    if isinstance(value, int):
        if value == 16:
            return "pcm", 16
        if value == 24:
            return "pcm", 24
        if value == 32:
            return "float", 32
    if not isinstance(value, str):
        raise ValueError("output format must be a string or bit depth")
    normalized = value.lower().replace("-", "_")
    aliases = {
        "pcm16": ("pcm", 16),
        "pcm_s16": ("pcm", 16),
        "int16": ("pcm", 16),
        "16": ("pcm", 16),
        "pcm24": ("pcm", 24),
        "pcm_s24": ("pcm", 24),
        "int24": ("pcm", 24),
        "24": ("pcm", 24),
        "float32": ("float", 32),
        "float_32": ("float", 32),
        "f32": ("float", 32),
        "32f": ("float", 32),
        "32": ("float", 32),
    }
    if normalized not in aliases:
        raise ValueError(f"unsupported output format: {value!r}")
    return aliases[normalized]


def _quantize_int(samples: np.ndarray, bits: int) -> np.ndarray:
    scale = float(1 << (bits - 1))
    lower = -(1 << (bits - 1))
    upper = (1 << (bits - 1)) - 1
    quantized = np.rint(samples * scale)
    np.clip(quantized, lower, upper, out=quantized)
    return quantized.astype(np.int32)


def _int_to_little_endian_bytes(values: np.ndarray, bits: int) -> bytes:
    if bits == 16:
        return values.astype("<i2").tobytes()
    if bits == 24:
        as_u32 = values.astype("<i4").view(np.uint8).reshape(-1, 4)
        return np.ascontiguousarray(as_u32[:, :3]).tobytes()
    raise ValueError(f"unsupported PCM bit depth: {bits}")


def _quantized_float_view(samples: np.ndarray, wave_format: str, bits: int) -> np.ndarray:
    if wave_format == "float":
        return samples.astype(np.float32).astype(np.float64)
    quantized = _quantize_int(samples, bits)
    return quantized.astype(np.float64) / float(1 << (bits - 1))


def write_wav_atomic(
    path: str | os.PathLike[str],
    samples: np.ndarray,
    sample_rate: int,
    wave_format: str,
    bits: int,
    *,
    chunk_frames: int = 262144,
) -> dict[str, Any]:
    """Write a canonical little-endian WAV via a same-directory atomic replace."""

    if wave_format not in ("pcm", "float"):
        raise ValueError("wave_format must be 'pcm' or 'float'")
    if samples.ndim != 2:
        raise ValueError("samples must have shape (frames, channels)")
    if not np.all(np.isfinite(samples)):
        raise ValueError("refusing to write non-finite samples")
    if wave_format == "pcm" and bits not in (16, 24):
        raise ValueError("PCM output must be 16 or 24 bit")
    if wave_format == "float" and bits != 32:
        raise ValueError("float output must be 32 bit")

    frames, channels = samples.shape
    bytes_per_sample = bits // 8
    block_align = channels * bytes_per_sample
    data_bytes = frames * block_align
    if data_bytes > 0xFFFFFFFF - 64:
        raise ValueError("WAV data exceeds the RIFF 4 GiB limit")
    format_code = 3 if wave_format == "float" else 1
    byte_rate = sample_rate * block_align
    padding = data_bytes & 1
    riff_size = 4 + (8 + 16) + (8 + data_bytes + padding)

    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    temp_name: str | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="wb", dir=target.parent, prefix=f".{target.name}.", suffix=".partial", delete=False
        ) as handle:
            temp_name = handle.name
            handle.write(b"RIFF")
            handle.write(struct.pack("<I", riff_size))
            handle.write(b"WAVEfmt ")
            handle.write(
                struct.pack(
                    "<IHHIIHH",
                    16,
                    format_code,
                    channels,
                    sample_rate,
                    byte_rate,
                    block_align,
                    bits,
                )
            )
            handle.write(b"data")
            handle.write(struct.pack("<I", data_bytes))
            for start in range(0, frames, chunk_frames):
                block = samples[start : start + chunk_frames]
                if wave_format == "float":
                    handle.write(block.astype("<f4").tobytes())
                else:
                    quantized = _quantize_int(block, bits)
                    handle.write(_int_to_little_endian_bytes(quantized, bits))
            if padding:
                handle.write(b"\x00")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, target)
        temp_name = None
    finally:
        if temp_name is not None:
            try:
                os.unlink(temp_name)
            except FileNotFoundError:
                pass

    header = read_wav_header(target)
    expected_block_align = channels * bytes_per_sample
    expected_data_bytes = frames * expected_block_align
    if (
        header.sample_rate != sample_rate
        or header.channels != channels
        or header.bits_per_sample != bits
        or header.format_code != format_code
        or header.block_align != expected_block_align
        or header.data_bytes != expected_data_bytes
        or header.frame_count != frames
    ):
        raise RuntimeError("written WAV header failed post-write consistency verification")
    return {
        "path": str(target),
        "format": "float32" if wave_format == "float" else f"pcm_s{bits}",
        "sample_rate": header.sample_rate,
        "channels": header.channels,
        "bits_per_sample": header.bits_per_sample,
        "format_code": header.format_code,
        "block_align": header.block_align,
        "byte_rate": header.byte_rate,
        "data_bytes": header.data_bytes,
        "frame_count": header.frame_count,
        "duration_seconds": header.frame_count / header.sample_rate,
        "sha256": sha256_file(target),
    }


def sha256_file(path: str | os.PathLike[str]) -> str:
    digest = hashlib.sha256()
    with Path(path).open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def quantized_float_view(
    samples: np.ndarray, wave_format: str, bits: int
) -> np.ndarray:
    """Return the values a listener will decode from the requested output format."""

    return _quantized_float_view(samples, wave_format, bits)

