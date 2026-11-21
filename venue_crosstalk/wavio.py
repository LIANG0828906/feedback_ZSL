"""Minimal deterministic 16-bit PCM WAV reader/writer for mono and 5.1 layouts."""

from __future__ import annotations

import wave
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import E_MEASUREMENT_INVALID, VenueError

LAYOUT_CHANNELS = {
    "mono": 1,
    "stereo": 2,
    "5.1": 6,
}

LAYOUT_NAMES = {
    "5.1": ("FL", "FR", "FC", "LFE", "SL", "SR"),
}


@dataclass(frozen=True)
class WavInfo:
    sample_rate: int
    channels: int
    frames: int
    sample_width: int
    layout: str | None


def _infer_layout(channels: int) -> str | None:
    for name, count in LAYOUT_CHANNELS.items():
        if count == channels:
            return name
    return None


def inspect_wav(path: str | Path, expected_rate: int | None = None) -> WavInfo:
    source = Path(path)
    try:
        with wave.open(str(source), "rb") as wav:
            info = WavInfo(
                sample_rate=wav.getframerate(),
                channels=wav.getnchannels(),
                frames=wav.getnframes(),
                sample_width=wav.getsampwidth(),
                layout=_infer_layout(wav.getnchannels()),
            )
    except (OSError, wave.Error) as exc:
        raise VenueError(
            E_MEASUREMENT_INVALID,
            f"cannot open WAV: {exc}",
            {"path": str(source)},
        ) from exc
    if info.sample_width != 2:
        raise VenueError(
            E_MEASUREMENT_INVALID,
            "only 16-bit PCM WAV is accepted",
            {"path": str(source), "sample_width": info.sample_width},
        )
    if expected_rate is not None and info.sample_rate != expected_rate:
        raise VenueError(
            E_MEASUREMENT_INVALID,
            "sample rate does not match manifest",
            {
                "path": str(source),
                "expected_sample_rate": expected_rate,
                "actual_sample_rate": info.sample_rate,
            },
        )
    return info


def read_wav_channel(
    path: str | Path,
    channel_index: int,
    *,
    expected_rate: int | None = None,
    expected_layout: str | None = None,
) -> np.ndarray:
    source = Path(path)
    info = inspect_wav(source, expected_rate=expected_rate)
    if expected_layout:
        expected_channels = LAYOUT_CHANNELS.get(expected_layout)
        if expected_channels is None:
            raise VenueError(
                E_MEASUREMENT_INVALID,
                "unknown WAV layout in manifest",
                {"path": str(source), "layout": expected_layout},
            )
        if info.channels != expected_channels:
            raise VenueError(
                E_MEASUREMENT_INVALID,
                "WAV channel count does not match declared layout",
                {
                    "path": str(source),
                    "layout": expected_layout,
                    "expected_channels": expected_channels,
                    "actual_channels": info.channels,
                },
            )
    if not (0 <= channel_index < info.channels):
        raise VenueError(
            E_MEASUREMENT_INVALID,
            "channel index is outside WAV layout",
            {
                "path": str(source),
                "channel_index": channel_index,
                "channels": info.channels,
            },
        )
    try:
        with wave.open(str(source), "rb") as wav:
            raw = wav.readframes(info.frames)
    except (OSError, wave.Error) as exc:
        raise VenueError(
            E_MEASUREMENT_INVALID,
            f"cannot read WAV frames: {exc}",
            {"path": str(source)},
        ) from exc
    interleaved = np.frombuffer(raw, dtype="<i2").reshape(-1, info.channels)
    return interleaved[:, channel_index].astype(np.float64) / 32768.0


def write_wav_channels(
    path: str | Path,
    channels: np.ndarray,
    sample_rate: int,
) -> None:
    """Write channels shaped (frames, channels) as 16-bit little-endian PCM."""

    data = np.asarray(channels, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    if data.ndim != 2:
        raise ValueError("channels must be one- or two-dimensional")
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    clipped = np.clip(data, -1.0, 32767.0 / 32768.0)
    quantized = np.rint(clipped * 32768.0).astype("<i2")
    try:
        with wave.open(str(target), "wb") as wav:
            wav.setnchannels(data.shape[1])
            wav.setsampwidth(2)
            wav.setframerate(sample_rate)
            wav.writeframes(quantized.tobytes(order="C"))
    except (OSError, wave.Error) as exc:
        raise VenueError(
            E_MEASUREMENT_INVALID,
            f"cannot write WAV: {exc}",
            {"path": str(target)},
        ) from exc
