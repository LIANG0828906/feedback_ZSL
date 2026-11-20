"""16-bit PCM WAV reader and deterministic local sample generator."""

from __future__ import annotations

import math
import wave
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import BizError, E_AUDIO_INVALID


@dataclass(frozen=True)
class AudioData:
    path: str
    sample_rate: int
    channels: int
    samples: np.ndarray
    duration_ms: int


def read_wav(path: Path) -> AudioData:
    try:
        with wave.open(str(path), "rb") as handle:
            channels = handle.getnchannels()
            sample_width = handle.getsampwidth()
            sample_rate = handle.getframerate()
            frames = handle.getnframes()
            compression = handle.getcomptype()
            raw = handle.readframes(frames)
    except (OSError, wave.Error) as exc:
        raise BizError(
            E_AUDIO_INVALID,
            "cannot read local WAV",
            {"file": str(path), "reason": str(exc)},
        ) from exc

    if compression != "NONE":
        raise BizError(E_AUDIO_INVALID, "compressed WAV is not supported", {"file": str(path), "field": "comptype"})
    if sample_width != 2:
        raise BizError(
            E_AUDIO_INVALID,
            "only 16-bit PCM WAV is supported",
            {"file": str(path), "field": "sampwidth", "value": sample_width},
        )
    if channels not in (1, 2):
        raise BizError(
            E_AUDIO_INVALID,
            "only mono or stereo WAV is supported",
            {"file": str(path), "field": "nchannels", "value": channels},
        )
    if not 8000 <= sample_rate <= 48000:
        raise BizError(
            E_AUDIO_INVALID,
            "sample rate must be within 8000..48000 Hz",
            {"file": str(path), "field": "framerate", "value": sample_rate},
        )
    if frames <= 0:
        raise BizError(E_AUDIO_INVALID, "WAV contains no samples", {"file": str(path), "field": "nframes"})

    samples = np.frombuffer(raw, dtype="<i2")
    expected = frames * channels
    if samples.size != expected:
        raise BizError(
            E_AUDIO_INVALID,
            "PCM payload length does not match WAV header",
            {"file": str(path), "field": "data", "expected_samples": expected, "actual_samples": int(samples.size)},
        )
    samples = samples.reshape(frames, channels)
    duration_ms = int(round(frames * 1000 / sample_rate))
    return AudioData(str(path), sample_rate, channels, samples, duration_ms)


def merge_to_mono(audio: AudioData, strategy: str) -> np.ndarray:
    if strategy not in {"mono", "mean", "left", "right", "sum_clip"}:
        raise BizError(
            E_AUDIO_INVALID,
            "invalid channel merge strategy",
            {"file": audio.path, "field": "channel_merge", "value": strategy},
        )
    if audio.channels == 1:
        if strategy not in {"mono", "mean", "left", "right", "sum_clip"}:
            raise BizError(E_AUDIO_INVALID, "invalid mono strategy", {"field": "channel_merge"})
        return audio.samples[:, 0].astype(np.float64) / 32768.0
    if strategy == "mono":
        raise BizError(
            E_AUDIO_INVALID,
            "stereo input requires an explicit merge strategy",
            {"file": audio.path, "field": "channel_merge", "value": strategy},
        )
    left = audio.samples[:, 0].astype(np.float64)
    right = audio.samples[:, 1].astype(np.float64)
    if strategy == "left":
        merged = left
    elif strategy == "right":
        merged = right
    elif strategy == "mean":
        merged = (left + right) / 2.0
    else:
        merged = np.clip(left + right, -32768.0, 32767.0)
    return merged / 32768.0


def _write_pcm16(path: Path, samples: np.ndarray, sample_rate: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    clipped = np.clip(samples, -1.0, 1.0)
    pcm = np.round(clipped * 32767.0).astype("<i2")
    channels = 1 if pcm.ndim == 1 else pcm.shape[1]
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(channels)
        handle.setsampwidth(2)
        handle.setframerate(sample_rate)
        handle.writeframes(pcm.tobytes(order="C"))


def _tone_track(seed: int, sample_rate: int, duration_s: float, stereo: bool) -> np.ndarray:
    rng = np.random.default_rng(seed)
    count = int(sample_rate * duration_s)
    t = np.arange(count, dtype=np.float64) / sample_rate
    signal = np.zeros(count, dtype=np.float64)
    base_notes = (196.0, 246.94, 293.66, 392.0, 493.88)
    for index, freq in enumerate(base_notes):
        start = index * duration_s / len(base_notes)
        stop = min(duration_s, start + 0.82)
        env = np.where((t >= start) & (t < stop), np.sin(np.pi * (t - start) / (stop - start)) ** 2, 0.0)
        vibrato = 1.0 + 0.004 * np.sin(2 * np.pi * 5.2 * t + index)
        signal += 0.21 * env * np.sin(2 * np.pi * freq * vibrato * t)
        signal += 0.08 * env * np.sin(2 * np.pi * freq * 2.01 * t + 0.2 * index)
    noise = rng.normal(0.0, 0.008, count)
    signal = np.tanh(1.35 * (signal + noise))
    if not stereo:
        return signal
    delayed = np.roll(signal, 37)
    right = 0.76 * signal + 0.24 * delayed
    right += 0.04 * np.sin(2 * np.pi * (base_notes[-1] * 1.5) * t)
    return np.stack([signal, np.clip(right, -1.0, 1.0)], axis=1)


def generate_sample_set(root: Path, seed: int = 20261121) -> list[dict[str, object]]:
    """Generate deterministic local fixtures with varied rates/channels."""
    root.mkdir(parents=True, exist_ok=True)
    specs = [
        ("track_alpha", 16000, 12.0, False),
        ("track_beta", 22050, 12.0, True),
        ("track_gamma", 8000, 10.0, False),
    ]
    rows: list[dict[str, object]] = []
    for index, (track_id, rate, duration, stereo) in enumerate(specs):
        path = root / f"{track_id}.wav"
        samples = _tone_track(seed + index * 101, rate, duration, stereo)
        _write_pcm16(path, samples, rate)
        rows.append(
            {
                "track_id": track_id,
                "path": str(path),
                "sample_rate": rate,
                "channels": 2 if stereo else 1,
                "duration_s": duration,
                "seed": seed + index * 101,
            }
        )
    return rows


def generate_single_sample(
    path: Path,
    *,
    seed: int = 7,
    sample_rate: int = 8000,
    duration_s: float = 4.0,
    stereo: bool = False,
) -> Path:
    if not 8000 <= sample_rate <= 48000:
        raise BizError(
            E_AUDIO_INVALID,
            "sample rate must be within 8000..48000 Hz",
            {"field": "sample_rate", "value": sample_rate},
        )
    _write_pcm16(path, _tone_track(seed, sample_rate, duration_s, stereo), sample_rate)
    return path
