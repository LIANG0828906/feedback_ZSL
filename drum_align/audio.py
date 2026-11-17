"""Deterministic 48 kHz / 16-bit PCM WAV I/O."""

from __future__ import annotations

import wave
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import BusinessError
from .jsonio import sha256_file


@dataclass
class AudioTrack:
    camera: str
    path: Path
    samples: np.ndarray
    sample_rate: int
    channels: int
    frame_count: int
    sha256: str

    def mono(self) -> np.ndarray:
        data = self.samples.astype(np.float64) / 32768.0
        if self.channels == 1:
            return data[:, 0]
        return data.mean(axis=1)


def read_wav(path: Path, camera: str, expected_rate: int | None) -> AudioTrack:
    if not path.exists():
        raise BusinessError(
            "E_TRACK_MISSING",
            f"file not found: {path.name}",
            camera=camera,
        )
    try:
        with wave.open(str(path), "rb") as wav:
            channels = wav.getnchannels()
            sample_width = wav.getsampwidth()
            frame_rate = wav.getframerate()
            frame_count = wav.getnframes()
            compression = wav.getcomptype()
            raw = wav.readframes(frame_count)
    except (OSError, EOFError, wave.Error) as exc:
        raise BusinessError(
            "E_WAV_INVALID",
            f"cannot read WAV ({type(exc).__name__})",
            camera=camera,
        ) from exc

    if channels not in (1, 2):
        raise BusinessError(
            "E_CHANNELS",
            f"unsupported channel count {channels}; expected mono or stereo",
            camera=camera,
        )
    if sample_width != 2 or compression != "NONE":
        raise BusinessError(
            "E_WAV_FORMAT",
            f"expected uncompressed 16-bit PCM, got width={sample_width}, "
            f"compression={compression}",
            camera=camera,
        )
    if expected_rate is not None and frame_rate != expected_rate:
        raise BusinessError(
            "E_SAMPLE_RATE",
            f"WAV sample rate {frame_rate} differs from project rate {expected_rate}",
            camera=camera,
        )
    if frame_rate != 48_000:
        raise BusinessError(
            "E_SAMPLE_RATE",
            f"WAV sample rate must be 48000, got {frame_rate}",
            camera=camera,
        )
    expected_bytes = frame_count * channels * 2
    if len(raw) < expected_bytes:
        raise BusinessError(
            "E_WAV_INVALID",
            f"truncated PCM payload: expected {expected_bytes} bytes, got {len(raw)}",
            camera=camera,
        )
    if len(raw) > expected_bytes:
        raw = raw[:expected_bytes]
    if frame_count == 0:
        raise BusinessError("E_TRACK_SHORT", "WAV contains zero frames", camera=camera)
    samples = np.frombuffer(raw, dtype="<i2").reshape(frame_count, channels).copy()
    return AudioTrack(
        camera=camera,
        path=path,
        samples=samples,
        sample_rate=frame_rate,
        channels=channels,
        frame_count=frame_count,
        sha256=sha256_file(path),
    )


def write_pcm_chunk(handle, samples: np.ndarray) -> None:
    pcm = np.asarray(samples, dtype="<i2", order="C")
    handle.write(pcm.tobytes(order="C"))


def pcm_bytes(samples: np.ndarray) -> bytes:
    return np.asarray(samples, dtype="<i2", order="C").tobytes(order="C")
