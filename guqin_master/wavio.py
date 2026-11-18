from __future__ import annotations

import hashlib
import os
import wave
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import BusinessError, invalid_project


@dataclass(frozen=True)
class WavInfo:
    sample_rate: int
    bit_depth: int
    channels: int
    frames: int
    sha256: str


def _decode_pcm(raw: bytes, sample_width: int) -> np.ndarray:
    if sample_width == 2:
        values = np.frombuffer(raw, dtype="<i2").astype(np.float64)
        return values / 32768.0
    if sample_width == 3:
        data = np.frombuffer(raw, dtype=np.uint8)
        if data.size % 3:
            raise ValueError("truncated 24-bit PCM payload")
        triples = data.reshape(-1, 3).astype(np.int32)
        values = triples[:, 0] | (triples[:, 1] << 8) | (triples[:, 2] << 16)
        values = np.where(values & 0x800000, values - 0x1000000, values)
        return values.astype(np.float64) / 8388608.0
    if sample_width == 4:
        values = np.frombuffer(raw, dtype="<i4").astype(np.float64)
        return values / 2147483648.0
    raise ValueError(f"unsupported PCM sample width: {sample_width} bytes")


def read_wav_mono(path: Path, channel_name: str) -> tuple[np.ndarray, WavInfo]:
    try:
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        with wave.open(str(path), "rb") as handle:
            if handle.getcomptype() != "NONE":
                raise invalid_project(channel_name, "compressed WAV is not supported")
            channels = handle.getnchannels()
            sample_width = handle.getsampwidth()
            sample_rate = handle.getframerate()
            frames = handle.getnframes()
            raw = handle.readframes(frames)
    except BusinessError:
        raise
    except FileNotFoundError as exc:
        raise invalid_project(channel_name, f"channel WAV not found: {path.name}") from exc
    except (OSError, wave.Error, EOFError) as exc:
        raise invalid_project(channel_name, f"cannot read channel WAV: {exc}") from exc

    if sample_width not in (2, 3, 4):
        raise invalid_project(
            channel_name,
            f"unsupported PCM bit depth: {sample_width * 8}",
            actual_bit_depth=sample_width * 8,
        )
    try:
        samples = _decode_pcm(raw, sample_width)
    except ValueError as exc:
        raise invalid_project(channel_name, str(exc)) from exc
    if channels != 1:
        samples = samples.reshape(-1, channels)
    info = WavInfo(
        sample_rate=sample_rate,
        bit_depth=sample_width * 8,
        channels=channels,
        frames=frames,
        sha256=digest,
    )
    return samples, info


def _encode_pcm(samples: np.ndarray, bit_depth: int) -> bytes:
    if bit_depth == 16:
        scale = 32768.0
        max_value = 32767
        minimum = -32768
        dtype = "<i2"
    elif bit_depth == 24:
        scale = 8388608.0
        max_value = 8388607
        minimum = -8388608
        dtype = None
    elif bit_depth == 32:
        scale = 2147483648.0
        max_value = 2147483647
        minimum = -2147483648
        dtype = "<i4"
    else:
        raise ValueError(f"unsupported PCM bit depth: {bit_depth}")

    clipped = np.clip(np.asarray(samples, dtype=np.float64), -1.0, 1.0)
    values = np.rint(clipped * scale).astype(np.int64)
    values = np.clip(values, minimum, max_value)
    if bit_depth == 24:
        unsigned = values.astype(np.uint32) & 0xFFFFFF
        packed = np.empty((unsigned.size, 3), dtype=np.uint8)
        packed[:, 0] = unsigned & 0xFF
        packed[:, 1] = (unsigned >> 8) & 0xFF
        packed[:, 2] = (unsigned >> 16) & 0xFF
        return packed.tobytes()
    return values.astype(dtype).tobytes()


def write_wav(path: Path, samples: np.ndarray, sample_rate: int, bit_depth: int) -> None:
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    if data.ndim != 2:
        raise ValueError("samples must be a 1-D or 2-D array")
    channels = int(data.shape[1])
    if channels not in (1, 2):
        raise ValueError("only mono and stereo WAV output are supported")
    sample_width = bit_depth // 8
    payload = _encode_pcm(data.reshape(-1), bit_depth)

    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    with wave.open(str(temporary), "wb") as handle:
        handle.setnchannels(channels)
        handle.setsampwidth(sample_width)
        handle.setframerate(int(sample_rate))
        handle.writeframes(payload)
    os.replace(temporary, path)


def wav_sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()
