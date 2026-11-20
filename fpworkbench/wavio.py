"""16-bit PCM WAV reading via the standard-library wave module only."""

from __future__ import annotations

import wave

import numpy as np

from .errors import E_FMT_INVALID, FpError

MIN_SAMPLE_RATE = 8000
MAX_SAMPLE_RATE = 48000


def read_wav(path: str):
    """Return (mono float32 signal in [-1, 1], sample_rate, channels).

    Stereo is down-mixed by averaging; the caller records the channel-merge
    flag. Only 16-bit PCM, 1-2 channels, 8000-48000 Hz are accepted.
    """
    try:
        with wave.open(path, "rb") as wf:
            channels = wf.getnchannels()
            sampwidth = wf.getsampwidth()
            sample_rate = wf.getframerate()
            n_frames = wf.getnframes()
            raw = wf.readframes(n_frames)
    except (wave.Error, OSError) as exc:
        raise FpError(E_FMT_INVALID, f"cannot read WAV: {exc}",
                      file=path, field="$")

    if sampwidth != 2:
        raise FpError(E_FMT_INVALID,
                      f"only 16-bit PCM supported, got sampwidth={sampwidth}",
                      file=path, field="sampwidth")
    if channels not in (1, 2):
        raise FpError(E_FMT_INVALID,
                      f"only mono/stereo supported, got {channels} channels",
                      file=path, field="channels")
    if not (MIN_SAMPLE_RATE <= sample_rate <= MAX_SAMPLE_RATE):
        raise FpError(E_FMT_INVALID,
                      f"sample rate {sample_rate} outside "
                      f"{MIN_SAMPLE_RATE}-{MAX_SAMPLE_RATE} Hz",
                      file=path, field="sample_rate")

    data = np.frombuffer(raw, dtype="<i2").astype(np.float32) / 32768.0
    if channels == 2:
        data = data.reshape(-1, 2).mean(axis=1)
    return data, sample_rate, channels


def write_wav(path: str, signal: np.ndarray, sample_rate: int, channels: int = 1):
    """Write a float signal in [-1, 1] as 16-bit PCM WAV (mono or stereo)."""
    sig = np.asarray(signal, dtype=np.float32)
    if channels == 2:
        if sig.ndim == 1:
            sig = np.stack([sig, sig], axis=1)
        interleaved = sig.reshape(-1)
    else:
        interleaved = sig
    pcm = np.clip(interleaved, -1.0, 1.0)
    pcm = (pcm * 32767.0).astype("<i2")
    with wave.open(path, "wb") as wf:
        wf.setnchannels(channels)
        wf.setsampwidth(2)
        wf.setframerate(sample_rate)
        wf.writeframes(pcm.tobytes())
