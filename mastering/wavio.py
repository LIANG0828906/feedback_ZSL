from __future__ import annotations

import json
import os
import struct
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import WavFormatError


_PCM = 1
_IEEE_FLOAT = 3
_EXTENSIBLE = 0xFFFE


@dataclass(frozen=True)
class WavInfo:
    path: Path
    sample_rate: int
    channels: int
    bits_per_sample: int
    format_tag: int
    frames: int
    data_bytes: int
    data_offset: int


def _read_exact(handle, size: int) -> bytes:
    value = handle.read(size)
    if len(value) != size:
        raise WavFormatError("truncated WAV header")
    return value


def _parse_wav(path: Path) -> WavInfo:
    try:
        file_size = path.stat().st_size
        with path.open("rb") as handle:
            header = _read_exact(handle, 12)
            riff, riff_size, wave = struct.unpack("<4sI4s", header)
            if riff != b"RIFF" or wave != b"WAVE":
                raise WavFormatError("not a little-endian RIFF/WAVE file")
            if riff_size + 8 > file_size:
                raise WavFormatError("RIFF size exceeds actual file size")

            fmt: tuple[int, int, int, int, int] | None = None
            data_size: int | None = None
            data_offset: int | None = None
            while handle.tell() + 8 <= file_size:
                chunk_header = _read_exact(handle, 8)
                chunk_id, chunk_size = struct.unpack("<4sI", chunk_header)
                payload_offset = handle.tell()
                if payload_offset + chunk_size > file_size:
                    raise WavFormatError(f"{chunk_id!r} chunk exceeds file size")

                if chunk_id == b"fmt ":
                    if chunk_size < 16:
                        raise WavFormatError("fmt chunk is shorter than 16 bytes")
                    payload = _read_exact(handle, chunk_size)
                    (
                        format_tag,
                        channels,
                        sample_rate,
                        byte_rate,
                        block_align,
                        bits_per_sample,
                    ) = struct.unpack("<HHIIHH", payload[:16])
                    if format_tag == _EXTENSIBLE:
                        if chunk_size < 40:
                            raise WavFormatError("WAVE_FORMAT_EXTENSIBLE fmt chunk is too short")
                        subformat = struct.unpack("<H", payload[24:26])[0]
                        if subformat not in (_PCM, _IEEE_FLOAT):
                            raise WavFormatError(f"unsupported extensible subformat {subformat}")
                        format_tag = subformat
                    fmt = (
                        format_tag,
                        channels,
                        sample_rate,
                        block_align,
                        bits_per_sample,
                    )
                    if byte_rate != sample_rate * block_align:
                        raise WavFormatError("fmt byte rate is inconsistent with block alignment")
                elif chunk_id == b"data":
                    data_size = chunk_size
                    data_offset = payload_offset
                    handle.seek(chunk_size, os.SEEK_CUR)
                else:
                    handle.seek(chunk_size, os.SEEK_CUR)

                if chunk_size & 1:
                    handle.seek(1, os.SEEK_CUR)

            if fmt is None:
                raise WavFormatError("missing fmt chunk")
            if data_size is None or data_offset is None:
                raise WavFormatError("missing data chunk")

            format_tag, channels, sample_rate, block_align, bits_per_sample = fmt
            if channels <= 0 or sample_rate <= 0:
                raise WavFormatError("invalid channel count or sample rate")
            if bits_per_sample not in (16, 24, 32):
                raise WavFormatError(f"unsupported bit depth {bits_per_sample}")
            if format_tag not in (_PCM, _IEEE_FLOAT):
                raise WavFormatError(f"unsupported WAV format tag {format_tag}")
            if format_tag == _IEEE_FLOAT and bits_per_sample != 32:
                raise WavFormatError("IEEE float WAV must use 32-bit samples")

            bytes_per_sample = bits_per_sample // 8
            expected_align = channels * bytes_per_sample
            if block_align != expected_align:
                raise WavFormatError(
                    f"block alignment {block_align} != channels * bytes/sample {expected_align}"
                )
            if data_size % block_align:
                raise WavFormatError("data chunk length is not an integral number of frames")

            frames = data_size // block_align
            return WavInfo(
                path=path,
                sample_rate=sample_rate,
                channels=channels,
                bits_per_sample=bits_per_sample,
                format_tag=format_tag,
                frames=frames,
                data_bytes=data_size,
                data_offset=data_offset,
            )
    except OSError as exc:
        raise WavFormatError(str(exc)) from exc


def inspect_wav(path: str | Path) -> WavInfo:
    return _parse_wav(Path(path))


def read_wav(path: str | Path) -> tuple[np.ndarray, WavInfo]:
    target = Path(path)
    info = _parse_wav(target)
    try:
        with target.open("rb") as handle:
            handle.seek(info.data_offset)
            raw = handle.read(info.data_bytes)
    except OSError as exc:
        raise WavFormatError(str(exc)) from exc
    if len(raw) != info.data_bytes:
        raise WavFormatError("data chunk is truncated")

    channels = info.channels
    if info.format_tag == _IEEE_FLOAT:
        samples = np.frombuffer(raw, dtype="<f4").astype(np.float64)
    elif info.bits_per_sample == 16:
        samples = np.frombuffer(raw, dtype="<i2").astype(np.float64) / 32768.0
    elif info.bits_per_sample == 24:
        octets = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3)
        unsigned = (
            octets[:, 0].astype(np.uint32)
            | (octets[:, 1].astype(np.uint32) << 8)
            | (octets[:, 2].astype(np.uint32) << 16)
        )
        signed = unsigned.astype(np.int32)
        signed = np.where(signed & 0x800000, signed - 0x1000000, signed)
        samples = signed.astype(np.float64) / 8388608.0
    else:
        samples = np.frombuffer(raw, dtype="<i4").astype(np.float64) / 2147483648.0

    return samples.reshape(info.frames, channels), info


