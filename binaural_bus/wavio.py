"""Minimal deterministic RIFF/WAVE I/O using only the Python standard library."""

from __future__ import annotations

import os
import struct
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import ValidationFailure, error_item


_PCM = 1
_IEEE_FLOAT = 3
_EXTENSIBLE = 0xFFFE


@dataclass(frozen=True)
class WavInfo:
    sample_rate: int
    bit_depth: int
    channels: int
    frames: int
    sample_format: str
    data_offset: int
    data_bytes: int


def inspect_wav(path: str | Path, channel_name: str | None = None) -> WavInfo:
    """Read and validate the RIFF header without decoding audio samples."""

    p = Path(path)
    if not p.is_file():
        raise ValidationFailure(
            [
                error_item(
                    "CHANNEL_FILE_MISSING",
                    channel=channel_name or p.name,
                    message="Declared channel file does not exist.",
                )
            ]
        )

    try:
        with p.open("rb") as fh:
            header = fh.read(12)
            if len(header) != 12 or header[:4] != b"RIFF" or header[8:12] != b"WAVE":
                raise ValueError("not a RIFF/WAVE file")

            fmt: tuple[int, int, int, int, int, int] | None = None
            data_offset = -1
            data_bytes = 0
            data_found = False
            while True:
                chunk_header = fh.read(8)
                if len(chunk_header) < 8:
                    break
                chunk_id, chunk_size = struct.unpack("<4sI", chunk_header)
                payload_start = fh.tell()
                if chunk_id == b"fmt ":
                    payload = fh.read(chunk_size)
                    if len(payload) < 16:
                        raise ValueError("short fmt chunk")
                    audio_format, channels, sample_rate, _byte_rate, block_align, bits = (
                        struct.unpack("<HHIIHH", payload[:16])
                    )
                    if audio_format == _EXTENSIBLE:
                        if len(payload) < 40:
                            raise ValueError("short extensible fmt chunk")
                        sub_format = payload[24:40]
                        audio_format = struct.unpack("<H", sub_format[:2])[0]
                    fmt = (
                        audio_format,
                        channels,
                        sample_rate,
                        block_align,
                        bits,
                        chunk_size,
                    )
                elif chunk_id == b"data":
                    data_offset = payload_start
                    data_bytes = chunk_size
                    data_found = True
                    fh.seek(chunk_size, os.SEEK_CUR)
                else:
                    fh.seek(chunk_size, os.SEEK_CUR)
                if chunk_size & 1:
                    fh.seek(1, os.SEEK_CUR)
            if fmt is None or not data_found:
                raise ValueError("missing fmt or data chunk")
    except (OSError, ValueError, struct.error) as exc:
        raise ValidationFailure(
            [
                error_item(
                    "CHANNEL_FILE_INVALID",
                    channel=channel_name or p.name,
                    message=str(exc),
                )
            ]
        ) from exc

    audio_format, channels, sample_rate, block_align, bits, _ = fmt
    if audio_format == _PCM:
        sample_format = "pcm"
    elif audio_format == _IEEE_FLOAT:
        sample_format = "float"
    else:
        raise ValidationFailure(
            [
                error_item(
                    "UNSUPPORTED_WAV_FORMAT",
                    channel=channel_name or p.name,
                    message=f"Unsupported WAVE audio format {audio_format}.",
                )
            ]
        )

    if channels <= 0 or block_align <= 0 or bits <= 0:
        raise ValidationFailure(
            [
                error_item(
                    "CHANNEL_FILE_INVALID",
                    channel=channel_name or p.name,
                    message="Invalid WAVE channel/bit-depth metadata.",
                )
            ]
        )
    if bits % 8 != 0:
        raise ValidationFailure(
            [
                error_item(
                    "UNSUPPORTED_BIT_DEPTH",
                    channel=channel_name or p.name,
                    message=f"Unsupported bit depth {bits}.",
                )
            ]
        )

    frames = data_bytes // block_align
    return WavInfo(
        sample_rate=sample_rate,
        bit_depth=bits,
        channels=channels,
        frames=frames,
        sample_format=sample_format,
        data_offset=data_offset,
        data_bytes=data_bytes,
    )


