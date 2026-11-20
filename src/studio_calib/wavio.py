from __future__ import annotations

import wave
from dataclasses import dataclass

import numpy as np

from .errors import CalibrationError


CHANNEL_LAYOUTS: dict[int, tuple[str, ...]] = {
    1: ("M",),
    2: ("L", "R"),
    4: ("FL", "FR", "SL", "SR"),
    6: ("FL", "FR", "FC", "LFE", "SL", "SR"),
}

LAYOUT_CHANNELS: dict[str, tuple[str, ...]] = {
    "mono": ("M",),
    "stereo": ("L", "R"),
    "5.1": ("FL", "FR", "FC", "LFE", "SL", "SR"),
}


def layout_channel_names(layout: str) -> tuple[str, ...]:
    return LAYOUT_CHANNELS[layout]


@dataclass
class Measurement:
    sample_rate: int
    channels: dict[str, np.ndarray]
    quantization_step: float
    layout: str


def read_pcm16_wav(path: str) -> Measurement:
    """Read a 16-bit PCM WAV and close the file before returning."""
    try:
        with wave.open(path, "rb") as handle:
            channels = handle.getnchannels()
            sample_width = handle.getsampwidth()
            sample_rate = handle.getframerate()
            compression = handle.getcomptype()
            frame_count = handle.getnframes()
            raw = handle.readframes(frame_count)
    except (OSError, wave.Error) as exc:
        raise CalibrationError("E_WAV_INVALID", f"cannot read WAV: {exc}") from exc

    if compression != "NONE" or sample_width != 2:
        raise CalibrationError(
            "E_WAV_INVALID", "only uncompressed 16-bit PCM WAV is supported"
        )
    if sample_rate != 48000:
        raise CalibrationError(
            "E_WAV_INVALID", f"expected 48000 Hz, got {sample_rate} Hz"
        )
    if channels not in CHANNEL_LAYOUTS:
        raise CalibrationError(
            "E_WAV_INVALID", f"unsupported channel count: {channels}"
        )
    frame_bytes = channels * sample_width
    usable = len(raw) - (len(raw) % frame_bytes)
    if usable == 0:
        raise CalibrationError("E_WAV_INVALID", "WAV contains no complete samples")

    interleaved = np.frombuffer(raw[:usable], dtype="<i2")
    matrix = interleaved.reshape(-1, channels)
    names = CHANNEL_LAYOUTS[channels]
    scale = 1.0 / 32768.0
    data = {
        name: matrix[:, index].astype(np.float64) * scale
        for index, name in enumerate(names)
    }
    layout = "mono" if channels == 1 else ("stereo" if channels == 2 else "5.1")
    return Measurement(
        sample_rate=sample_rate,
        channels=data,
        quantization_step=scale,
        layout=layout,
    )


def write_pcm16_wav(path: str, matrix: np.ndarray, sample_rate: int = 48000) -> None:
    """Write float samples in [-1, 1) as deterministic little-endian PCM16."""
    data = np.asarray(matrix, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    if not np.all(np.isfinite(data)):
        raise CalibrationError("E_WAV_INVALID", "cannot write non-finite samples")
    integer = np.clip(np.rint(data * 32768.0), -32768, 32767).astype("<i2")
    with wave.open(path, "wb") as handle:
        handle.setnchannels(data.shape[1])
        handle.setsampwidth(2)
        handle.setframerate(sample_rate)
        handle.setcomptype("NONE", "not compressed")
        handle.writeframes(integer.tobytes(order="C"))