def quantize_samples(samples: np.ndarray, bits_per_sample: int) -> np.ndarray:
    """Return the exact float64 values represented by the requested WAV format."""
    if bits_per_sample == 32:
        return np.asarray(samples, dtype="<f4").astype(np.float64)
    if bits_per_sample not in (16, 24):
        raise ValueError(f"unsupported integer PCM bit depth: {bits_per_sample}")
    full_scale = float(1 << (bits_per_sample - 1))
    positive_scale = full_scale - 1.0
    low = -(1 << (bits_per_sample - 1))
    high = (1 << (bits_per_sample - 1)) - 1
    clipped = np.clip(samples, -1.0, 1.0)
    codes = np.where(clipped < 0.0, clipped * full_scale, clipped * positive_scale)
    codes = np.rint(codes)
    codes = np.clip(codes, low, high)
    return codes / float(1 << (bits_per_sample - 1))


def _pack_integer_pcm(samples: np.ndarray, bits_per_sample: int) -> tuple[bytes, np.ndarray]:
    full_scale = float(1 << (bits_per_sample - 1))
    positive_scale = full_scale - 1.0
    low = -(1 << (bits_per_sample - 1))
    high = (1 << (bits_per_sample - 1)) - 1
    clipped = np.clip(samples, -1.0, 1.0)
    codes = np.where(clipped < 0.0, clipped * full_scale, clipped * positive_scale)
    codes = np.rint(codes)
    codes = np.clip(codes, low, high).astype(np.int64)
    normalized = codes.astype(np.float64) / float(1 << (bits_per_sample - 1))
    flat = codes.reshape(-1)
    if bits_per_sample == 16:
        raw = flat.astype("<i2").tobytes()
    else:
        unsigned = (flat & 0xFFFFFF).astype(np.uint32)
        octets = np.empty((unsigned.size, 3), dtype=np.uint8)
        octets[:, 0] = unsigned & 0xFF
        octets[:, 1] = (unsigned >> 8) & 0xFF
        octets[:, 2] = (unsigned >> 16) & 0xFF
        raw = octets.tobytes()
    return raw, normalized


def encode_wav(samples: np.ndarray, sample_rate: int, bits_per_sample: int) -> tuple[bytes, np.ndarray]:
    if samples.ndim != 2:
        raise ValueError("samples must be a 2-D (frames, channels) array")
    if sample_rate <= 0 or samples.shape[1] <= 0:
        raise ValueError("sample rate and channel count must be positive")
    if bits_per_sample not in (16, 24, 32):
        raise ValueError(f"unsupported output bit depth: {bits_per_sample}")
    frames, channels = samples.shape
    samples64 = np.ascontiguousarray(samples, dtype=np.float64)

    if bits_per_sample == 32:
        data = np.ascontiguousarray(samples64, dtype="<f4").tobytes()
        represented = np.frombuffer(data, dtype="<f4").astype(np.float64).reshape(frames, channels)
        format_tag = _IEEE_FLOAT
        fact_chunk = b"fact" + struct.pack("<II", 4, frames)
    else:
        data, represented = _pack_integer_pcm(samples64, bits_per_sample)
        represented = represented.reshape(frames, channels)
        format_tag = _PCM
        fact_chunk = b""

    bytes_per_sample = bits_per_sample // 8
    block_align = channels * bytes_per_sample
    data_len = len(data)
    fmt_payload = struct.pack(
        "<HHIIHH",
        format_tag,
        channels,
        sample_rate,
        sample_rate * block_align,
        block_align,
        bits_per_sample,
    )
    fmt_chunk = b"fmt " + struct.pack("<I", len(fmt_payload)) + fmt_payload
    padding = b"\x00" if data_len & 1 else b""
    riff_payload_size = 4 + len(fmt_chunk) + len(fact_chunk) + 8 + data_len + len(padding)
    header = (
        b"RIFF"
        + struct.pack("<I", riff_payload_size)
        + b"WAVE"
        + fmt_chunk
        + fact_chunk
        + b"data"
        + struct.pack("<I", data_len)
    )
    return header + data + padding, represented


def write_wav_atomic(
    path: str | Path,
    samples: np.ndarray,
    sample_rate: int,
    bits_per_sample: int,
) -> np.ndarray:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    payload, represented = encode_wav(samples, sample_rate, bits_per_sample)
    temporary: str | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="wb", dir=destination.parent, prefix=f".{destination.name}.", delete=False
        ) as handle:
            temporary = handle.name
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
        temporary = None
    finally:
        if temporary is not None:
            try:
                os.unlink(temporary)
            except FileNotFoundError:
                pass
    return represented


def write_bytes_atomic(path: str | Path, payload: bytes) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary: str | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="wb", dir=destination.parent, prefix=f".{destination.name}.", delete=False
        ) as handle:
            temporary = handle.name
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
        temporary = None
    finally:
        if temporary is not None:
            try:
                os.unlink(temporary)
            except FileNotFoundError:
                pass


def write_json_atomic(path: str | Path, value: Any) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False, allow_nan=False) + "\n"
    temporary: str | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            newline="\n",
            dir=destination.parent,
            prefix=f".{destination.name}.",
            delete=False,
        ) as handle:
            temporary = handle.name
            handle.write(text)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
        temporary = None
    finally:
        if temporary is not None:
            try:
                os.unlink(temporary)
            except FileNotFoundError:
                pass
