from __future__ import annotations

import hashlib
import io
import os
import struct
import tempfile
import wave
from dataclasses import dataclass
from pathlib import Path

import numpy as np


@dataclass(frozen=True)
class WavInfo:
    sample_rate: int
    channels: int
    bit_depth: int
    sample_format: str
    data_bytes: int
    frames: int


def _parse_wav_info(payload: bytes) -> tuple[WavInfo, bytes]:
    if len(payload) < 12 or payload[:4] != b"RIFF" or payload[8:12] != b"WAVE":
        raise ValueError("not a RIFF/WAVE file")

    offset = 12
    fmt: tuple[int, int, int, int, int, int] | None = None
    data: bytes | None = None
    while offset + 8 <= len(payload):
        chunk_id = payload[offset : offset + 4]
        chunk_size = struct.unpack_from("<I", payload, offset + 4)[0]
        body_start = offset + 8
        body_end = body_start + chunk_size
        if body_end > len(payload):
            raise ValueError("truncated WAV chunk")
        body = payload[body_start:body_end]
        if chunk_id == b"fmt ":
            if len(body) < 16:
                raise ValueError("short fmt chunk")
            audio_format, channels, sample_rate, _, block_align, bits = struct.unpack_from(
                "<HHIIHH", body, 0
            )
            if audio_format == 0xFFFE:
                if len(body) < 40:
                    raise ValueError("short WAVE_FORMAT_EXTENSIBLE fmt chunk")
                valid_bits = struct.unpack_from("<H", body, 18)[0]
                subformat_tag = struct.unpack_from("<H", body, 24)[0]
                audio_format = subformat_tag
                if valid_bits:
                    bits = valid_bits
            fmt = (audio_format, channels, sample_rate, block_align, bits, len(body))
        elif chunk_id == b"data":
            if data is None:
                data = body
        offset = body_end + (chunk_size & 1)

    if fmt is None:
        raise ValueError("missing fmt chunk")
    if data is None:
        raise ValueError("missing data chunk")

    audio_format, channels, sample_rate, block_align, bits, _ = fmt
    if audio_format == 1:
        sample_format = "pcm"
    elif audio_format == 3:
        sample_format = "float"
    else:
        raise ValueError(f"unsupported WAV format tag {audio_format}")
    if channels <= 0 or block_align <= 0:
        raise ValueError("invalid channel count or block alignment")
    if len(data) % block_align:
        raise ValueError("data chunk is not frame aligned")

    info = WavInfo(
        sample_rate=sample_rate,
        channels=channels,
        bit_depth=bits,
        sample_format=sample_format,
        data_bytes=len(data),
        frames=len(data) // block_align,
    )
    return info, data


def inspect_wav(path: Path) -> tuple[WavInfo, bytes]:
    return _parse_wav_info(path.read_bytes())


def decode_wav_data(info: WavInfo, data: bytes) -> np.ndarray:
    channels = info.channels
    bits = info.bit_depth
    if info.sample_format == "float":
        if bits == 32:
            values = np.frombuffer(data, dtype="<f4").astype(np.float64)
        elif bits == 64:
            values = np.frombuffer(data, dtype="<f8").astype(np.float64)
        else:
            raise ValueError(f"unsupported float bit depth {bits}")
    elif bits == 8:
        values = (np.frombuffer(data, dtype=np.uint8).astype(np.float64) - 128.0) / 128.0
    elif bits == 16:
        values = np.frombuffer(data, dtype="<i2").astype(np.float64) / 32768.0
    elif bits == 24:
        raw = np.frombuffer(data, dtype=np.uint8)
        if raw.size % 3:
            raise ValueError("24-bit data is not aligned")
        triples = raw.reshape(-1, 3).astype(np.int32)
        packed = triples[:, 0] | (triples[:, 1] << 8) | (triples[:, 2] << 16)
        signed = (packed ^ 0x800000) - 0x800000
        values = signed.astype(np.float64) / 8388608.0
    elif bits == 32:
        values = np.frombuffer(data, dtype="<i4").astype(np.float64) / 2147483648.0
    else:
        raise ValueError(f"unsupported PCM bit depth {bits}")
    return values.reshape(-1, channels)


def read_wav_mono(path: Path) -> tuple[WavInfo, np.ndarray, str]:
    payload = path.read_bytes()
    digest = hashlib.sha256(payload).hexdigest()
    info, data = _parse_wav_info(payload)
    values = decode_wav_data(info, data)
    mono = values[:, 0] if values.shape[1] == 1 else values.mean(axis=1)
    return info, mono, digest


def quantize_pcm(samples: np.ndarray, bit_depth: int) -> tuple[np.ndarray, np.ndarray]:
    """Return fixed-point integers and their decoded float representation."""
    values = np.asarray(samples, dtype=np.float64)
    if bit_depth == 16:
        peak = 32767.0
        ints = np.clip(np.rint(values * peak), -32768, 32767).astype("<i2")
        decoded = ints.astype(np.float64) / 32768.0
    elif bit_depth == 24:
        peak = 8388607.0
        ints = np.clip(np.rint(values * peak), -8388608, 8388607).astype("<i4")
        decoded = ints.astype(np.float64) / 8388608.0
    elif bit_depth == 32:
        peak = 2147483647.0
        ints = np.clip(np.rint(values * peak), -2147483648, 2147483647).astype("<i4")
        decoded = ints.astype(np.float64) / 2147483648.0
    else:
        raise ValueError(f"unsupported PCM bit depth {bit_depth}")
    return ints, decoded


def _pcm_bytes(ints: np.ndarray, bit_depth: int) -> bytes:
    if bit_depth == 16:
        return ints.astype("<i2", copy=False).tobytes(order="C")
    if bit_depth == 24:
        raw = ints.astype("<i4", copy=False).view(np.uint8).reshape(-1, 4)
        return raw[:, :3].tobytes(order="C")
    if bit_depth == 32:
        return ints.astype("<i4", copy=False).tobytes(order="C")
    raise ValueError(f"unsupported PCM bit depth {bit_depth}")


def encode_wav_bytes(samples: np.ndarray, sample_rate: int, bit_depth: int) -> tuple[bytes, np.ndarray]:
    values = np.asarray(samples, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    if values.ndim != 2:
        raise ValueError("samples must be mono or multi-channel")
    ints, decoded = quantize_pcm(values, bit_depth)
    frames = _pcm_bytes(ints, bit_depth)
    buffer = io.BytesIO()
    with wave.open(buffer, "wb") as wav:
        wav.setnchannels(values.shape[1])
        wav.setsampwidth(bit_depth // 8)
        wav.setframerate(sample_rate)
        wav.writeframes(frames)
    return buffer.getvalue(), decoded


def atomic_write_bytes(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, path)
        try:
            directory_fd = os.open(path.parent, os.O_RDONLY)
        except OSError:
            return
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        if os.path.exists(temp_name):
            os.unlink(temp_name)


def write_wav(path: Path, samples: np.ndarray, sample_rate: int, bit_depth: int) -> tuple[str, np.ndarray]:
    payload, decoded = encode_wav_bytes(samples, sample_rate, bit_depth)
    atomic_write_bytes(path, payload)
    return hashlib.sha256(payload).hexdigest(), decoded
