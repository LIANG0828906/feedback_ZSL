from __future__ import annotations

import wave
from dataclasses import dataclass
from pathlib import Path

import numpy as np


@dataclass(frozen=True)
class WavInfo:
    sample_rate: int
    channels: int
    bit_depth: int
    frames: int


@dataclass(frozen=True)
class WavData:
    samples: np.ndarray
    info: WavInfo


def read_wav(path: Path) -> WavData:
    with wave.open(str(path), "rb") as handle:
        channels = handle.getnchannels()
        sample_rate = handle.getframerate()
        sample_width = handle.getsampwidth()
        frames = handle.getnframes()
        compression = handle.getcomptype()
        raw = handle.readframes(frames)

    if compression != "NONE":
        raise ValueError(f"compressed WAV is unsupported: {compression}")
    if sample_width not in (2, 3, 4):
        raise ValueError(f"unsupported PCM sample width: {sample_width * 8} bit")

    if sample_width == 2:
        values = np.frombuffer(raw, dtype="<i2").astype(np.float64) / 32768.0
    elif sample_width == 3:
        octets = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3).astype(np.int32)
        integers = octets[:, 0] | (octets[:, 1] << 8) | (octets[:, 2] << 16)
        integers = np.where(integers & 0x800000, integers - 0x1000000, integers)
        values = integers.astype(np.float64) / 8388608.0
    else:
        values = np.frombuffer(raw, dtype="<i4").astype(np.float64) / 2147483648.0

    expected = frames * channels
    if values.size != expected:
        raise ValueError(f"truncated WAV: expected {expected} PCM values, got {values.size}")
    samples = values.reshape(frames, channels).copy()
    return WavData(
        samples=samples,
        info=WavInfo(sample_rate, channels, sample_width * 8, frames),
    )


def wav_bytes(samples: np.ndarray, sample_rate: int, bit_depth: int) -> bytes:
    if samples.ndim != 2:
        raise ValueError("samples must have shape (frames, channels)")
    if bit_depth not in (16, 24, 32):
        raise ValueError(f"unsupported output bit depth: {bit_depth}")

    channels = samples.shape[1]
    scale = float(1 << (bit_depth - 1))
    clipped = np.clip(samples, -1.0, 1.0)
    integers = np.rint(clipped * scale).astype(np.int64)
    integers = np.clip(integers, -(1 << (bit_depth - 1)), (1 << (bit_depth - 1)) - 1)

    if bit_depth == 16:
        raw = integers.astype("<i2").tobytes(order="C")
    elif bit_depth == 24:
        low = integers.astype("<i4").view(np.uint8).reshape(-1, 4)
        raw = np.ascontiguousarray(low[:, :3]).tobytes(order="C")
    else:
        raw = integers.astype("<i4").tobytes(order="C")

    import io

    buffer = io.BytesIO()
    with wave.open(buffer, "wb") as handle:
        handle.setnchannels(channels)
        handle.setsampwidth(bit_depth // 8)
        handle.setframerate(sample_rate)
        handle.setcomptype("NONE", "NONE")
        handle.writeframes(raw)
    return buffer.getvalue()


def write_wav(path: Path, samples: np.ndarray, sample_rate: int, bit_depth: int) -> None:
    from .util import atomic_write_bytes

    atomic_write_bytes(path, wav_bytes(samples, sample_rate, bit_depth))