def read_wav(path: str | Path, channel_name: str | None = None) -> tuple[np.ndarray, int]:
    """Read a WAV file as float64 samples with shape (channels, frames)."""

    info = inspect_wav(path, channel_name)
    with Path(path).open("rb") as fh:
        fh.seek(info.data_offset)
        raw = fh.read(info.data_bytes)

    usable = len(raw) // (info.channels * (info.bit_depth // 8))
    width = info.bit_depth // 8
    raw = raw[: usable * info.channels * width]

    if info.sample_format == "float":
        if info.bit_depth == 32:
            values = np.frombuffer(raw, dtype="<f4").astype(np.float64)
        elif info.bit_depth == 64:
            values = np.frombuffer(raw, dtype="<f8").astype(np.float64)
        else:
            raise ValidationFailure(
                [
                    error_item(
                        "UNSUPPORTED_BIT_DEPTH",
                        channel=channel_name or Path(path).name,
                        message=f"Float WAV bit depth {info.bit_depth} is unsupported.",
                    )
                ]
            )
    elif width == 2:
        values = np.frombuffer(raw, dtype="<i2").astype(np.float64) / 32768.0
    elif width == 3:
        octets = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3).astype(np.int32)
        signed = octets[:, 0] | (octets[:, 1] << 8) | (octets[:, 2] << 16)
        signed = np.where(signed & 0x800000, signed - 0x1000000, signed)
        values = signed.astype(np.float64) / 8388608.0
    elif width == 4:
        values = np.frombuffer(raw, dtype="<i4").astype(np.float64) / 2147483648.0
    else:
        raise ValidationFailure(
            [
                error_item(
                    "UNSUPPORTED_BIT_DEPTH",
                    channel=channel_name or Path(path).name,
                    message=f"PCM bit depth {info.bit_depth} is unsupported.",
                )
            ]
        )

    return values.reshape(-1, info.channels).T.copy(), info.sample_rate


def float_to_pcm_bytes(samples: np.ndarray, bit_depth: int) -> bytes:
    """Convert channel-first samples to interleaved little-endian PCM bytes."""

    if samples.ndim != 2:
        raise ValueError("samples must have shape (channels, frames)")
    interleaved = samples.T.reshape(-1)
    if bit_depth == 16:
        ints = np.rint(np.clip(interleaved, -1.0, 1.0) * 32768.0).astype(np.int64)
        ints = np.clip(ints, -32768, 32767).astype("<i2")
        return ints.tobytes(order="C")
    if bit_depth == 24:
        ints = np.rint(np.clip(interleaved, -1.0, 1.0) * 8388608.0).astype(np.int64)
        ints = np.clip(ints, -8388608, 8388607).astype(np.int64)
        octets = np.empty((ints.size, 3), dtype=np.uint8)
        octets[:, 0] = ints & 0xFF
        octets[:, 1] = (ints >> 8) & 0xFF
        octets[:, 2] = (ints >> 16) & 0xFF
        return octets.tobytes(order="C")
    if bit_depth == 32:
        ints = np.rint(np.clip(interleaved, -1.0, 1.0) * 2147483648.0).astype(np.int64)
        ints = np.clip(ints, -2147483648, 2147483647).astype("<i4")
        return ints.tobytes(order="C")
    raise ValueError(f"unsupported PCM bit depth: {bit_depth}")


def write_wav(path: str | Path, samples: np.ndarray, sample_rate: int, bit_depth: int) -> None:
    """Write a complete PCM WAV file atomically."""

    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = float_to_pcm_bytes(samples, bit_depth)
    tmp = path.with_name(path.name + ".tmp")
    with tmp.open("wb") as fh:
        _write_header(fh, sample_rate, bit_depth, samples.shape[0], len(payload))
        fh.write(payload)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


def create_streaming_wav(
    path: str | Path,
    sample_rate: int,
    bit_depth: int,
    channels: int,
    reserved_frames: int = 0,
) -> int:
    """Create a WAV with a placeholder data size and return its data offset."""

    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("wb") as fh:
        _write_header(
            fh,
            sample_rate,
            bit_depth,
            channels,
            reserved_frames * channels * (bit_depth // 8),
        )
        fh.flush()
        os.fsync(fh.fileno())
    return 44


def finalize_streaming_wav(
    path: str | Path,
    sample_rate: int,
    bit_depth: int,
    channels: int,
    frames: int,
) -> None:
    """Rewrite a streaming WAV header after the PCM payload is complete."""

    path = Path(path)
    data_bytes = frames * channels * (bit_depth // 8)
    with path.open("r+b") as fh:
        fh.seek(0)
        _write_header(fh, sample_rate, bit_depth, channels, data_bytes)
        fh.flush()
        os.fsync(fh.fileno())


def _write_header(
    fh,
    sample_rate: int,
    bit_depth: int,
    channels: int,
    data_bytes: int,
) -> None:
    width = bit_depth // 8
    block_align = channels * width
    byte_rate = sample_rate * block_align
    riff_size = 36 + data_bytes
    header = struct.pack(
        "<4sI4s4sIHHIIHH4sI",
        b"RIFF",
        riff_size & 0xFFFFFFFF,
        b"WAVE",
        b"fmt ",
        16,
        _PCM,
        channels,
        sample_rate,
        byte_rate,
        block_align,
        bit_depth,
        b"data",
        data_bytes & 0xFFFFFFFF,
    )
    fh.write(header)

