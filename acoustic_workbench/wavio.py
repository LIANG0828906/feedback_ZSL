from __future__ import annotations

import os
import tempfile
import wave
import io
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import E_WAV_INVALID, fail


@dataclass(frozen=True)
class WavData:
    sample_rate: int
    channels: int
    samples: np.ndarray
    path: str

    @property
    def frames(self) -> int:
        return int(self.samples.shape[1])


def read_wav(path: str | Path, *, expected_channels: int | None = None) -> WavData:
    source = Path(path)
    try:
        with wave.open(str(source), "rb") as reader:
            channels = reader.getnchannels()
            sample_rate = reader.getframerate()
            sample_width = reader.getsampwidth()
            frames = reader.getnframes()
            raw = reader.readframes(frames)
    except (wave.Error, OSError, EOFError) as exc:
        fail(E_WAV_INVALID, f"cannot read WAV: {source.name}", error=str(exc))

    if sample_width != 2:
        fail(
            E_WAV_INVALID,
            "only 16-bit PCM WAV is supported",
            path=source.name,
            sample_width=sample_width,
        )
    if expected_channels is not None and channels != expected_channels:
        fail(
            E_WAV_INVALID,
            f"expected {expected_channels} channels, found {channels}",
            path=source.name,
            channels=channels,
        )
    if frames <= 0:
        fail(E_WAV_INVALID, "WAV contains no audio frames", path=source.name)

    interleaved = np.frombuffer(raw, dtype="<i2")
    if interleaved.size != frames * channels:
        fail(E_WAV_INVALID, "WAV payload length does not match header", path=source.name)
    matrix = interleaved.reshape(frames, channels).T.astype(np.float64) / 32768.0
    return WavData(
        sample_rate=sample_rate,
        channels=channels,
        samples=np.ascontiguousarray(matrix),
        path=str(source),
    )


def pcm16_bytes(samples: np.ndarray) -> bytes:
    """Convert channel-first float samples to deterministic interleaved PCM16."""

    if samples.ndim == 1:
        samples = samples[np.newaxis, :]
    if samples.ndim != 2 or samples.shape[0] < 1:
        fail(E_WAV_INVALID, "audio samples must have shape [channels, frames]")
    if not np.all(np.isfinite(samples)):
        fail(E_WAV_INVALID, "audio samples contain NaN or infinity")
    pcm = np.rint(np.clip(samples, -1.0, 1.0) * 32767.0).astype("<i2")
    return np.ascontiguousarray(pcm.T).tobytes(order="C")


def write_wav_atomic(
    path: str | Path,
    samples: np.ndarray,
    sample_rate: int = 48000,
) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    payload = pcm16_bytes(samples)
    channels = 1 if samples.ndim == 1 else int(samples.shape[0])
    temporary: str | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="wb", dir=destination.parent, prefix=".wav-", suffix=".tmp", delete=False
        ) as raw_handle:
            temporary = raw_handle.name
            with wave.open(raw_handle, "wb") as writer:
                writer.setnchannels(channels)
                writer.setsampwidth(2)
                writer.setframerate(sample_rate)
                writer.writeframes(payload)
            raw_handle.flush()
            os.fsync(raw_handle.fileno())
        os.replace(temporary, destination)
        temporary = None
    finally:
        if temporary is not None:
            try:
                os.unlink(temporary)
            except FileNotFoundError:
                pass


def encode_wav_bytes(samples: np.ndarray, sample_rate: int = 48000) -> bytes:
    buffer = io.BytesIO()
    channels = 1 if samples.ndim == 1 else int(samples.shape[0])
    with wave.open(buffer, "wb") as writer:
        writer.setnchannels(channels)
        writer.setsampwidth(2)
        writer.setframerate(sample_rate)
        writer.writeframes(pcm16_bytes(samples))
    return buffer.getvalue()

